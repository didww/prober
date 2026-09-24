// Package dns is the name-lookup engine: it resolves a name with the host's
// own resolver — its A and AAAA records and the SIP service SRV records — and
// reports each answer as it arrives.
//
// It deliberately asks the host resolver (net.DefaultResolver: resolv.conf and
// the hosts file) rather than running a DNS client of its own. The question
// the tool answers is "what does this site resolve the name to", which is
// exactly what the trace and SIP engines see when they resolve a target, so
// GeoDNS, split-horizon views and a broken resolver at a PoP all show up as
// they would to a probe.
package dns

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"
)

// Spec is one lookup as the engine runs it.
type Spec struct {
	// Name is the host name to resolve, e.g. "sip.example.com". It is
	// lower-cased and stripped of a trailing dot; an address literal is
	// refused, since there is nothing to look up.
	Name string
	// Timeout bounds each query. 0 means DefaultTimeout.
	Timeout time.Duration
}

const DefaultTimeout = 5 * time.Second

// Query is one question: a record type and the owner name it is asked for.
type Query struct {
	Type string // "A", "AAAA" or "SRV"
	Name string // "_sip._udp.example.com" for SRV; the bare name otherwise
}

// Record is one answer. Value is the address for A and AAAA and the target
// host for SRV; the rest is SRV only.
type Record struct {
	Value    string
	Priority uint16
	Weight   uint16
	Port     uint16
}

// Result is one query's outcome. Err is nil when the query was answered, even
// with no records: NXDOMAIN and NODATA are answers, not failures.
type Result struct {
	Query   Query
	Records []Record
	Err     error
	RTT     time.Duration
}

// Resolver is what the engine asks. *net.Resolver satisfies it; tests supply
// a fake.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// srvServices are the SIP SRV names looked up, in the order they are shown.
// _sip._tls is not in RFC 3263 (TLS is _sips._tcp there) but is widely
// published, so it is asked for alongside.
var srvServices = [...]struct{ service, proto string }{
	{"sip", "udp"}, {"sip", "tcp"}, {"sip", "tls"}, {"sips", "tcp"},
}

// Queries returns the questions asked for a name, in a fixed order: A, AAAA,
// then the SIP SRV records.
func Queries(name string) []Query {
	qs := []Query{{Type: "A", Name: name}, {Type: "AAAA", Name: name}}
	for _, s := range srvServices {
		qs = append(qs, Query{Type: "SRV", Name: "_" + s.service + "._" + s.proto + "." + name})
	}
	return qs
}

func (s Spec) normalize() (Spec, error) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.Name), "."))
	if name == "" {
		return s, errors.New("dns: name is required")
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return s, fmt.Errorf("dns: %q is an address, not a name", name)
	}
	if !validName(name) {
		return s, fmt.Errorf("dns: %q is not a valid host name", name)
	}
	s.Name = name
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	return s, nil
}

// validName mirrors the resolver's own domain-name rules: dot-separated labels
// of 1–63 letters, digits, hyphens or underscores, 253 characters in all. An
// underscore is allowed because service names like _sip._udp carry them.
func validName(name string) bool {
	if len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
			default:
				return false
			}
		}
	}
	return true
}

// --- events -----------------------------------------------------------------

type EventKind uint8

const (
	Started EventKind = iota + 1
	ResultDone
	Finished
)

type Event struct {
	Kind        EventKind
	Time        time.Time
	Spec        *Spec    // Started
	Nameservers []string // Started: the host's resolver addresses, if known
	Result      *Result  // ResultDone
	Cancelled   bool     // Finished
}

// Run asks every query in parallel and calls emit for each event, in order,
// from a single goroutine: Started, one ResultDone per query as it completes,
// then Finished. A cancelled ctx cuts the pending queries short; they are
// still reported, as errors, before Finished. A nil resolver means the host's.
func Run(ctx context.Context, spec Spec, r Resolver, emit func(Event)) error {
	spec, err := spec.normalize()
	if err != nil {
		return err
	}
	if r == nil {
		r = net.DefaultResolver
	}
	emit(Event{Kind: Started, Time: time.Now(), Spec: &spec, Nameservers: Nameservers()})

	qs := Queries(spec.Name)
	results := make(chan Result, len(qs))
	for _, q := range qs {
		go func() { results <- lookup(ctx, r, q, spec.Timeout) }()
	}
	for range qs {
		res := <-results
		emit(Event{Kind: ResultDone, Time: time.Now(), Result: &res})
	}
	emit(Event{Kind: Finished, Time: time.Now(), Cancelled: ctx.Err() != nil})
	return nil
}

// lookup asks one query, bounded by timeout, and normalises the answer: records
// sorted so two sites' answers compare line by line, "no such name" folded into
// an empty answer.
func lookup(ctx context.Context, r Resolver, q Query, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := Result{Query: q}
	start := time.Now()
	var err error
	switch q.Type {
	case "A", "AAAA":
		network, want4 := "ip4", true
		if q.Type == "AAAA" {
			network, want4 = "ip6", false
		}
		var ips []netip.Addr
		ips, err = r.LookupNetIP(ctx, network, q.Name)
		for _, ip := range ips {
			ip = ip.Unmap()
			if ip.Is4() != want4 {
				continue
			}
			res.Records = append(res.Records, Record{Value: ip.String()})
		}
		slices.SortFunc(res.Records, func(a, b Record) int {
			return netip.MustParseAddr(a.Value).Compare(netip.MustParseAddr(b.Value))
		})
	case "SRV":
		// An empty service and proto make the resolver ask for the name as
		// given, which already carries the _service._proto prefix.
		var srvs []*net.SRV
		_, srvs, err = r.LookupSRV(ctx, "", "", q.Name)
		for _, s := range srvs {
			target := s.Target
			if len(target) > 1 {
				target = strings.TrimSuffix(target, ".")
			}
			res.Records = append(res.Records, Record{Value: target, Priority: s.Priority, Weight: s.Weight, Port: s.Port})
		}
		// The resolver shuffles equal priorities by weight for load spreading;
		// a fixed order reads better and compares across sites.
		slices.SortFunc(res.Records, func(a, b Record) int {
			if a.Priority != b.Priority {
				return int(a.Priority) - int(b.Priority)
			}
			if a.Weight != b.Weight {
				return int(b.Weight) - int(a.Weight)
			}
			if a.Value != b.Value {
				return strings.Compare(a.Value, b.Value)
			}
			return int(a.Port) - int(b.Port)
		})
	default:
		err = fmt.Errorf("dns: unknown record type %q", q.Type)
	}
	res.RTT = time.Since(start)

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		err = nil
	}
	res.Err = err
	return res
}

// Nameservers returns the resolver addresses the host's resolv.conf names,
// which is where the pure-Go resolver sends its queries. Nil when the file
// cannot be read.
func Nameservers() []string {
	return nameserversFrom("/etc/resolv.conf")
}

func nameserversFrom(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			out = append(out, fields[1])
		}
	}
	return out
}
