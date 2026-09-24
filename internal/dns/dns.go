// Package dns is the name-lookup engine: it resolves a name through the
// host's own nameservers — its A and AAAA records and the SIP service SRV
// records, or the PTR record when given an address — and reports each answer
// as it arrives, with the response code the nameserver gave.
//
// It speaks DNS itself rather than going through the standard library
// resolver, because that resolver folds every negative answer into "no such
// host" and every server failure into "server misbehaving". A looking glass
// has to say NXDOMAIN, NODATA, SERVFAIL or REFUSED, and which server said it.
// The nameservers, timeout and attempts come from resolv.conf and the servers
// are tried in order, as the stub resolver does, so the answers are the
// site's resolver's. It is not the stub resolver, though: it does not read
// the hosts file, it does not apply the search list (a name is asked for
// exactly as given, as dig does), and it always sends EDNS0. The trace and
// SIP probes resolve through the standard library, so a name pinned in the
// hosts file or reached through the search list can differ between them and
// this tool.
package dns

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Spec is one lookup as the engine runs it.
type Spec struct {
	// Name is the host name to resolve, e.g. "sip.example.com", or an IPv4
	// or IPv6 address to reverse-resolve. A name is lower-cased, stripped of
	// a trailing dot, and asked for exactly as given: no search list is
	// applied. An address is canonicalised.
	Name string
	// Timeout bounds each question, over every nameserver and attempt. 0
	// means DefaultTimeout.
	Timeout time.Duration
}

const (
	// The per-attempt timeout and attempts per nameserver when resolv.conf
	// sets no options: the stub resolver's own defaults.
	defaultAttemptTimeout = 5 * time.Second
	defaultAttempts       = 2
	// DefaultTimeout is the whole question's budget: the stub resolver's
	// worst case of three servers, two attempts each, at the default timeout.
	DefaultTimeout = 30 * time.Second
	// The UDP payload size advertised with EDNS0, so that most answers fit
	// without a TCP retry. 1232 is the value that avoids fragmentation.
	udpPayload = 1232
)

// Query is one question: a record type and the owner name it is asked for.
type Query struct {
	Type string // "A", "AAAA", "SRV" or "PTR"
	Name string // "_sip._udp.example.com" for SRV, "4.3.2.1.in-addr.arpa" for PTR; the bare name otherwise
}

// Record is one answer. Value is the address for A and AAAA and the target
// host for SRV and PTR; the rest is SRV only.
type Record struct {
	Value    string
	Priority uint16
	Weight   uint16
	Port     uint16
	TTL      uint32
	// Addresses are the SRV target's A and AAAA records, looked up alongside
	// so a target that does not resolve shows beside its record. When there
	// are none, AddressStatus says why: the worse of the two lookups'
	// outcomes (NODATA, NXDOMAIN, SERVFAIL, TIMEOUT, ...).
	Addresses     []string
	AddressStatus string
}

// Result is one query's outcome. When a nameserver answered, Status names
// its response code and Server says which one; Err is set only when none did.
type Result struct {
	Query   Query
	Records []Record
	// Status is the response code by name: NOERROR, NXDOMAIN, SERVFAIL,
	// REFUSED and so on, or NODATA for a NOERROR answer that has no record
	// of the asked type. Empty when no nameserver answered.
	Status string
	// Server is the nameserver that answered, as host:port.
	Server string
	Err    error
	RTT    time.Duration
}

// Client asks the questions the way the host's stub resolver would: each
// nameserver in turn, a few attempts each, over UDP with a TCP retry when the
// answer is truncated. A nil Client means HostClient.
type Client struct {
	// Nameservers as host:port. Empty means the loopback defaults the stub
	// resolver falls back to when resolv.conf names none.
	Nameservers []string
	// Timeout is per attempt and Attempts is per nameserver; zero means
	// the resolv.conf option or its default.
	Timeout  time.Duration
	Attempts int
}

// HostClient builds a Client from /etc/resolv.conf: its nameservers and its
// timeout and attempts options.
func HostClient() *Client { return clientFrom("/etc/resolv.conf") }

