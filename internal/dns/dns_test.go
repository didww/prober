package dns_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/didww/prober/internal/dns"
	"github.com/didww/prober/internal/dns/dnstest"
)

func run(t *testing.T, ctx context.Context, spec dns.Spec, c *dns.Client) []dns.Event {
	t.Helper()
	var evs []dns.Event
	if err := dns.Run(ctx, spec, c, func(e dns.Event) { evs = append(evs, e) }); err != nil {
		t.Fatalf("run: %v", err)
	}
	return evs
}

// byQuery indexes the ResultDone events of a run and checks the envelope.
func byQuery(t *testing.T, evs []dns.Event) map[dns.Query]*dns.Result {
	t.Helper()
	if len(evs) < 2 || evs[0].Kind != dns.Started || evs[len(evs)-1].Kind != dns.Finished {
		t.Fatalf("bad envelope: %+v", evs)
	}
	out := map[dns.Query]*dns.Result{}
	for _, e := range evs[1 : len(evs)-1] {
		if e.Kind != dns.ResultDone || e.Result == nil {
			t.Fatalf("unexpected event in the middle: %+v", e)
		}
		out[e.Result.Query] = e.Result
	}
	return out
}

func client(ns ...*dnstest.Server) *dns.Client {
	c := &dns.Client{}
	for _, s := range ns {
		c.Nameservers = append(c.Nameservers, s.Addr)
	}
	return c
}

