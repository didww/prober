package trace

import (
	"math"
	"net/netip"
	"testing"
	"time"
)

func TestHopStatsAggregates(t *testing.T) {
	var h hopStats
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")

	for range 5 {
		h.sendProbe()
	}
	h.reply(a, 10*time.Millisecond)
	h.reply(a, 20*time.Millisecond)
	h.reply(b, 30*time.Millisecond)
	h.lost()
	// One still in flight.

	if h.sent != 5 || h.received != 3 || h.inFlight != 1 {
		t.Fatalf("counts: sent=%d received=%d inFlight=%d", h.sent, h.received, h.inFlight)
	}
	if h.best != 10*time.Millisecond || h.worst != 30*time.Millisecond || h.last != 30*time.Millisecond {
		t.Fatalf("best/worst/last: %v %v %v", h.best, h.worst, h.last)
	}
	if got := h.avg(); got != 20*time.Millisecond {
		t.Fatalf("avg: %v", got)
	}
	// Sample stdev of 10, 20, 30 is 10.
	if got := h.stdev(); math.Abs(float64(got-10*time.Millisecond)) > float64(time.Microsecond) {
		t.Fatalf("stdev: %v", got)
	}
	// |20-10| and |30-20| average to 10.
	if got := h.jitter(); got != 10*time.Millisecond {
		t.Fatalf("jitter: %v", got)
	}
	// 4 settled, 3 received: 25%, the in-flight one not counted.
	if got := h.lossPct(); got != 25 {
		t.Fatalf("loss: %v", got)
	}
	if len(h.addrs) != 2 || h.addrs[0].Addr != a || h.addrs[0].Count != 2 || h.addrs[1].Addr != b || h.addrs[1].Count != 1 {
		t.Fatalf("addrs: %+v", h.addrs)
	}
}

func TestHopStatsEmpty(t *testing.T) {
	var h hopStats
	if h.lossPct() != 0 || h.avg() != 0 || h.stdev() != 0 || h.jitter() != 0 {
		t.Fatal("zero stats must report zeros")
	}
	h.sendProbe()
	if h.lossPct() != 0 {
		t.Fatal("a probe in flight is not loss")
	}
	h.lost()
	if h.lossPct() != 100 {
		t.Fatalf("one probe lost is 100%%, got %v", h.lossPct())
	}
}

func TestSpecNormalizeDefaults(t *testing.T) {
	s, err := Spec{Target: netip.MustParseAddr("::ffff:192.0.2.1")}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Target.Is4() {
		t.Fatalf("mapped address must be unmapped, got %v", s.Target)
	}
	if s.Protocol != ICMP || s.Mode != MTR || s.Cycles != DefaultCycles || s.MaxTTL != DefaultMaxTTL || s.FirstTTL != 1 || s.Port != 0 {
		t.Fatalf("defaults: %+v", s)
	}

	p, err := Spec{Target: netip.MustParseAddr("2001:db8::1"), Mode: Ping, Protocol: TCP}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if p.FirstTTL != DefaultPingTTL || p.MaxTTL != DefaultPingTTL || p.Port != DefaultTCPPort || p.hopCount() != 1 {
		t.Fatalf("ping defaults: %+v", p)
	}

	if _, err := (Spec{}).Normalize(); err == nil {
		t.Fatal("missing target must fail")
	}
	if _, err := (Spec{Target: netip.MustParseAddr("192.0.2.1"), Source: netip.MustParseAddr("2001:db8::1")}).Normalize(); err == nil {
		t.Fatal("mixed families must fail")
	}
	if _, err := (Spec{Target: netip.MustParseAddr("192.0.2.1"), DSCP: 64}).Normalize(); err == nil {
		t.Fatal("dscp 64 must fail")
	}
}