func clientFrom(path string) *Client {
	c := &Client{}
	f, err := os.Open(path)
	if err != nil {
		return c
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			c.Nameservers = append(c.Nameservers, net.JoinHostPort(fields[1], "53"))
		case "options":
			for _, o := range fields[1:] {
				if v, ok := strings.CutPrefix(o, "timeout:"); ok {
					if n, err := strconv.Atoi(v); err == nil && n > 0 {
						c.Timeout = time.Duration(min(n, 30)) * time.Second
					}
				}
				if v, ok := strings.CutPrefix(o, "attempts:"); ok {
					if n, err := strconv.Atoi(v); err == nil && n > 0 {
						c.Attempts = min(n, 5)
					}
				}
			}
		}
	}
	return c
}

func (c *Client) normalized() Client {
	out := *c
	if len(out.Nameservers) == 0 {
		out.Nameservers = []string{"127.0.0.1:53", "[::1]:53"}
	}
	if out.Timeout <= 0 {
		out.Timeout = defaultAttemptTimeout
	}
	if out.Attempts <= 0 {
		out.Attempts = defaultAttempts
	}
	return out
}

// srvServices are the SIP SRV names looked up, in the order they are shown.
// _sip._tls is not in RFC 3263 (TLS is _sips._tcp there) but is widely
// published, so it is asked for alongside.
var srvServices = [...]struct{ service, proto string }{
	{"sip", "udp"}, {"sip", "tcp"}, {"sip", "tls"}, {"sips", "tcp"},
}

// Queries returns the questions asked for a name, in a fixed order: A, AAAA,
// then the SIP SRV records. For an address it is the one PTR question.
func Queries(name string) []Query {
	if addr, err := netip.ParseAddr(name); err == nil {
		return []Query{{Type: "PTR", Name: ReverseName(addr)}}
	}
	qs := []Query{{Type: "A", Name: name}, {Type: "AAAA", Name: name}}
	for _, s := range srvServices {
		qs = append(qs, Query{Type: "SRV", Name: "_" + s.service + "._" + s.proto + "." + name})
	}
	return qs
}

// ReverseName is the owner name of an address's PTR record: the octets
// reversed under in-addr.arpa for IPv4, the nibbles reversed under ip6.arpa
// for IPv6.
func ReverseName(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is4() {
		b := addr.As4()
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", b[3], b[2], b[1], b[0])
	}
	const hex = "0123456789abcdef"
	b := addr.As16()
	var sb strings.Builder
	for i := 15; i >= 0; i-- {
		sb.WriteByte(hex[b[i]&0xf])
		sb.WriteByte('.')
		sb.WriteByte(hex[b[i]>>4])
		sb.WriteByte('.')
	}
	sb.WriteString("ip6.arpa")
	return sb.String()
}

func (s Spec) normalize() (Spec, error) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.Name), "."))
	if name == "" {
		return s, errors.New("dns: name is required")
	}
	if addr, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
		s.Name = addr.Unmap().WithZone("").String()
	} else if validName(name) {
		s.Name = name
	} else {
		return s, fmt.Errorf("dns: %q is not a valid host name or address", name)
	}
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
	Nameservers []string // Started: the servers the questions go to
	Result      *Result  // ResultDone
	Cancelled   bool     // Finished
}

// Run asks every query in parallel and calls emit for each event, in order,
// from a single goroutine: Started, one ResultDone per query as it completes,
// then Finished. A cancelled ctx cuts the pending queries short; they are
// still reported, as errors, before Finished.
func Run(ctx context.Context, spec Spec, c *Client, emit func(Event)) error {
	spec, err := spec.normalize()
	if err != nil {
		return err
	}
	if c == nil {
		c = HostClient()
	}
	cl := c.normalized()
	emit(Event{Kind: Started, Time: time.Now(), Spec: &spec, Nameservers: cl.Nameservers})

	qs := Queries(spec.Name)
	targets := newTargetCache(&cl)
	results := make(chan Result, len(qs))
	for _, q := range qs {
		go func() {
			qctx, cancel := context.WithTimeout(ctx, spec.Timeout)
			defer cancel()
			results <- cl.lookup(qctx, q, targets)
		}()
	}
	for range qs {
		res := <-results
		emit(Event{Kind: ResultDone, Time: time.Now(), Result: &res})
	}
	emit(Event{Kind: Finished, Time: time.Now(), Cancelled: ctx.Err() != nil})
	return nil
}