func TestQueries(t *testing.T) {
	got := dns.Queries("example.com")
	want := []dns.Query{
		{Type: "A", Name: "example.com"},
		{Type: "AAAA", Name: "example.com"},
		{Type: "SRV", Name: "_sip._udp.example.com"},
		{Type: "SRV", Name: "_sip._tcp.example.com"},
		{Type: "SRV", Name: "_sip._tls.example.com"},
		{Type: "SRV", Name: "_sips._tcp.example.com"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// An address asks one PTR question, under in-addr.arpa or ip6.arpa.
	if got := dns.Queries("192.0.2.1"); len(got) != 1 || got[0] != (dns.Query{Type: "PTR", Name: "1.2.0.192.in-addr.arpa"}) {
		t.Errorf("v4 PTR: %+v", got)
	}
	want6 := "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa"
	if got := dns.Queries("2001:db8::1"); len(got) != 1 || got[0] != (dns.Query{Type: "PTR", Name: want6}) {
		t.Errorf("v6 PTR: %+v", got)
	}
}

func TestRunAnswers(t *testing.T) {
	ns := dnstest.Start(t, map[string]dnstest.Answer{
		// Out of order on purpose: the engine sorts. Behind a CNAME, so the
		// records' owner is not the name asked for.
		"A example.com":    {CNAME: "web.example.net", Addrs: []string{"192.0.2.20", "192.0.2.10"}},
		"AAAA example.com": {}, // NOERROR with nothing in it
		"SRV _sip._udp.example.com": {SRV: []dnstest.SRV{
			{Target: "b.example.com", Port: 5060, Priority: 20, Weight: 0},
			{Target: "a.example.com", Port: 5060, Priority: 10, Weight: 40},
			{Target: "c.example.com", Port: 5061, Priority: 10, Weight: 60},
		}},
		// _sip._tcp is absent: NXDOMAIN.
		"SRV _sip._tls.example.com":  {RCode: dnsmessage.RCodeServerFailure},
		"SRV _sips._tcp.example.com": {TruncateUDP: true, SRV: []dnstest.SRV{{Target: "."}}},
		// The SRV targets: one dual-stack, one v6-only, one that does not
		// exist at all.
		"A a.example.com":    {Addrs: []string{"192.0.2.1"}},
		"AAAA a.example.com": {Addrs: []string{"2001:db8::1"}},
		"AAAA c.example.com": {Addrs: []string{"2001:db8::3"}},
		"A c.example.com":    {},
	})

	// The name is normalised before anything is asked, and the servers are
	// reported up front.
	evs := run(t, context.Background(), dns.Spec{Name: "  Example.COM. "}, client(ns))
	if got := evs[0].Spec.Name; got != "example.com" {
		t.Fatalf("normalised name: got %q", got)
	}
	if got := evs[0].Nameservers; len(got) != 1 || got[0] != ns.Addr {
		t.Fatalf("nameservers: %v", got)
	}
	if evs[len(evs)-1].Cancelled {
		t.Fatal("finished as cancelled")
	}
	res := byQuery(t, evs)
	if len(res) != 6 {
		t.Fatalf("got %d results, want 6", len(res))
	}
	for q, r := range res {
		if r.Err != nil || r.Server != ns.Addr || r.RTT <= 0 {
			t.Errorf("%+v: err=%v server=%q rtt=%v", q, r.Err, r.Server, r.RTT)
		}
	}

	a := res[dns.Query{Type: "A", Name: "example.com"}]
	if a.Status != "NOERROR" || len(a.Records) != 2 || a.Records[0].Value != "192.0.2.10" || a.Records[1].Value != "192.0.2.20" || a.Records[0].TTL != 300 {
		t.Errorf("A: %+v", a)
	}
	if aaaa := res[dns.Query{Type: "AAAA", Name: "example.com"}]; aaaa.Status != "NODATA" || len(aaaa.Records) != 0 {
		t.Errorf("AAAA (nodata): %+v", aaaa)
	}

	// SRV sorted by priority, then weight descending; trailing dots dropped;
	// each target's addresses beside it, or why there are none.
	udp := res[dns.Query{Type: "SRV", Name: "_sip._udp.example.com"}]
	if udp.Status != "NOERROR" || len(udp.Records) != 3 {
		t.Fatalf("SRV udp: %+v", udp)
	}
	want := []dns.Record{
		{Value: "c.example.com", Priority: 10, Weight: 60, Port: 5061, TTL: 60, Addresses: []string{"2001:db8::3"}},
		{Value: "a.example.com", Priority: 10, Weight: 40, Port: 5060, TTL: 60, Addresses: []string{"192.0.2.1", "2001:db8::1"}},
		{Value: "b.example.com", Priority: 20, Weight: 0, Port: 5060, TTL: 60, AddressStatus: "NXDOMAIN"},
	}
	for i, w := range want {
		got := udp.Records[i]
		if got.Value != w.Value || got.Priority != w.Priority || got.Weight != w.Weight || got.Port != w.Port || got.TTL != w.TTL ||
			!slices.Equal(got.Addresses, w.Addresses) || got.AddressStatus != w.AddressStatus {
			t.Errorf("SRV udp[%d]: got %+v, want %+v", i, got, w)
		}
	}
	// Each target's addresses are asked for once, and the root target never.
	for _, key := range []string{"A a.example.com", "AAAA a.example.com", "A b.example.com", "AAAA c.example.com"} {
		if n := ns.Asked(key); n != 1 {
			t.Errorf("%s asked %d times, want 1", key, n)
		}
	}
	if n := ns.Asked("A ."); n != 0 {
		t.Errorf("the root target was looked up %d times", n)
	}

	if tcp := res[dns.Query{Type: "SRV", Name: "_sip._tcp.example.com"}]; tcp.Status != "NXDOMAIN" || len(tcp.Records) != 0 {
		t.Errorf("SRV tcp (nxdomain): %+v", tcp)
	}
	if tls := res[dns.Query{Type: "SRV", Name: "_sip._tls.example.com"}]; tls.Status != "SERVFAIL" || len(tls.Records) != 0 {
		t.Errorf("SRV tls (servfail): %+v", tls)
	}
	// Truncated over UDP, so answered over TCP; the RFC 2782 "service not
	// available" target is kept as "." and has no addresses to look up.
	if sips := res[dns.Query{Type: "SRV", Name: "_sips._tcp.example.com"}]; sips.Status != "NOERROR" || len(sips.Records) != 1 || sips.Records[0].Value != "." || sips.Records[0].AddressStatus != "" {
		t.Errorf("SRV sips (truncated): %+v", sips)
	}
}

func TestRunSharesSrvTargets(t *testing.T) {
	// All four SIP services point at the same host, as they usually do: its
	// addresses are asked for once for the whole run.
	srv := dnstest.Answer{SRV: []dnstest.SRV{{Target: "sip.example.com", Port: 5060, Priority: 10, Weight: 10}}}
	ns := dnstest.Start(t, map[string]dnstest.Answer{
		"SRV _sip._udp.example.com":  srv,
		"SRV _sip._tcp.example.com":  srv,
		"SRV _sip._tls.example.com":  srv,
		"SRV _sips._tcp.example.com": srv,
		"A sip.example.com":          {Addrs: []string{"192.0.2.1"}},
	})
	res := byQuery(t, run(t, context.Background(), dns.Spec{Name: "example.com"}, client(ns)))
	for _, r := range res {
		if r.Query.Type == "SRV" && (len(r.Records) != 1 || !slices.Equal(r.Records[0].Addresses, []string{"192.0.2.1"})) {
			t.Errorf("%+v: %+v", r.Query, r.Records)
		}
	}
	if n := ns.Asked("A sip.example.com"); n != 1 {
		t.Errorf("shared target asked %d times, want 1", n)
	}
	if n := ns.Asked("AAAA sip.example.com"); n != 1 {
		t.Errorf("shared target asked %d times for AAAA, want 1", n)
	}
}

func TestRunReverse(t *testing.T) {
	ns := dnstest.Start(t, map[string]dnstest.Answer{
		"PTR 1.2.0.192.in-addr.arpa": {PTR: []string{"host.example.com"}},
	})

	// An address, even bracketed or mapped, is canonicalised and asks one
	// PTR question.
	evs := run(t, context.Background(), dns.Spec{Name: " [::ffff:192.0.2.1] "}, client(ns))
	if got := evs[0].Spec.Name; got != "192.0.2.1" {
		t.Fatalf("normalised address: got %q", got)
	}
	res := byQuery(t, evs)
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(res), res)
	}
	ptr := res[dns.Query{Type: "PTR", Name: "1.2.0.192.in-addr.arpa"}]
	if ptr == nil || ptr.Status != "NOERROR" || len(ptr.Records) != 1 || ptr.Records[0].Value != "host.example.com" || ptr.Records[0].TTL != 3600 {
		t.Fatalf("PTR: %+v", ptr)
	}

	// An address without a PTR record reports the code, not an error.
	res = byQuery(t, run(t, context.Background(), dns.Spec{Name: "2001:db8::1"}, client(ns)))
	for _, r := range res {
		if r.Query.Type != "PTR" || !strings.HasSuffix(r.Query.Name, ".ip6.arpa") || r.Status != "NXDOMAIN" || r.Err != nil {
			t.Errorf("v6 PTR: %+v", r)
		}
	}
}

