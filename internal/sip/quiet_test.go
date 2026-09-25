package sip

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the library's goroutines to log
// into while a test reads it. A leak test discards while it runs, so it
// measures the engine's memory and not the log of it.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	discard atomic.Bool
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	if b.discard.Load() {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) SetDiscard(on bool) { b.discard.Store(on) }

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// libraryLog collects everything sipgo logs during the package's tests. The
// logger is installed once here, before any test starts a server or a
// probe, because sipgo's default logger is a plain global that must be set
// before the library runs anything.
var libraryLog syncBuffer

func TestMain(m *testing.M) {
	UseLogger(slog.New(slog.NewTextHandler(&libraryLog, &slog.HandlerOptions{Level: slog.LevelDebug})))
	os.Exit(m.Run())
}

func TestQuietHandlerDropsOnlyTheRefWarning(t *testing.T) {
	var buf syncBuffer
	log := slog.New(&quietHandler{Handler: slog.NewTextHandler(&buf, nil)})
	log.Warn("UDP ref went negative on try close", "src", "0.0.0.0:1", "ref", -1)
	log.Warn("UDP ref went negative", "ref", -1)
	log.With("tx", "x").Warn("TCP ref went negative", "ref", -1)
	log.WithGroup("g").Warn("WS ref went negative", "ref", -1)
	log.Warn("connection pool not clean cleanup", "error", "boom")
	log.With("tx", "x").Info("Client transaction destroyed")
	out := buf.String()
	if strings.Contains(out, "went negative") {
		t.Errorf("reference warning not dropped:\n%s", out)
	}
	if !strings.Contains(out, "connection pool not clean cleanup") || !strings.Contains(out, "Client transaction destroyed") {
		t.Errorf("other records lost:\n%s", out)
	}
}

// A real probe run against a local server: sipgo's output arrives in the
// logger we installed, in our format, without the reference warning.
func TestUseLoggerCoversAProbeRun(t *testing.T) {
	addr, got := startServer(t, "udp", 200, "OK")
	evs := run(t, Spec{Target: addr.Addr(), Port: addr.Port(), Transport: UDP, Cycles: 2, Interval: 10 * time.Millisecond})
	if rs := results(t, evs); len(rs) != 2 || !rs[0].Responded || got.Load() != 2 {
		t.Fatalf("probe run: %d results, %d received", len(rs), got.Load())
	}
	// Give the closed socket's reader goroutine, where the warning comes
	// from, a moment to run its cleanup.
	time.Sleep(50 * time.Millisecond)
	out := libraryLog.String()
	if strings.Contains(out, "went negative") {
		t.Errorf("reference warning reached the log:\n%s", out)
	}
	if out == "" {
		t.Error("sipgo wrote nothing through the installed logger; is the hook still in place?")
	}
}