// --- the client ---------------------------------------------------------------

// lookup answers one query: the question itself and, for SRV, the addresses
// of every target it names.
func (c *Client) lookup(ctx context.Context, q Query, targets *targetCache) Result {
	res := c.ask(ctx, q)
	if q.Type == "SRV" && len(res.Records) > 0 {
		targets.fill(ctx, res.Records)
	}
	return res
}

// targetCache resolves each SRV target's addresses once per run, however
// many of the SRV questions name it: the four SIP services usually point at
// the same hosts.
type targetCache struct {
	c  *Client
	mu sync.Mutex
	m  map[string]*targetAddrs
}

// targetAddrs is one target's A and AAAA answers, resolved on first use;
// later users of the same target wait for that.
type targetAddrs struct {
	once   sync.Once
	addrs  []string
	status string
}

func newTargetCache(c *Client) *targetCache {
	return &targetCache{c: c, m: map[string]*targetAddrs{}}
}

// fill sets every record's Addresses, or AddressStatus when it has none,
// resolving the targets in parallel. The root target "." means "service not
// available" and names nothing.
func (tc *targetCache) fill(ctx context.Context, recs []Record) {
	entries := make([]*targetAddrs, len(recs))
	var wg sync.WaitGroup
	for i, r := range recs {
		if r.Value == "." {
			continue
		}
		e := tc.entry(r.Value)
		entries[i] = e
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.resolve(ctx, tc.c, r.Value)
		}()
	}
	wg.Wait()
	for i, e := range entries {
		if e == nil {
			continue
		}
		recs[i].Addresses = e.addrs
		if len(e.addrs) == 0 {
			recs[i].AddressStatus = e.status
		}
	}
}

func (tc *targetCache) entry(target string) *targetAddrs {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	e := tc.m[target]
	if e == nil {
		e = &targetAddrs{}
		tc.m[target] = e
	}
	return e
}

// resolve asks A and AAAA in parallel, the first time only.
func (e *targetAddrs) resolve(ctx context.Context, c *Client, target string) {
	e.once.Do(func() {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, typ := range []string{"A", "AAAA"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := c.ask(ctx, Query{Type: typ, Name: target})
				mu.Lock()
				defer mu.Unlock()
				for _, rec := range r.Records {
					e.addrs = append(e.addrs, rec.Value)
				}
				e.status = worse(e.status, outcome(r))
			}()
		}
		wg.Wait()
		slices.SortFunc(e.addrs, func(a, b string) int {
			return netip.MustParseAddr(a).Compare(netip.MustParseAddr(b))
		})
	})
}

// outcome is a result's one-word summary: its response code, or for a
// question no server answered, why.
func outcome(r Result) string {
	if r.Status != "" {
		return r.Status
	}
	var ne net.Error
	if errors.Is(r.Err, context.DeadlineExceeded) || (errors.As(r.Err, &ne) && ne.Timeout()) {
		return "TIMEOUT"
	}
	if errors.Is(r.Err, context.Canceled) {
		return "CANCELLED"
	}
	return "ERROR"
}

