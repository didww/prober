package trace

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// These tests need raw sockets. Without CAP_NET_RAW they skip; the Makefile
// runs them inside an unprivileged user+network namespace, where the test
// process is root over a loopback of its own:
//
//	unshare -Urn sh -c 'ip link set lo up && go test ./internal/trace/'

func testEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(nil)
	if err != nil {
		t.Skipf("raw sockets unavailable: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func fastSpec(target string) Spec {
	return Spec{
		Target:       netip.MustParseAddr(target),
		Cycles:       2,
		Interval:     150 * time.Millisecond,
		ProbeTimeout: 800 * time.Millisecond,
	}
}

func runTrace(t *testing.T, e *Engine, spec Spec) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var events []Event
	if err := e.Run(ctx, spec, func(ev Event) { events = append(events, ev) }); err != nil {
		t.Fatalf("run: %v", err)
	}
	return events
}

// cycles pulls the CycleDone events out and checks the envelope around them.
func cycles(t *testing.T, events []Event, want int) []*Cycle {
	t.Helper()
	if len(events) < 3 || events[0].Kind != Started || events[len(events)-1].Kind != Finished {
		t.Fatalf("event envelope wrong: %+v", kinds(events))
	}
	var out []*Cycle
	for _, ev := range events[1 : len(events)-1] {
		if ev.Kind != CycleDone {
			t.Fatalf("unexpected event in the middle: %v", ev.Kind)
		}
		out = append(out, ev.Cycle)
	}
	if len(out) != want {
		t.Fatalf("got %d cycles, want %d", len(out), want)
	}
	for i, c := range out {
		if c.Number != i+1 {
			t.Fatalf("cycle %d numbered %d", i+1, c.Number)
		}
	}
	return out
}

func kinds(events []Event) []EventKind {
	k := make([]EventKind, len(events))
	for i, ev := range events {
		k[i] = ev.Kind
	}
	return k
}

// expectReachedAtOne is the loopback truth for every protocol: one hop, the
// target itself, answering every probe.
func expectReachedAtOne(t *testing.T, cs []*Cycle, target string) {
	t.Helper()
	for _, c := range cs {
		if c.ReachedAt != 1 {
			t.Fatalf("cycle %d: reached at %d, want 1", c.Number, c.ReachedAt)
		}
		if len(c.Hops) != 1 {
			t.Fatalf("cycle %d: %d hops shown, want 1", c.Number, len(c.Hops))
		}
		h := c.Hops[0]
		if h.TTL != 1 || h.Sample == nil || h.Received != c.Number || h.LossPct != 0 {
			t.Fatalf("cycle %d hop: %+v", c.Number, h)
		}
		if len(h.Addresses) != 1 || h.Addresses[0].Addr.String() != target {
			t.Fatalf("cycle %d addresses: %+v", c.Number, h.Addresses)
		}
	}
}

func TestLoopbackICMP4(t *testing.T) {
	e := testEngine(t)
	cs := cycles(t, runTrace(t, e, fastSpec("127.0.0.1")), 2)
	expectReachedAtOne(t, cs, "127.0.0.1")
}

func TestLoopbackICMP6(t *testing.T) {
	e := testEngine(t)
	if !e.Capabilities().IPv6 {
		t.Skip("no IPv6")
	}
	cs := cycles(t, runTrace(t, e, fastSpec("::1")), 2)
	expectReachedAtOne(t, cs, "::1")
}

func TestLoopbackUDPClosedPort(t *testing.T) {
	e := testEngine(t)
	spec := fastSpec("127.0.0.1")
	spec.Protocol = UDP
	cs := cycles(t, runTrace(t, e, spec), 2)
	expectReachedAtOne(t, cs, "127.0.0.1")
}

func TestLoopbackUDPOpenPort(t *testing.T) {
	e := testEngine(t)
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := srv.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = srv.WriteToUDP(buf[:n], from)
		}
	}()
	spec := fastSpec("127.0.0.1")
	spec.Protocol = UDP
	spec.Port = uint16(srv.LocalAddr().(*net.UDPAddr).Port)
	cs := cycles(t, runTrace(t, e, spec), 2)
	expectReachedAtOne(t, cs, "127.0.0.1")
}

