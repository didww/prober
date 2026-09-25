// Package sip is the SIP OPTIONS probing engine: it repeatedly sends an
// OPTIONS request to a target over a chosen transport (UDP, TCP, TLS or WSS)
// and reports each attempt's outcome — status code, round-trip time, and the
// raw request and response text.
//
// One request is sent per cycle with retransmission suppressed, so N cycles is
// exactly N packets and a lost response shows as a timeout rather than being
// hidden by the transaction layer's retransmits. That is deliberate: a looking
// glass should reveal loss, not mask it. Built on github.com/emiago/sipgo.
//
// SIP needs no raw sockets, so this engine works on any host, unlike the
// traceroute engine.
package sip

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type Transport uint8

const (
	UDP Transport = iota + 1
	TCP
	TLS
	WSS
)

func (t Transport) String() string {
	switch t {
	case UDP:
		return "udp"
	case TCP:
		return "tcp"
	case TLS:
		return "tls"
	case WSS:
		return "wss"
	}
	return "unknown"
}

// name is the transport token sipgo expects on the request.
func (t Transport) name() string {
	switch t {
	case UDP:
		return "UDP"
	case TCP:
		return "TCP"
	case TLS:
		return "TLS"
	case WSS:
		return "WSS"
	}
	return "UDP"
}

// tlsBased reports whether the transport runs over TLS (so cert validity is
// meaningful).
func (t Transport) tlsBased() bool { return t == TLS || t == WSS }

// defaultPort is the port used when the spec leaves it 0.
func (t Transport) defaultPort() uint16 {
	switch t {
	case TLS:
		return 5061
	case WSS:
		return 443
	default:
		return 5060
	}
}

// Spec is one SIP OPTIONS probe as the engine runs it. The target is already
// an address: resolving a name is the caller's job, since which resolver
// answers is a property of the site.
type Spec struct {
	Target    netip.Addr
	Port      uint16
	Transport Transport
	Cycles    int // 0 = until the context is cancelled
	Interval  time.Duration
	Timeout   time.Duration
	Source    netip.Addr
	UserAgent string
}

const (
	DefaultInterval  = time.Second
	DefaultTimeout   = 5 * time.Second
	DefaultUserAgent = "prober"
	// fromIdentity is the SIP From user and display name. It stays a bare
	// token (no version, no slash) so the From URI user part is always valid;
	// the software version is carried in the User-Agent header instead.
	fromIdentity = "prober"
)