// worse picks the more serious of two outcomes, so a target whose A lookup
// failed is not reported as merely having no AAAA record.
func worse(a, b string) string {
	if a == "" {
		return b
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func rank(o string) int {
	switch o {
	case "NOERROR", "NODATA":
		return 0
	case "NXDOMAIN":
		return 1
	case "TIMEOUT", "CANCELLED", "ERROR":
		return 3
	}
	return 2 // SERVFAIL, REFUSED, any other code
}

// ask puts one question to each nameserver in turn until one gives a final
// answer. NOERROR and NXDOMAIN are final; any other code, like SERVFAIL or
// REFUSED, moves on to the next server the way the stub resolver does, and is
// reported only when every server gave one. A server that does not answer is
// retried Attempts times before the next is tried. The RTT is the answering
// exchange's; for a question no server answered, it is the whole wait.
func (c *Client) ask(ctx context.Context, q Query) Result {
	start := time.Now()
	done := func(r Result) Result {
		r.Query = q
		if r.RTT == 0 {
			r.RTT = time.Since(start)
		}
		return r
	}
	qtype, ok := recordType(q.Type)
	if !ok {
		return done(Result{Err: fmt.Errorf("dns: unknown record type %q", q.Type)})
	}
	msg, id, err := buildQuery(q.Name, qtype)
	if err != nil {
		return done(Result{Err: err})
	}

	var lastErr error
	var refused *Result
	for _, server := range c.Nameservers {
		for attempt := 0; attempt < c.Attempts; attempt++ {
			if ctx.Err() != nil {
				// Out of time: a refusal already in hand still beats
				// reporting nothing.
				if refused != nil {
					return done(*refused)
				}
				return done(Result{Err: ctx.Err()})
			}
			sent := time.Now()
			ans, err := c.exchange(ctx, server, msg, id, q.Name, qtype)
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", server, err)
				continue
			}
			r := ans.result(server)
			r.RTT = time.Since(sent)
			if ans.rcode == dnsmessage.RCodeSuccess || ans.rcode == dnsmessage.RCodeNameError {
				return done(r)
			}
			refused = &r
			break
		}
	}
	if refused != nil {
		return done(*refused)
	}
	if lastErr == nil {
		lastErr = errors.New("dns: no nameservers")
	}
	return done(Result{Err: lastErr})
}

// exchange sends the question to one server over UDP and reads the reply,
// repeating over TCP when the reply was truncated.
func (c *Client) exchange(ctx context.Context, server string, msg []byte, id uint16, name string, qtype dnsmessage.Type) (answer, error) {
	actx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	resp, err := roundTrip(actx, "udp", server, msg, id)
	if err != nil {
		return answer{}, err
	}
	ans, truncated, err := parseAnswer(resp, id, name, qtype)
	if err != nil || !truncated {
		return ans, err
	}
	resp, err = roundTrip(actx, "tcp", server, msg, id)
	if err != nil {
		return answer{}, err
	}
	ans, truncated, err = parseAnswer(resp, id, name, qtype)
	if err == nil && truncated {
		// TCP has no size limit; a truncated reply here is a broken server
		// or middlebox, not an empty answer.
		return answer{}, errors.New("reply truncated over TCP")
	}
	return ans, err
}

// roundTrip does one send and receive on a fresh socket. On TCP the message
// is length-prefixed; on UDP a reply with another id (a late one to an
// earlier attempt) is skipped.
func roundTrip(ctx context.Context, network, server string, msg []byte, id uint16) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// A cancelled context ends the read at once rather than at the deadline,
	// so Stop is prompt.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if network == "tcp" {
		buf := make([]byte, 2+len(msg))
		binary.BigEndian.PutUint16(buf, uint16(len(msg)))
		copy(buf[2:], msg)
		if _, err := conn.Write(buf); err != nil {
			return nil, err
		}
		var l [2]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return nil, err
		}
		resp := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(conn, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 2 && binary.BigEndian.Uint16(buf[:2]) == id {
			return buf[:n], nil
		}
	}
}

func buildQuery(name string, qtype dnsmessage.Type) ([]byte, uint16, error) {
	qname, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, 0, fmt.Errorf("dns: %q: %w", name, err)
	}
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, 0, err
	}
	id := binary.BigEndian.Uint16(idb[:])
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{ID: id, RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, 0, err
	}
	if err := b.Question(dnsmessage.Question{Name: qname, Type: qtype, Class: dnsmessage.ClassINET}); err != nil {
		return nil, 0, err
	}
	if err := b.StartAdditionals(); err != nil {
		return nil, 0, err
	}
	// EDNS0: the OPT record's class carries the UDP payload size we accept.
	opt := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: udpPayload}
	if err := b.OPTResource(opt, dnsmessage.OPTResource{}); err != nil {
		return nil, 0, err
	}
	msg, err := b.Finish()
	return msg, id, err
}

// answer is a parsed reply: its code and the records of the asked type from
// the answer section, whatever their owner name, so a CNAME chain's final
// records count.
type answer struct {
	rcode dnsmessage.RCode
	recs  []Record
}