func TestLoopbackTCPRefused(t *testing.T) {
	e := testEngine(t)
	// A port nothing listens on: pick one by binding and releasing it.
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	spec := fastSpec("127.0.0.1")
	spec.Protocol = TCP
	spec.Port = uint16(port)
	cs := cycles(t, runTrace(t, e, spec), 2)
	expectReachedAtOne(t, cs, "127.0.0.1")
}

func TestLoopbackTCPOpen(t *testing.T) {
	e := testEngine(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	spec := fastSpec("127.0.0.1")
	spec.Protocol = TCP
	spec.Port = uint16(l.Addr().(*net.TCPAddr).Port)
	cs := cycles(t, runTrace(t, e, spec), 2)
	expectReachedAtOne(t, cs, "127.0.0.1")
}

func TestLoopbackTCP6(t *testing.T) {
	e := testEngine(t)
	if !e.Capabilities().IPv6 {
		t.Skip("no IPv6")
	}
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no ::1: %v", err)
	}
	defer l.Close()
	spec := fastSpec("::1")
	spec.Protocol = TCP
	spec.Port = uint16(l.Addr().(*net.TCPAddr).Port)
	cs := cycles(t, runTrace(t, e, spec), 2)
	expectReachedAtOne(t, cs, "::1")
}

func TestPingMode(t *testing.T) {
	e := testEngine(t)
	spec := fastSpec("127.0.0.1")
	spec.Mode = Ping
	cs := cycles(t, runTrace(t, e, spec), 2)
	for _, c := range cs {
		if len(c.Hops) != 1 || c.Hops[0].TTL != DefaultPingTTL || c.Hops[0].Sample == nil {
			t.Fatalf("ping cycle: %+v", c)
		}
	}
}

func TestUnroutableTargetIsLoss(t *testing.T) {
	e := testEngine(t)
	// Inside the test namespace there is no route anywhere but loopback, so
	// every send fails with ENETUNREACH. That is loss, not a failed run.
	// On a host with a default route this target is a documentation-range
	// blackhole and simply times out, which is the same outcome.
	spec := fastSpec("192.0.2.1")
	spec.MaxTTL = 3
	cs := cycles(t, runTrace(t, e, spec), 2)
	last := cs[len(cs)-1]
	if last.ReachedAt != 0 || len(last.Hops) != 3 {
		t.Fatalf("blackhole cycle: %+v", last)
	}
	for _, h := range last.Hops {
		if h.Sample != nil || h.Received != 0 || h.Sent != 2 {
			t.Fatalf("blackhole hop: %+v", h)
		}
	}
	if last.Hops[0].LossPct != 100 {
		t.Fatalf("loss %v, want 100 once every probe has settled", last.Hops[0].LossPct)
	}
}

func TestCancel(t *testing.T) {
	e := testEngine(t)
	spec := fastSpec("127.0.0.1")
	spec.Cycles = 1000
	ctx, cancel := context.WithCancel(context.Background())
	var events []Event
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, spec, func(ev Event) { events = append(events, ev) }) }()
	time.Sleep(400 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after cancel")
	}
	last := events[len(events)-1]
	if last.Kind != Finished || !last.Cancelled {
		t.Fatalf("last event %+v", last)
	}
	if n := len(events); n < 3 || n > 8 {
		t.Fatalf("%d events for a 400ms run at 150ms interval", n)
	}
	// Nothing left registered on the engine.
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.probes) != 0 || len(e.ids) != 0 {
		t.Fatalf("leaked: %d probes, %d ids", len(e.probes), len(e.ids))
	}
}

func TestConcurrentRunsShareTheSockets(t *testing.T) {
	e := testEngine(t)
	const n = 5
	errs := make(chan error, n)
	for range n {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var bad error
			err := e.Run(ctx, fastSpec("127.0.0.1"), func(ev Event) {
				if ev.Kind == CycleDone && (ev.Cycle.ReachedAt != 1 || ev.Cycle.Hops[0].Sample == nil) {
					bad = errFmt("cycle %d not answered: %+v", ev.Cycle.Number, ev.Cycle.Hops[0])
				}
			})
			if err == nil {
				err = bad
			}
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
