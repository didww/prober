package sip

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/didww/prober/internal/leaktest"
)

// probeOver runs one single-cycle OPTIONS the way a monitor tick does.
func probeOver(t *testing.T, transport Transport, target netip.AddrPort, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Run(ctx, Spec{Target: target.Addr(), Port: target.Port(), Transport: transport, Cycles: 1, Timeout: timeout}, nil, func(Event) {}); err != nil {
		t.Fatal(err)
	}
}

// The heap budget per run. A leaked context is about a kilobyte; the
// engine's own per-run noise measures well under a hundred bytes.
const perRunBudget = 256

// TestRunReleasesEverything runs the engine the way the scheduler does, one
// user agent per tick, many times, and checks that goroutines and live heap
// return to where they started: on the answered path, and on the timeout
// path, which is what a dead peer produces in production.
func TestRunReleasesEverything(t *testing.T) {
	leaktest.SkipUnlessEnabled(t)
	libraryLog.SetDiscard(true)
	defer libraryLog.SetDiscard(false)

	answering := startRawResponder(t)
	silent := startSilentUDP(t)

	// Warm every path once so first-time allocations do not count.
	for i := 0; i < 50; i++ {
		probeOver(t, UDP, answering, time.Second)
	}
	probeOver(t, UDP, silent, 20*time.Millisecond)
	base := leaktest.Mark(200 * time.Millisecond)

	const answered, timedOut = 2000, 300
	for i := 0; i < answered; i++ {
		probeOver(t, UDP, answering, time.Second)
	}
	for i := 0; i < timedOut; i++ {
		probeOver(t, UDP, silent, 20*time.Millisecond)
	}
	base.Check(t, "udp", answered+timedOut, perRunBudget)
}

// TestRunReleasesEverythingOverTCP is the same check for the TCP transport,
// whose connections are stateful: an answering listener, one that accepts
// and never answers, and a port nobody listens on.
func TestRunReleasesEverythingOverTCP(t *testing.T) {
	leaktest.SkipUnlessEnabled(t)
	libraryLog.SetDiscard(true)
	defer libraryLog.SetDiscard(false)

	answering := startRawTCPResponder(t, true)
	silent := startRawTCPResponder(t, false)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := netip.MustParseAddrPort(l.Addr().String())
	l.Close()

	for i := 0; i < 20; i++ {
		probeOver(t, TCP, answering, time.Second)
	}
	probeOver(t, TCP, silent, 20*time.Millisecond)
	probeOver(t, TCP, refused, time.Second)
	base := leaktest.Mark(200 * time.Millisecond)

	const answered, timedOut, refusedRuns = 600, 150, 150
	for i := 0; i < answered; i++ {
		probeOver(t, TCP, answering, time.Second)
	}
	for i := 0; i < timedOut; i++ {
		probeOver(t, TCP, silent, 20*time.Millisecond)
	}
	for i := 0; i < refusedRuns; i++ {
		probeOver(t, TCP, refused, time.Second)
	}
	base.Check(t, "tcp", answered+timedOut+refusedRuns, perRunBudget)
}

// rawOK builds a 200 OK for an OPTIONS request by echoing the headers a
// response must carry, with no SIP stack involved, so nothing of the
// library's server side, which the agent never runs, is in the process.
func rawOK(req []byte) []byte {
	var hdrs []string
	for _, line := range strings.Split(string(req), "\r\n") {
		k, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "via", "from", "call-id", "cseq":
			hdrs = append(hdrs, line)
		case "to":
			hdrs = append(hdrs, line+";tag=raw")
		}
	}
	return []byte("SIP/2.0 200 OK\r\n" + strings.Join(hdrs, "\r\n") + "\r\nContent-Length: 0\r\n\r\n")
}

// startRawResponder answers every OPTIONS on a UDP socket with a 200 OK.
func startRawResponder(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(rawOK(buf[:n]), from)
		}
	}()
	return netip.MustParseAddrPort(pc.LocalAddr().String())
}

// startSilentUDP is a UDP socket that reads and never answers, so a probe
// waits its timeout out.
func startSilentUDP(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	return netip.MustParseAddrPort(pc.LocalAddr().String())
}

// startRawTCPResponder accepts connections and, when answer is set, replies
// to each OPTIONS with a 200 OK; otherwise it reads and stays silent.
func startRawTCPResponder(t *testing.T, answer bool) netip.AddrPort {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if !answer {
						continue
					}
					if _, err := c.Write(rawOK(buf[:n])); err != nil {
						return
					}
				}
			}()
		}
	}()
	return netip.MustParseAddrPort(l.Addr().String())
}
