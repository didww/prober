package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/vlog"
)

func sinkLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestSink(t *testing.T, mons []MonitorConfig, vl *vlog.Client) *MonitorSink {
	t.Helper()
	reg := NewMonitorRegistry(mons)
	if vl == nil {
		vl = vlog.New(vlog.Options{}, sinkLogger()) // disabled
	}
	return NewMonitorSink(reg, vl, prometheus.NewRegistry())
}

func TestSinkTraceMetrics(t *testing.T) {
	s := newTestSink(t, []MonitorConfig{
		{ID: "t1", Kind: "trace", Target: "1.2.3.4", IntervalS: 10, Trace: &TraceParams{Protocol: "icmp"}},
	}, nil)

	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{
		Started: &pb.JobStarted{Resolved: "1.2.3.4", Protocol: pb.Protocol_PROTOCOL_ICMP},
	}})
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 1, Hops: []*pb.Hop{{
			Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4"}},
			Sent: 3, Received: 3, LossPct: 0, AvgUs: 12000,
		}}},
	}})

	lv := []string{"fra", "t1", "trace", "1.2.3.4", "icmp"}
	if v := testutil.ToFloat64(s.probesTotal.WithLabelValues(lv...)); v != 1 {
		t.Errorf("probes_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(s.successTotal.WithLabelValues(lv...)); v != 1 {
		t.Errorf("success_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(s.up.WithLabelValues(lv...)); v != 1 {
		t.Errorf("up = %v, want 1", v)
	}
	if v := testutil.ToFloat64(s.lossRatio.WithLabelValues(lv...)); v != 0 {
		t.Errorf("loss_ratio = %v, want 0", v)
	}
}

func TestSinkTraceUnreachable(t *testing.T) {
	s := newTestSink(t, []MonitorConfig{
		{ID: "t1", Kind: "trace", Target: "9.9.9.9", IntervalS: 10, Trace: &TraceParams{Protocol: "icmp"}},
	}, nil)
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{
		Started: &pb.JobStarted{Resolved: "9.9.9.9"},
	}})
	// A cycle whose last hop is not the target (target never answered).
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 1, Hops: []*pb.Hop{{
			Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1"}}, Sent: 3, Received: 0, LossPct: 100,
		}}},
	}})
	lv := []string{"fra", "t1", "trace", "9.9.9.9", "icmp"}
	if v := testutil.ToFloat64(s.up.WithLabelValues(lv...)); v != 0 {
		t.Errorf("up = %v, want 0 (unreachable)", v)
	}
	if v := testutil.ToFloat64(s.successTotal.WithLabelValues(lv...)); v != 0 {
		t.Errorf("success_total = %v, want 0", v)
	}
	if v := testutil.ToFloat64(s.lossRatio.WithLabelValues(lv...)); v != 1 {
		t.Errorf("loss_ratio = %v, want 1", v)
	}
}