func TestRunTriesNextServer(t *testing.T) {
	refusing := dnstest.Start(t, nil)
	refusing.SetDefaultRCode(dnsmessage.RCodeRefused)
	answering := dnstest.Start(t, map[string]dnstest.Answer{"A example.com": {Addrs: []string{"192.0.2.1"}}})

	// REFUSED from the first server is not final: the second answers.
	res := byQuery(t, run(t, context.Background(), dns.Spec{Name: "example.com"}, client(refusing, answering)))
	a := res[dns.Query{Type: "A", Name: "example.com"}]
	if a.Err != nil || a.Status != "NOERROR" || a.Server != answering.Addr || len(a.Records) != 1 {
		t.Errorf("A via second server: %+v", a)
	}
	// NXDOMAIN from the second is final too.
	if aaaa := res[dns.Query{Type: "AAAA", Name: "example.com"}]; aaaa.Status != "NXDOMAIN" || aaaa.Server != answering.Addr {
		t.Errorf("AAAA: %+v", aaaa)
	}

	// When every server refuses, that is the answer, from the last one.
	res = byQuery(t, run(t, context.Background(), dns.Spec{Name: "example.com"}, client(refusing, refusing)))
	if a := res[dns.Query{Type: "A", Name: "example.com"}]; a.Err != nil || a.Status != "REFUSED" || a.Server != refusing.Addr {
		t.Errorf("all refused: %+v", a)
	}

	// A silent first server is retried Attempts times, then the second is
	// asked; the RTT is the second's answer, not the wait for the first.
	silent := dnstest.Start(t, nil)
	silent.SetDropAll(true)
	c := client(silent, answering)
	c.Timeout, c.Attempts = 100*time.Millisecond, 2
	res = byQuery(t, run(t, context.Background(), dns.Spec{Name: "example.com"}, c))
	if a := res[dns.Query{Type: "A", Name: "example.com"}]; a.Err != nil || a.Status != "NOERROR" || a.Server != answering.Addr || a.RTT >= 100*time.Millisecond {
		t.Errorf("after a silent server: %+v", a)
	}
	if n := silent.AskedTotal(); n != 12 {
		t.Errorf("silent server asked %d times, want 12 (6 questions x 2 attempts)", n)
	}

	// Out of time after a refusal but before the next server answered: the
	// refusal is still reported, not a bare timeout.
	c = client(refusing, silent)
	c.Timeout, c.Attempts = 5*time.Second, 1
	res = byQuery(t, run(t, context.Background(), dns.Spec{Name: "example.com", Timeout: 150 * time.Millisecond}, c))
	if a := res[dns.Query{Type: "A", Name: "example.com"}]; a.Err != nil || a.Status != "REFUSED" || a.Server != refusing.Addr {
		t.Errorf("refused then out of time: %+v", a)
	}
}

