package backend_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/agent"
	"github.com/didww/prober/internal/backend"
	"github.com/didww/prober/internal/trace"
	"github.com/didww/prober/internal/vlog"
)

func itLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestMonitoringEndToEnd wires a real gateway, a real agent, and a fake
// VictoriaLogs together and checks that self-scheduled monitors produce metrics
// and log records, and that a reload re-pushes a changed assignment. It needs
// raw sockets (loopback ICMP), so it skips outside `unshare -Urn` (the Makefile's
// test-trace environment).
func TestMonitoringEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	eng, err := trace.New(itLogger())
	if err != nil {
		t.Skipf("raw sockets unavailable (run under unshare -Urn): %v", err)
	}
	eng.Close()

	log := itLogger()

	// Fake VictoriaLogs: decode NDJSON into a channel.
	vlRecs := make(chan map[string]any, 64)
	vlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sc := bufio.NewScanner(strings.NewReader(string(body)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				select {
				case vlRecs <- m:
				default:
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer vlSrv.Close()

	// Backend pieces: registry with a ping + a trace monitor to loopback.
	promReg := prometheus.NewRegistry()
	reg := backend.NewMonitorRegistry([]backend.MonitorConfig{
		{ID: "ping-lo", Kind: "ping", Target: "127.0.0.1", IntervalS: 1, Trace: &backend.TraceParams{Protocol: "icmp", Cycles: 2}},
		{ID: "mtr-lo", Kind: "trace", Target: "127.0.0.1", IntervalS: 1, Trace: &backend.TraceParams{Protocol: "icmp", Cycles: 1}},
	})
	vl := vlog.New(vlog.Options{URL: vlSrv.URL, BatchMax: 1, Flush: 20 * time.Millisecond, StreamFields: []string{"site", "monitor"}}, log)
	vlCtx, vlCancel := context.WithCancel(context.Background())
	defer vlCancel()
	go vl.Run(vlCtx)
	sink := backend.NewMonitorSink(reg, vl, promReg)

	// Gateway over real TLS on loopback.
	gw := backend.NewGateway(backend.Config{AgentToken: "shared"}, log)
	gw.SetMonitoring(reg, sink)

	cert := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	grpcSrv := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterAgentGatewayServer(grpcSrv, gw)
	go grpcSrv.Serve(ln)
	defer grpcSrv.Stop()

	// Agent dialing the gateway, trusting it via insecure_skip_verify (the point
	// under test is monitoring, not TLS trust).
	acfg := agent.Config{Backend: agent.BackendConfig{
		Address:            ln.Addr().String(),
		Site:               "testsite",
		Token:              "shared",
		InsecureSkipVerify: true,
	}}
	ag, err := agent.New(acfg, log, agent.BuildInfo{Version: "test", Commit: "abc"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	defer ag.Close()
	agCtx, agCancel := context.WithCancel(context.Background())
	defer agCancel()
	go ag.Run(agCtx)

	metricsSrv := httptest.NewServer(promhttp.HandlerFor(promReg, promhttp.HandlerOpts{}))
	defer metricsSrv.Close()
	scrape := func() string {
		resp, err := http.Get(metricsSrv.URL)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// The ping monitor should report up=1 to loopback within a few seconds.
	if !waitFor(10*time.Second, func() bool {
		return strings.Contains(scrape(), `prober_monitor_up{kind="ping",monitor="ping-lo",site="testsite",target="127.0.0.1",transport="icmp"} 1`)
	}) {
		t.Fatalf("ping monitor never reported up=1.\nmetrics:\n%s", scrape())
	}

	// The trace monitor should ship its report to VictoriaLogs once the trace
	// completes: one record, the mtr text in _msg, loopback reached.
	if !waitForRecord(10*time.Second, vlRecs, func(m map[string]any) bool {
		msg, _ := m["_msg"].(string)
		return m["monitor"] == "mtr-lo" && m["site"] == "testsite" && m["reached"] == true &&
			strings.HasPrefix(msg, "Start: ") && strings.Contains(msg, "HOST: testsite") && strings.Contains(msg, "1.|-- 127.0.0.1")
	}) {
		t.Fatal("no VictoriaLogs report record for the trace monitor")
	}

	// Reload: add a second ping monitor, re-push, and confirm it starts too.
	reg.Replace([]backend.MonitorConfig{
		{ID: "ping-lo", Kind: "ping", Target: "127.0.0.1", IntervalS: 1, Trace: &backend.TraceParams{Protocol: "icmp", Cycles: 2}},
		{ID: "ping-lo2", Kind: "ping", Target: "127.0.0.1", IntervalS: 1, Trace: &backend.TraceParams{Protocol: "icmp", Cycles: 2}},
	})
	sink.Retain()
	gw.PushAssignments()

	if !waitFor(10*time.Second, func() bool {
		return strings.Contains(scrape(), `monitor="ping-lo2"`)
	}) {
		t.Fatalf("reloaded monitor ping-lo2 never appeared.\nmetrics:\n%s", scrape())
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func waitForRecord(d time.Duration, ch chan map[string]any, match func(map[string]any) bool) bool {
	deadline := time.After(d)
	for {
		select {
		case m := <-ch:
			if match(m) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "prober-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