// parseAnswer reads a reply to the question (name, qtype). The reply must
// carry the id and echo the question: a 16-bit id alone is a thin guard
// against a spoofed or misdelivered reply.
func parseAnswer(msg []byte, id uint16, name string, qtype dnsmessage.Type) (ans answer, truncated bool, err error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return answer{}, false, fmt.Errorf("malformed reply: %w", err)
	}
	if h.ID != id {
		return answer{}, false, errors.New("reply id mismatch")
	}
	if !h.Response {
		return answer{}, false, errors.New("reply is not a response")
	}
	q, err := p.Question()
	if err != nil {
		return answer{}, false, fmt.Errorf("malformed reply: %w", err)
	}
	if !strings.EqualFold(q.Name.String(), name+".") || q.Type != qtype || q.Class != dnsmessage.ClassINET {
		return answer{}, false, errors.New("reply answers a different question")
	}
	if h.Truncated {
		return answer{}, true, nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return answer{}, false, fmt.Errorf("malformed reply: %w", err)
	}
	ans.rcode = h.RCode
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return ans, false, nil
		}
		if err != nil {
			return answer{}, false, fmt.Errorf("malformed reply: %w", err)
		}
		if rh.Type != qtype {
			if err := p.SkipAnswer(); err != nil {
				return answer{}, false, fmt.Errorf("malformed reply: %w", err)
			}
			continue
		}
		rec := Record{TTL: rh.TTL}
		switch qtype {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return answer{}, false, fmt.Errorf("malformed reply: %w", err)
			}
			rec.Value = netip.AddrFrom4(r.A).String()
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return answer{}, false, fmt.Errorf("malformed reply: %w", err)
			}
			rec.Value = netip.AddrFrom16(r.AAAA).String()
		case dnsmessage.TypeSRV:
			r, err := p.SRVResource()
			if err != nil {
				return answer{}, false, fmt.Errorf("malformed reply: %w", err)
			}
			rec.Value = trimDot(r.Target.String())
			rec.Priority, rec.Weight, rec.Port = r.Priority, r.Weight, r.Port
		case dnsmessage.TypePTR:
			r, err := p.PTRResource()
			if err != nil {
				return answer{}, false, fmt.Errorf("malformed reply: %w", err)
			}
			rec.Value = trimDot(r.PTR.String())
		}
		ans.recs = append(ans.recs, rec)
	}
}

// trimDot drops a name's trailing dot, keeping the root "." as is.
func trimDot(name string) string {
	if len(name) > 1 {
		return strings.TrimSuffix(name, ".")
	}
	return name
}

// result turns an answer into a Result: records in a fixed order so two
// sites' answers compare line by line, and the code by name.
func (a answer) result(server string) Result {
	res := Result{Records: a.recs, Server: server, Status: rcodeName(a.rcode)}
	if a.rcode == dnsmessage.RCodeSuccess && len(res.Records) == 0 {
		res.Status = "NODATA"
	}
	slices.SortFunc(res.Records, func(x, y Record) int {
		if x.Priority != y.Priority {
			return int(x.Priority) - int(y.Priority)
		}
		if x.Weight != y.Weight {
			return int(y.Weight) - int(x.Weight)
		}
		if ax, err := netip.ParseAddr(x.Value); err == nil {
			if ay, err := netip.ParseAddr(y.Value); err == nil {
				return ax.Compare(ay)
			}
		}
		if x.Value != y.Value {
			return strings.Compare(x.Value, y.Value)
		}
		return int(x.Port) - int(y.Port)
	})
	return res
}

func recordType(t string) (dnsmessage.Type, bool) {
	switch t {
	case "A":
		return dnsmessage.TypeA, true
	case "AAAA":
		return dnsmessage.TypeAAAA, true
	case "SRV":
		return dnsmessage.TypeSRV, true
	case "PTR":
		return dnsmessage.TypePTR, true
	}
	return 0, false
}

// rcodeName is the response code's mnemonic as dig prints it.
func rcodeName(rc dnsmessage.RCode) string {
	switch rc {
	case dnsmessage.RCodeSuccess:
		return "NOERROR"
	case dnsmessage.RCodeFormatError:
		return "FORMERR"
	case dnsmessage.RCodeServerFailure:
		return "SERVFAIL"
	case dnsmessage.RCodeNameError:
		return "NXDOMAIN"
	case dnsmessage.RCodeNotImplemented:
		return "NOTIMP"
	case dnsmessage.RCodeRefused:
		return "REFUSED"
	}
	return "RCODE" + strconv.Itoa(int(rc))
}