func TestSinkSipMetrics(t *testing.T) {
	s := newTestSink(t, []MonitorConfig{
		{ID: "s1", Kind: "sip", Target: "5.5.5.5", IntervalS: 10, SIP: &SipParams{Transport: "udp"}},
	}, nil)
	rtt := uint32(30000)
	s.OnMonitorEvent("ams", &pb.JobEvent{JobId: "mon:s1", Event: &pb.JobEvent_SipResult{
		SipResult: &pb.SipResult{Cycle: 1, StatusCode: 200, Responded: true, RttUs: &rtt},
	}})
	lv := []string{"ams", "s1", "sip", "5.5.5.5", "udp"}
	if v := testutil.ToFloat64(s.probesTotal.WithLabelValues(lv...)); v != 1 {
		t.Errorf("probes_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(s.successTotal.WithLabelValues(lv...)); v != 1 {
		t.Errorf("success_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(s.up.WithLabelValues(lv...)); v != 1 {
		t.Errorf("up = %v, want 1", v)
	}
	code := append(append([]string{}, lv...), "200")
	if v := testutil.ToFloat64(s.sipResponses.WithLabelValues(code...)); v != 1 {
		t.Errorf("sip_responses{code=200} = %v, want 1", v)
	}
}

func TestSinkInfoMetricCarriesLabels(t *testing.T) {
	s := newTestSink(t, []MonitorConfig{
		{ID: "s1", Kind: "sip", Target: "5.5.5.5", IntervalS: 10, Labels: map[string]string{"supplier": "acme"}},
	}, nil)
	s.OnMonitorEvent("ams", &pb.JobEvent{JobId: "mon:s1", Event: &pb.JobEvent_SipResult{
		SipResult: &pb.SipResult{Cycle: 1, StatusCode: 200, Responded: true},
	}})
	want := `
# HELP prober_monitor_info Monitor metadata (operator labels); value is always 1. Join on (site, monitor).
# TYPE prober_monitor_info gauge
prober_monitor_info{kind="sip",monitor="s1",site="ams",supplier="acme",target="5.5.5.5"} 1
`
	if err := testutil.CollectAndCompare(s, readerFrom(want), "prober_monitor_info"); err != nil {
		t.Fatalf("info metric mismatch: %v", err)
	}
}

func TestSinkShipsTraceToVictoriaLogs(t *testing.T) {
	// Capture what the sink ships for a trace cycle.
	recs := make(chan vlog.Record, 8)
	cl := newCapturingVL(t, recs)
	s := newTestSink(t, []MonitorConfig{
		{ID: "t1", Kind: "trace", Target: "1.2.3.4", IntervalS: 10, Labels: map[string]string{"supplier": "acme"}},
	}, cl)

	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{
		Started: &pb.JobStarted{Resolved: "1.2.3.4", Source: "10.0.0.9"},
	}})
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 2, Hops: []*pb.Hop{
			{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1"}}, Sent: 3, Received: 3, AvgUs: 1000},
			{Ttl: 2, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4"}}, Sent: 3, Received: 3, AvgUs: 12000},
		}},
	}})

	got := drain(t, recs, 2)
	if len(got) != 2 {
		t.Fatalf("shipped %d records, want 2 (one per hop)", len(got))
	}
	r := got[1]
	if r["hop_addr"] != "1.2.3.4" || r["monitor"] != "t1" || r["site"] != "fra" {
		t.Errorf("record fields wrong: %+v", r)
	}
	if r["supplier"] != "acme" {
		t.Errorf("custom label not shipped: %+v", r)
	}
	if r["avg_ms"] != 12.0 {
		t.Errorf("avg_ms = %v, want 12", r["avg_ms"])
	}
}

// --- test helpers -----------------------------------------------------------

func readerFrom(s string) io.Reader { return strings.NewReader(s) }

// captureServer starts an HTTP server that decodes posted NDJSON into ch.
func captureServer(t *testing.T, ch chan vlog.Record) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sc := bufio.NewScanner(strings.NewReader(string(body)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err == nil {
				ch <- vlog.Record(m)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newCapturingVL returns a running vlog client that forwards each shipped record
// to ch (decoded from the NDJSON it posts).
func newCapturingVL(t *testing.T, ch chan vlog.Record) *vlog.Client {
	t.Helper()
	cl := vlog.New(vlog.Options{URL: captureServer(t, ch), BatchMax: 1, Flush: 5 * time.Millisecond}, sinkLogger())
	ctx, cancel := context.WithCancel(context.Background())
	go cl.Run(ctx)
	t.Cleanup(cancel)
	return cl
}

func drain(t *testing.T, ch chan vlog.Record, n int) []vlog.Record {
	t.Helper()
	var out []vlog.Record
	deadline := time.After(2 * time.Second)
	for len(out) < n {
		select {
		case r := <-ch:
			out = append(out, r)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestSinkRetainDropsRemovedMonitors(t *testing.T) {
	reg := NewMonitorRegistry([]MonitorConfig{
		{ID: "keep", Kind: "sip", Target: "1.1.1.1", IntervalS: 10, SIP: &SipParams{Transport: "udp"}},
		{ID: "gone", Kind: "sip", Target: "2.2.2.2", IntervalS: 10, SIP: &SipParams{Transport: "udp"}},
	})
	s := NewMonitorSink(reg, vlog.New(vlog.Options{}, sinkLogger()), prometheus.NewRegistry())
	feed := func(id string) {
		s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:" + id, Event: &pb.JobEvent_SipResult{
			SipResult: &pb.SipResult{Cycle: 1, StatusCode: 200, Responded: true},
		}})
	}
	feed("keep")
	feed("gone")
	if c := testutil.CollectAndCount(s.probesTotal); c != 2 {
		t.Fatalf("probes series = %d, want 2", c)
	}

	// Reload without "gone", then retain: its series should be pruned.
	reg.Replace([]MonitorConfig{{ID: "keep", Kind: "sip", Target: "1.1.1.1", IntervalS: 10, SIP: &SipParams{Transport: "udp"}}})
	s.Retain()
	if c := testutil.CollectAndCount(s.probesTotal); c != 1 {
		t.Fatalf("probes series after retain = %d, want 1", c)
	}
}
