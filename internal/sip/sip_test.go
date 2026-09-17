package sip

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// startServer runs an in-process SIP server that answers OPTIONS with the given
// code over the given network ("udp"/"tcp"), returning its host:port and a
// counter of received OPTIONS.
func startServer(t *testing.T, network string, code int, reason string) (netip.AddrPort, *atomic.Int64) {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("test-srv"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	var got atomic.Int64
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		got.Add(1)
		_ = tx.Respond(sip.NewResponseFromRequest(req, code, reason, nil))
	})

	var addr string
	if network == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = pc.LocalAddr().String()
		go srv.ServeUDP(pc)
	} else {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = l.Addr().String()
		go srv.ServeTCP(l)
	}
	t.Cleanup(func() { ua.Close() })

	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the listener come up
	return ap, &got
}

func run(t *testing.T, spec Spec) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var evs []Event
	log := slog.New(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Run(ctx, spec, log, func(e Event) { evs = append(evs, e) }); err != nil {
		t.Fatalf("run: %v", err)
	}
	return evs
}

func results(t *testing.T, evs []Event) []*Result {
	t.Helper()
	if len(evs) < 2 || evs[0].Kind != Started || evs[len(evs)-1].Kind != Finished {
		t.Fatalf("bad envelope: %+v", evs)
	}
	var rs []*Result
	for _, e := range evs[1 : len(evs)-1] {
		if e.Kind != ResultDone {
			t.Fatalf("unexpected middle event %v", e.Kind)
		}
		rs = append(rs, e.Result)
	}
	return rs
}

func testOptions(t *testing.T, transport Transport, network string) {
	ap, got := startServer(t, network, 200, "OK")
	spec := Spec{
		Target:    ap.Addr(),
		Port:      ap.Port(),
		Transport: transport,
		Cycles:    3,
		Interval:  150 * time.Millisecond,
		Timeout:   2 * time.Second,
	}
	rs := results(t, run(t, spec))
	if len(rs) != 3 {
		t.Fatalf("got %d results, want 3", len(rs))
	}
	for i, r := range rs {
		if !r.Responded || r.StatusCode != 200 {
			t.Fatalf("cycle %d: responded=%v code=%d", i+1, r.Responded, r.StatusCode)
		}
		if r.RTT <= 0 || r.Request == "" || r.Response == "" {
			t.Fatalf("cycle %d: rtt=%v reqLen=%d respLen=%d", i+1, r.RTT, len(r.Request), len(r.Response))
		}
	}
	// Exactly one packet per cycle — no retransmission storm.
	if n := got.Load(); n != 3 {
		t.Fatalf("server received %d OPTIONS, want exactly 3 (one per cycle)", n)
	}
}

func TestOptionsUDP(t *testing.T) { testOptions(t, UDP, "udp") }
func TestOptionsTCP(t *testing.T) { testOptions(t, TCP, "tcp") }

func TestOptionsTimeout(t *testing.T) {
	// A UDP port with no server: every cycle times out, and each still sends
	// exactly one packet (no retransmit).
	spec := Spec{
		Target:    netip.MustParseAddr("127.0.0.1"),
		Port:      1, // nothing listening
		Transport: UDP,
		Cycles:    2,
		Interval:  100 * time.Millisecond,
		Timeout:   400 * time.Millisecond,
	}
	rs := results(t, run(t, spec))
	if len(rs) != 2 {
		t.Fatalf("got %d results, want 2", len(rs))
	}
	for i, r := range rs {
		if r.Responded || r.StatusCode != 0 || r.Request == "" {
			t.Fatalf("cycle %d should be a timeout with a captured request: %+v", i+1, r)
		}
	}
}

func TestNormalizeDefaults(t *testing.T) {
	s, err := Spec{Target: netip.MustParseAddr("192.0.2.1"), Transport: TLS}.normalize()
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != 5061 || s.Interval != DefaultInterval || s.Timeout != DefaultTimeout || s.UserAgent != DefaultUserAgent {
		t.Fatalf("defaults: %+v", s)
	}
	if _, err := (Spec{}).normalize(); err == nil {
		t.Fatal("missing target must fail")
	}
}
