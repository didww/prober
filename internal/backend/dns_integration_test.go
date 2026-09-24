package backend_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/agent"
	"github.com/didww/prober/internal/backend"
	"github.com/didww/prober/internal/trace"
)

// connectAgent serves gw over TLS on loopback and runs a real agent against it
// as site "testsite", returning once the agent has registered.
func connectAgent(t *testing.T, gw *backend.Gateway) {
	t.Helper()
	log := itLogger()

	cert := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	grpcSrv := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterAgentGatewayServer(grpcSrv, gw)
	go grpcSrv.Serve(ln)
	t.Cleanup(grpcSrv.Stop)

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
	t.Cleanup(func() { ag.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Run(ctx)

	if !waitFor(10*time.Second, func() bool { return gw.Connected("testsite") }) {
		t.Fatal("agent never connected")
	}
}

// readSSE reads a run's event stream until the backend signals done, returning
// the decoded data payloads in order.
func readSSE(t *testing.T, url string) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events: HTTP %d", resp.StatusCode)
	}

	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "event: done" {
			return out
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("bad SSE data %q: %v", data, err)
			}
			out = append(out, m)
		}
	}
	t.Fatalf("stream ended without done after %d events: %v", len(out), sc.Err())
	return nil
}

// TestDnsRunEndToEnd starts a DNS run through the HTTP API against a real
// agent and checks the events that come back over SSE. It resolves
// "localhost", which the hosts file answers, so it works without a network;
// the SRV lookups need a nameserver and are only checked to be reported. Like
// the monitoring test it needs raw sockets to build the agent, so it skips
// outside `unshare -Urn`.
func TestDnsRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	eng, err := trace.New(itLogger())
	if err != nil {
		t.Skipf("raw sockets unavailable (run under unshare -Urn): %v", err)
	}
	eng.Close()

	log := itLogger()
	gw := backend.NewGateway(backend.Config{AgentToken: "shared"}, log)
	connectAgent(t, gw)

	mgr := backend.NewManager(gw, time.Minute)
	api := httptest.NewServer(backend.NewAPI(gw, mgr, nil, log, "test", "abc").Routes())
	defer api.Close()

	// An address is refused up front: there is nothing to look up.
	resp, err := http.Post(api.URL+"/dns-runs", "application/json", strings.NewReader(`{"name":"127.0.0.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("address literal: HTTP %d, want 400", resp.StatusCode)
	}

	resp, err = http.Post(api.URL+"/dns-runs", "application/json", strings.NewReader(`{"name":"LocalHost.","timeout_ms":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		ID    string   `json:"id"`
		Sites []string `json:"sites"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || started.ID == "" || len(started.Sites) != 1 || started.Sites[0] != "testsite" {
		t.Fatalf("start: HTTP %d, %+v", resp.StatusCode, started)
	}

	events := readSSE(t, api.URL+"/runs/"+started.ID+"/events")

	byType := map[string][]map[string]any{}
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		byType[typ] = append(byType[typ], ev)
	}
	if errs := byType["error"]; len(errs) > 0 {
		t.Fatalf("error events: %v", errs)
	}
	if n := len(byType["started"]); n != 1 {
		t.Fatalf("got %d started events, want 1: %v", n, events)
	}
	if st := byType["started"][0]; st["site"] != "testsite" || st["target"] != "localhost" {
		t.Fatalf("started: %v", st)
	}
	if n := len(byType["finished"]); n != 1 || byType["finished"][0]["reason"] != "REASON_COMPLETED" {
		t.Fatalf("finished: %v", byType["finished"])
	}

	// Every question is answered exactly once, and the A answer is loopback.
	want := map[string]bool{
		"A localhost":              false,
		"AAAA localhost":           false,
		"SRV _sip._udp.localhost":  false,
		"SRV _sip._tcp.localhost":  false,
		"SRV _sip._tls.localhost":  false,
		"SRV _sips._tcp.localhost": false,
	}
	for _, r := range byType["dns_result"] {
		key := r["record_type"].(string) + " " + r["name"].(string)
		seen, known := want[key]
		if !known || seen {
			t.Fatalf("unexpected or repeated result %q: %v", key, r)
		}
		want[key] = true
		if _, ok := r["records"].([]any); !ok {
			t.Fatalf("%s: records is not an array: %v", key, r)
		}
		if _, ok := r["error"].(string); !ok {
			t.Fatalf("%s: error is not a string: %v", key, r)
		}
		if key == "A localhost" {
			recs := r["records"].([]any)
			if r["error"] != "" || len(recs) == 0 || recs[0].(map[string]any)["value"] != "127.0.0.1" {
				t.Fatalf("A localhost: %v", r)
			}
		}
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("no result for %s; events: %v", key, events)
		}
	}
}