func TestRunRejectsBrokenReplies(t *testing.T) {
	ns := dnstest.Start(t, map[string]dnstest.Answer{
		"A example.com":    {TruncateUDP: true, TruncateTCP: true, Addrs: []string{"192.0.2.1"}},
		"AAAA example.com": {WrongQuestion: true, Addrs: []string{"2001:db8::1"}},
	})
	c := client(ns)
	c.Attempts = 1
	res := byQuery(t, run(t, context.Background(), dns.Spec{Name: "example.com"}, c))
	if a := res[dns.Query{Type: "A", Name: "example.com"}]; a.Err == nil || !strings.Contains(a.Err.Error(), "truncated over TCP") || a.Status != "" {
		t.Errorf("truncated over TCP: %+v", a)
	}
	if aaaa := res[dns.Query{Type: "AAAA", Name: "example.com"}]; aaaa.Err == nil || !strings.Contains(aaaa.Err.Error(), "different question") || len(aaaa.Records) != 0 {
		t.Errorf("wrong question: %+v", aaaa)
	}
}

func TestRunRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "   ", "-bad.example.com", "a..b", "sp ace.example.com"} {
		err := dns.Run(context.Background(), dns.Spec{Name: name}, &dns.Client{}, func(dns.Event) {
			t.Errorf("%q: emitted an event", name)
		})
		if err == nil {
			t.Errorf("%q: accepted", name)
		}
	}
}

func TestRunTimeout(t *testing.T) {
	silent := dnstest.Start(t, nil)
	silent.SetDropAll(true)
	c := client(silent)
	c.Timeout, c.Attempts = 30*time.Millisecond, 1

	start := time.Now()
	evs := run(t, context.Background(), dns.Spec{Name: "slow.example.com", Timeout: 200 * time.Millisecond}, c)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v, the timeout did not bound the queries", el)
	}
	if evs[len(evs)-1].Cancelled {
		t.Fatal("a timeout is not a cancellation")
	}
	for q, r := range byQuery(t, evs) {
		if r.Err == nil || r.Status != "" || r.Server != "" || !strings.Contains(r.Err.Error(), silent.Addr) {
			t.Errorf("%+v: err=%v status=%q server=%q, want a timeout naming the server", q, r.Err, r.Status, r.Server)
		}
	}

	// An unreachable server (nothing listening) is an error too, at once.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := pc.LocalAddr().String()
	pc.Close()
	res := byQuery(t, run(t, context.Background(), dns.Spec{Name: "example.com"}, &dns.Client{Nameservers: []string{dead}, Timeout: 500 * time.Millisecond, Attempts: 1}))
	if a := res[dns.Query{Type: "A", Name: "example.com"}]; a.Err == nil || a.Status != "" {
		t.Errorf("unreachable: %+v", a)
	}
}

func TestRunCancel(t *testing.T) {
	// Long per-attempt timeouts: a cancel must not wait them out.
	silent := dnstest.Start(t, nil)
	silent.SetDropAll(true)
	c := client(silent)
	c.Timeout, c.Attempts = 5*time.Second, 2

	ctx, cancel := context.WithCancel(context.Background())
	var evs []dns.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = dns.Run(ctx, dns.Spec{Name: "slow.example.com"}, c, func(e dns.Event) { evs = append(evs, e) })
	}()
	time.Sleep(20 * time.Millisecond)
	cancelled := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if took := time.Since(cancelled); took > 500*time.Millisecond {
		t.Errorf("cancel took %v, want prompt", took)
	}
	if !evs[len(evs)-1].Cancelled {
		t.Fatalf("finished event not marked cancelled: %+v", evs[len(evs)-1])
	}
	res := byQuery(t, evs)
	if len(res) != 6 {
		t.Fatalf("got %d results after cancel, want all 6 reported", len(res))
	}
	for q, r := range res {
		if !errors.Is(r.Err, context.Canceled) {
			t.Errorf("%+v: err = %v, want cancelled", q, r.Err)
		}
	}
}
