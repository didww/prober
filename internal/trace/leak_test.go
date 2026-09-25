package trace

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/didww/prober/internal/leaktest"
)

func (e *Engine) registered() (probes, ids int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.probes), len(e.ids)
}

// deadTarget is the address that never answers: on the dummy interface the
// test runner sets up (see the Makefile's test-trace). It is probed only
// when 10.99.0.1 is a local address, which is that interface; on a machine
// where 10.99.0.0/24 might be a real network the half is skipped instead.
const deadTarget = "10.99.0.2"

func haveDummyInterface() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(net.IPv4(10, 99, 0, 1)) {
			return true
		}
	}
	return false
}

// TestEngineReleasesEverything runs ping sessions the way a monitor tick
// does, many times, and checks that the probe registry, the identifier
// table, goroutines and live heap all return to where they started: against
// loopback, where every probe is answered, and against an address that never
// answers, where every probe expires as lost.
func TestEngineReleasesEverything(t *testing.T) {
	leaktest.SkipUnlessEnabled(t)
	e, err := New(nil)
	if err != nil {
		t.Skipf("raw sockets unavailable: %v", err)
	}
	defer e.Close()

	ping := func(t *testing.T, target string, timeout time.Duration) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		spec := Spec{Target: netip.MustParseAddr(target), Protocol: ICMP, Mode: Ping, Cycles: 5, Interval: 5 * time.Millisecond, ProbeTimeout: timeout}
		if err := e.Run(ctx, spec, func(Event) {}); err != nil {
			t.Fatalf("run to %s: %v", target, err)
		}
	}
	registries := func(t *testing.T, what string, runs int) {
		t.Helper()
		if probes, ids := e.registered(); probes != 0 || ids != 0 {
			t.Errorf("%s: %d probes and %d identifiers still registered after %d runs", what, probes, ids, runs)
		}
	}
	const perRunBudget = 256

	t.Run("answered", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			ping(t, "127.0.0.1", time.Second)
		}
		base := leaktest.Mark(200 * time.Millisecond)
		const runs = 300
		for i := 0; i < runs; i++ {
			ping(t, "127.0.0.1", time.Second)
		}
		base.Check(t, "answered", runs, perRunBudget)
		registries(t, "answered", runs)
	})

	t.Run("unanswered", func(t *testing.T) {
		if !haveDummyInterface() {
			t.Skip("no dummy interface for 10.99.0.0/24 (make test-trace sets one up)")
		}
		for i := 0; i < 5; i++ {
			ping(t, deadTarget, 20*time.Millisecond)
		}
		base := leaktest.Mark(200 * time.Millisecond)
		const runs = 200
		for i := 0; i < runs; i++ {
			ping(t, deadTarget, 20*time.Millisecond)
		}
		base.Check(t, "unanswered", runs, perRunBudget)
		registries(t, "unanswered", runs)
	})
}
