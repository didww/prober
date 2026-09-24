package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fake answers from fixed tables, keyed "A host", "AAAA host" and "SRV name".
// A missing key is NXDOMAIN; a key in errs fails with that error. block makes
// every lookup wait for the context, for the timeout and cancel tests.
type fake struct {
	ips   map[string][]netip.Addr
	srv   map[string][]*net.SRV
	errs  map[string]error
	block bool
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fake) wait(ctx context.Context) error {
	if !f.block {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fake) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	typ := "A"
	if network == "ip6" {
		typ = "AAAA"
	}
	key := typ + " " + host
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	ips, ok := f.ips[key]
	if !ok {
		return nil, notFound(host)
	}
	return ips, nil
}

func (f *fake) LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
	if err := f.wait(ctx); err != nil {
		return "", nil, err
	}
	if service != "" || proto != "" {
		return "", nil, errors.New("expected a direct lookup of the full name")
	}
	key := "SRV " + name
	if err := f.errs[key]; err != nil {
		return "", nil, err
	}
	srvs, ok := f.srv[key]
	if !ok {
		return "", nil, notFound(name)
	}
	return name, srvs, nil
}

func run(t *testing.T, ctx context.Context, spec Spec, r Resolver) []Event {
	t.Helper()
	var evs []Event
	if err := Run(ctx, spec, r, func(e Event) { evs = append(evs, e) }); err != nil {
		t.Fatalf("run: %v", err)
	}
	return evs
}

// byQuery indexes the ResultDone events of a run and checks the envelope.
func byQuery(t *testing.T, evs []Event) map[Query]*Result {
	t.Helper()
	if len(evs) < 2 || evs[0].Kind != Started || evs[len(evs)-1].Kind != Finished {
		t.Fatalf("bad envelope: %+v", evs)
	}
	out := map[Query]*Result{}
	for _, e := range evs[1 : len(evs)-1] {
		if e.Kind != ResultDone || e.Result == nil {
			t.Fatalf("unexpected event in the middle: %+v", e)
		}
		out[e.Result.Query] = e.Result
	}
	return out
}