func (s Spec) normalize() (Spec, error) {
	if !s.Target.IsValid() {
		return s, errors.New("sip: target address is required")
	}
	s.Target = s.Target.Unmap()
	switch s.Transport {
	case UDP, TCP, TLS, WSS:
	case 0:
		s.Transport = UDP
	default:
		return s, fmt.Errorf("sip: unknown transport %d", s.Transport)
	}
	if s.Port == 0 {
		s.Port = s.Transport.defaultPort()
	}
	if s.Interval <= 0 {
		s.Interval = DefaultInterval
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	if s.UserAgent == "" {
		s.UserAgent = DefaultUserAgent
	}
	return s, nil
}

// --- events -----------------------------------------------------------------

type EventKind uint8

const (
	Started EventKind = iota + 1
	ResultDone
	Finished
	Failed
)

type Event struct {
	Kind      EventKind
	Time      time.Time
	Spec      *Spec   // Started
	Result    *Result // ResultDone
	Cancelled bool    // Finished
	Err       error   // Failed
}

// Result is one cycle's outcome.
type Result struct {
	Cycle      int
	StatusCode int // 0 = no response (timeout)
	Reason     string
	RTT        time.Duration
	Responded  bool
	Request    string
	Response   string
	TLS        bool   // a TLS transport was used
	TLSValid   bool   // the server cert chained to a trusted root and matched
	TLSError   string // why not, for the warning hover
}

// suppressRetransmit makes sipgo's transaction retransmit timers effectively
// infinite, process-wide and once. Each probe bounds its own wait with a
// context deadline and terminates the transaction on return, so exactly one
// packet leaves per cycle regardless of the timeout the operator picks.
var suppressRetransmit = sync.OnceFunc(func() {
	const forever = 24 * time.Hour
	sip.SetTimers(forever, forever, 5*time.Second)
})

// Run sends OPTIONS every Interval and calls emit for each event, in order,
// from a single goroutine. It returns when the run finishes, fails, or ctx is
// cancelled; the terminal event has already been emitted.
func Run(ctx context.Context, spec Spec, log *slog.Logger, emit func(Event)) error {
	spec, err := spec.normalize()
	if err != nil {
		return err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	suppressRetransmit()

	// Per-run TLS state, written by the verify callback on handshake and read
	// into every result. Never fails the handshake — the probe should reach a
	// server with a bad cert and simply flag it.
	var tlsMu sync.Mutex
	tlsChecked := false
	tlsValid := false
	tlsErr := ""

	// WithUserAgent sets sipgo's UA name, which it uses for BOTH the From
	// display name and the From URI user part. Feed it the clean identity, not
	// spec.UserAgent (e.g. "prober/1.2.0"), whose slash would land inside the
	// From URI user part. The version travels in the explicit User-Agent header.
	uaOpts := []sipgo.UserAgentOption{sipgo.WithUserAgent(fromIdentity)}
	if spec.Transport.tlsBased() {
		serverName := spec.Target.String()
		conf := &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // deliberate: a reachability probe, validity is reported not enforced
			VerifyConnection: func(cs tls.ConnectionState) error {
				valid, why := verifyChain(cs, serverName)
				tlsMu.Lock()
				tlsChecked, tlsValid, tlsErr = true, valid, why
				tlsMu.Unlock()
				return nil
			},
		}
		uaOpts = append(uaOpts, sipgo.WithUserAgenTLSConfig(conf))
	}

	ua, err := sipgo.NewUA(uaOpts...)
	if err != nil {
		emit(Event{Kind: Failed, Time: time.Now(), Err: err})
		return err
	}
	defer ua.Close()

	clientOpts := []sipgo.ClientOption{sipgo.WithClientLogger(log)}
	if spec.Source.IsValid() {
		clientOpts = append(clientOpts, sipgo.WithClientHostname(spec.Source.String()))
	}
	client, err := sipgo.NewClient(ua, clientOpts...)
	if err != nil {
		emit(Event{Kind: Failed, Time: time.Now(), Err: err})
		return err
	}
	defer client.Close()

	emit(Event{Kind: Started, Time: time.Now(), Spec: &spec})

	dest := destination(spec.Target, spec.Port)
	recipient := recipientURI(spec)

	t := time.NewTicker(spec.Interval)
	defer t.Stop()

	cancelled := false
	for cycle := 1; spec.Cycles == 0 || cycle <= spec.Cycles; cycle++ {
		res := probeOnce(ctx, client, recipient, dest, spec)
		res.Cycle = cycle
		if spec.Transport.tlsBased() {
			res.TLS = true
			tlsMu.Lock()
			if tlsChecked {
				res.TLSValid, res.TLSError = tlsValid, tlsErr
			}
			tlsMu.Unlock()
		}
		emit(Event{Kind: ResultDone, Time: time.Now(), Result: &res})

		if spec.Cycles != 0 && cycle >= spec.Cycles {
			break
		}
		select {
		case <-ctx.Done():
			cancelled = true
		case <-t.C:
		}
		if cancelled {
			break
		}
	}

	emit(Event{Kind: Finished, Time: time.Now(), Cancelled: cancelled})
	return nil
}

// probeOnce sends exactly one OPTIONS and waits up to Timeout for the final
// response.
func probeOnce(ctx context.Context, client *sipgo.Client, recipient sip.Uri, dest string, spec Spec) Result {
	req := sip.NewRequest(sip.OPTIONS, recipient)
	req.SetTransport(spec.Transport.name())
	req.SetDestination(dest)
	// sipgo's UserAgent name only feeds the From header, not a User-Agent
	// header, so set one explicitly. Accept mirrors what an OPTIONS peer expects.
	req.AppendHeader(sip.NewHeader("User-Agent", spec.UserAgent))
	req.AppendHeader(sip.NewHeader("Accept", "application/sdp"))

	reqCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	sent := time.Now()
	res, err := client.Do(reqCtx, req)
	rtt := time.Since(sent)

	out := Result{Request: req.String(), RTT: rtt}
	if err != nil || res == nil {
		// Timeout or transport error: no usable response. The request text is
		// still captured so the operator sees what was sent.
		return out
	}
	out.Responded = true
	out.StatusCode = int(res.StatusCode)
	out.Reason = res.Reason
	out.Response = res.String()
	return out
}

func recipientURI(spec Spec) sip.Uri {
	scheme := "sip"
	if spec.Transport.tlsBased() {
		scheme = "sips"
	}
	u := sip.Uri{
		Scheme:    scheme,
		Host:      spec.Target.String(),
		Port:      int(spec.Port),
		UriParams: sip.NewParams(),
	}
	u.UriParams.Add("transport", spec.Transport.String())
	return u
}

// destination is the host:port sipgo dials, bracketing IPv6.
func destination(addr netip.Addr, port uint16) string {
	return net.JoinHostPort(addr.String(), strconv.Itoa(int(port)))
}

// verifyChain checks a presented certificate against the system roots and the
// server name, returning whether it is valid and, if not, a short reason.
func verifyChain(cs tls.ConnectionState, serverName string) (bool, string) {
	if len(cs.PeerCertificates) == 0 {
		return false, "server presented no certificate"
	}
	opts := x509.VerifyOptions{
		DNSName:       serverName,
		Intermediates: x509.NewCertPool(),
	}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
		// A probe targets an IP, so a name mismatch is expected and not worth a
		// warning; report only genuine trust/expiry problems by retrying the
		// verification without the name check.
		noName := x509.VerifyOptions{Intermediates: opts.Intermediates}
		if _, err2 := cs.PeerCertificates[0].Verify(noName); err2 != nil {
			return false, err2.Error()
		}
		return false, err.Error()
	}
	return true, ""
}
