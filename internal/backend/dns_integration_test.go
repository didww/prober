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
	"github.com/didww/prober/internal/dns/dnstest"
	"github.com/didww/prober/internal/trace"
)

// connectAgent serves gw over TLS on loopback and runs a real agent against it
// as site "testsite", with the DNS tool pointed at nameservers, returning once
// the agent has registered.
func connectAgent(t *testing.T, gw *backend.Gateway, nameservers []string) {
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

	acfg := agent.Config{
		Backend: agent.BackendConfig{
			Address:            ln.Addr().String(),
			Site:               "testsite",
			Token:              "shared",
			InsecureSkipVerify: true,
		},
		DNS: agent.DNSConfig{Nameservers: nameservers},
	}
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
// agent whose DNS tool is pointed at a fake nameserver on loopback, and checks
// the answers that come back over SSE: codes, records, TTLs, SRV target
// addresses, and the answering server. Like the monitoring test it needs raw
// sockets to build the agent, so it skips outside `unshare -Urn`.
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
	ns := dnstest.Start(t, map[string]dnstest.Answer{
		"A example.test":             {Addrs: []string{"192.0.2.1"}},
		"AAAA example.test":          {},
		"SRV _sip._udp.example.test": {SRV: []dnstest.SRV{{Target: "sip.example.test", Priority: 10, Weight: 5, Port: 5060}}},
		"A sip.example.test":         {Addrs: []string{"192.0.2.2"}},
	})
	gw := backend.NewGateway(backend.Config{AgentToken: "shared"}, log)
	connectAgent(t, gw, []string{ns.Addr})

	mgr := backend.NewManager(gw, time.Minute)
	api := httptest.NewServer(backend.NewAPI(gw, mgr, nil, log, "test", "abc").Routes())
	defer api.Close()

	resp, err := http.Post(api.URL+"/dns-runs", "application/json", strings.NewReader(`{"name":"Example.TEST.","timeout_ms":5000}`))
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
	if st := byType["started"][0]; st["site"] != "testsite" || st["target"] != "example.test" {
		t.Fatalf("started: %v", st)
	}
	if nss, _ := byType["started"][0]["nameservers"].([]any); len(nss) != 1 || nss[0] != ns.Addr {
		t.Fatalf("started nameservers: %v", byType["started"][0]["nameservers"])
	}
	if n := len(byType["finished"]); n != 1 || byType["finished"][0]["reason"] != "REASON_COMPLETED" {
		t.Fatalf("finished: %v", byType["finished"])
	}

	// Every question is answered exactly once, by the fake nameserver.
	want := map[string]bool{
		"A example.test":              false,
		"AAAA example.test":           false,
		"SRV _sip._udp.example.test":  false,
		"SRV _sip._tcp.example.test":  false,
		"SRV _sip._tls.example.test":  false,
		"SRV _sips._tcp.example.test": false,
	}
	byKey := map[string]map[string]any{}
	for _, r := range byType["dns_result"] {
		key := r["record_type"].(string) + " " + r["name"].(string)
		seen, known := want[key]
		if !known || seen {
			t.Fatalf("unexpected or repeated result %q: %v", key, r)
		}
		want[key] = true
		byKey[key] = r
		if _, ok := r["records"].([]any); !ok {
			t.Fatalf("%s: records is not an array: %v", key, r)
		}
		if r["error"] != "" || r["server"] != ns.Addr {
			t.Fatalf("%s: not answered by the fake nameserver: %v", key, r)
		}
	}
	rec := func(r map[string]any, i int) map[string]any { return r["records"].([]any)[i].(map[string]any) }
	if a := byKey["A example.test"]; a["status"] != "NOERROR" || len(a["records"].([]any)) != 1 || rec(a, 0)["value"] != "192.0.2.1" || rec(a, 0)["ttl"] != 300.0 {
		t.Errorf("A: %v", a)
	}
	if aaaa := byKey["AAAA example.test"]; aaaa["status"] != "NODATA" || len(aaaa["records"].([]any)) != 0 {
		t.Errorf("AAAA: %v", aaaa)
	}
	if srv := byKey["SRV _sip._udp.example.test"]; srv["status"] != "NOERROR" || len(srv["records"].([]any)) != 1 {
		t.Errorf("SRV udp: %v", srv)
	} else if r := rec(srv, 0); r["value"] != "sip.example.test" || r["port"] != 5060.0 || r["priority"] != 10.0 || r["address_status"] != "" {
		t.Errorf("SRV udp record: %v", r)
	} else if addrs, _ := r["addresses"].([]any); len(addrs) != 1 || addrs[0] != "192.0.2.2" {
		t.Errorf("SRV udp target addresses: %v", r["addresses"])
	}
	if srv := byKey["SRV _sip._tcp.example.test"]; srv["status"] != "NXDOMAIN" || len(srv["records"].([]any)) != 0 {
		t.Errorf("SRV tcp: %v", srv)
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("no result for %s; events: %v", key, events)
		}
	}
}