func TestQueries(t *testing.T) {
	got := Queries("example.com")
	want := []Query{
		{"A", "example.com"},
		{"AAAA", "example.com"},
		{"SRV", "_sip._udp.example.com"},
		{"SRV", "_sip._tcp.example.com"},
		{"SRV", "_sip._tls.example.com"},
		{"SRV", "_sips._tcp.example.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d queries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("query %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRunAnswers(t *testing.T) {
	r := &fake{
		ips: map[string][]netip.Addr{
			// Out of order on purpose: the engine sorts.
			"A example.com":    {netip.MustParseAddr("192.0.2.20"), netip.MustParseAddr("192.0.2.10")},
			"AAAA example.com": {netip.MustParseAddr("2001:db8::2")},
		},
		srv: map[string][]*net.SRV{
			"SRV _sip._udp.example.com": {
				{Target: "b.example.com.", Port: 5060, Priority: 20, Weight: 0},
				{Target: "a.example.com.", Port: 5060, Priority: 10, Weight: 40},
				{Target: "c.example.com.", Port: 5061, Priority: 10, Weight: 60},
			},
			"SRV _sips._tcp.example.com": {{Target: ".", Port: 0, Priority: 0, Weight: 0}},
		},
		errs: map[string]error{
			"SRV _sip._tls.example.com": &net.DNSError{Err: "server misbehaving", Name: "_sip._tls.example.com", IsTemporary: true},
		},
	}
	// The name is normalised before anything is asked.
	evs := run(t, context.Background(), Spec{Name: "  Example.COM. "}, r)
	if got := evs[0].Spec.Name; got != "example.com" {
		t.Fatalf("normalised name: got %q", got)
	}
	if evs[len(evs)-1].Cancelled {
		t.Fatal("finished as cancelled")
	}
	res := byQuery(t, evs)
	if len(res) != 6 {
		t.Fatalf("got %d results, want 6", len(res))
	}

	a := res[Query{"A", "example.com"}]
	if a.Err != nil || len(a.Records) != 2 || a.Records[0].Value != "192.0.2.10" || a.Records[1].Value != "192.0.2.20" {
		t.Errorf("A: %+v", a)
	}
	aaaa := res[Query{"AAAA", "example.com"}]
	if aaaa.Err != nil || len(aaaa.Records) != 1 || aaaa.Records[0].Value != "2001:db8::2" {
		t.Errorf("AAAA: %+v", aaaa)
	}

	// SRV sorted by priority, then weight descending; trailing dots dropped.
	udp := res[Query{"SRV", "_sip._udp.example.com"}]
	if udp.Err != nil || len(udp.Records) != 3 {
		t.Fatalf("SRV udp: %+v", udp)
	}
	wantOrder := []Record{
		{"c.example.com", 10, 60, 5061},
		{"a.example.com", 10, 40, 5060},
		{"b.example.com", 20, 0, 5060},
	}
	for i, w := range wantOrder {
		if udp.Records[i] != w {
			t.Errorf("SRV udp[%d]: got %+v, want %+v", i, udp.Records[i], w)
		}
	}

	// NXDOMAIN is an empty answer, not an error.
	tcp := res[Query{"SRV", "_sip._tcp.example.com"}]
	if tcp.Err != nil || len(tcp.Records) != 0 {
		t.Errorf("SRV tcp (nxdomain): %+v", tcp)
	}
	// The RFC 2782 "service not available" target is kept as ".".
	sips := res[Query{"SRV", "_sips._tcp.example.com"}]
	if sips.Err != nil || len(sips.Records) != 1 || sips.Records[0].Value != "." {
		t.Errorf("SRV sips: %+v", sips)
	}
	// A resolver failure is an error.
	tls := res[Query{"SRV", "_sip._tls.example.com"}]
	if tls.Err == nil || len(tls.Records) != 0 {
		t.Errorf("SRV tls (servfail): %+v", tls)
	}
}

func TestRunRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "   ", "192.0.2.1", "2001:db8::1", "-bad.example.com", "a..b", "sp ace.example.com"} {
		err := Run(context.Background(), Spec{Name: name}, &fake{}, func(Event) {
			t.Errorf("%q: emitted an event", name)
		})
		if err == nil {
			t.Errorf("%q: accepted", name)
		}
	}
}

func TestRunTimeout(t *testing.T) {
	start := time.Now()
	evs := run(t, context.Background(), Spec{Name: "slow.example.com", Timeout: 50 * time.Millisecond}, &fake{block: true})
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v, the timeout did not bound the queries", el)
	}
	if evs[len(evs)-1].Cancelled {
		t.Fatal("a timeout is not a cancellation")
	}
	for q, r := range byQuery(t, evs) {
		if !errors.Is(r.Err, context.DeadlineExceeded) {
			t.Errorf("%+v: err = %v, want deadline exceeded", q, r.Err)
		}
	}
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var evs []Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, Spec{Name: "slow.example.com"}, &fake{block: true}, func(e Event) { evs = append(evs, e) })
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if !evs[len(evs)-1].Cancelled {
		t.Fatalf("finished event not marked cancelled: %+v", evs[len(evs)-1])
	}
	if n := len(byQuery(t, evs)); n != 6 {
		t.Fatalf("got %d results after cancel, want all 6 reported", n)
	}
}

func TestNameserversFrom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	conf := "# generated\nsearch example.com\nnameserver 10.0.0.53\noptions ndots:1\nnameserver 2001:db8::53 # v6\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	got := nameserversFrom(path)
	if len(got) != 2 || got[0] != "10.0.0.53" || got[1] != "2001:db8::53" {
		t.Fatalf("got %v", got)
	}
	if nameserversFrom(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("a missing file should yield nil")
	}
}
