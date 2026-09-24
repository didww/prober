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
	"google.golang.org/protobuf/types/known/timestamppb"

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
		Cycle: &pb.Cycle{Number: 1, ReachedAt: 1, Hops: []*pb.Hop{{
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
	// Capture what the sink ships for a whole trace: nothing per cycle, one
	// record with the mtr report when the trace finishes.
	recs := make(chan vlog.Record, 8)
	cl := newCapturingVL(t, recs)
	s := newTestSink(t, []MonitorConfig{
		{ID: "t1", Kind: "trace", Target: "1.2.3.4", IntervalS: 10, Labels: map[string]string{"supplier": "acme"}},
	}, cl)

	startedAt := time.Date(2026, 9, 16, 20, 45, 12, 0, time.UTC)
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Time: timestamppb.New(startedAt), Event: &pb.JobEvent_Started{
		Started: &pb.JobStarted{Resolved: "1.2.3.4", Source: "10.0.0.9", Protocol: pb.Protocol_PROTOCOL_ICMP},
	}})
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 1, ReachedAt: 2, Hops: []*pb.Hop{
			{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1"}}, Sent: 1, Received: 1, AvgUs: 900},
			{Ttl: 2, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4"}}, Sent: 1, Received: 1, AvgUs: 11000},
		}},
	}})
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 2, ReachedAt: 2, Hops: []*pb.Hop{
			{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1"}}, Sent: 2, Received: 2, AvgUs: 1000},
			{Ttl: 2, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4", Name: "dst.example.net"}}, Sent: 2, Received: 2, AvgUs: 12000, BestUs: 11000, WorstUs: 13000},
		}},
	}})
	select {
	case r := <-recs:
		t.Fatalf("a record was shipped before the trace finished: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Finished{
		Finished: &pb.JobFinished{Reason: pb.JobFinished_REASON_COMPLETED},
	}})
	got := drain(t, recs, 1)
	expectNoMore(t, recs)
	if len(got) != 1 {
		t.Fatalf("shipped %d records, want 1 (one per trace)", len(got))
	}
	r := got[0]
	if r["monitor"] != "t1" || r["site"] != "fra" || r["target"] != "1.2.3.4" || r["resolved"] != "1.2.3.4" || r["source"] != "10.0.0.9" || r["protocol"] != "icmp" {
		t.Errorf("record fields wrong: %+v", r)
	}
	if r["supplier"] != "acme" {
		t.Errorf("custom label not shipped: %+v", r)
	}
	// The destination hop's aggregates from the last cycle are fields.
	if r["cycles"] != 2.0 || r["hops"] != 2.0 || r["reached"] != true || r["sent"] != 2.0 || r["received"] != 2.0 || r["loss_pct"] != 0.0 {
		t.Errorf("summary fields wrong: %+v", r)
	}
	if r["avg_ms"] != 12.0 || r["best_ms"] != 11.0 || r["worst_ms"] != 13.0 {
		t.Errorf("rtt fields wrong: %+v", r)
	}
	if _, has := r["error"]; has {
		t.Errorf("a completed trace carries no error: %+v", r)
	}
	// The message is the full report: both hops, the last cycle's numbers.
	msg, _ := r["_msg"].(string)
	lines := strings.Split(msg, "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "Start: 2026-09-16T") || !strings.HasPrefix(lines[1], "HOST: fra (10.0.0.9)") {
		t.Fatalf("report header wrong:\n%s", msg)
	}
	if !strings.Contains(lines[2], "1.|-- 10.0.0.1") || !strings.Contains(lines[2], "0.0%     2     2") {
		t.Errorf("hop 1 row wrong: %q", lines[2])
	}
	if !strings.Contains(lines[3], "2.|-- dst.example.net") || !strings.Contains(lines[3], "  12.0  11.0  13.0") {
		t.Errorf("hop 2 row wrong: %q", lines[3])
	}
}

func TestSinkShipsPartialTraceOnError(t *testing.T) {
	recs := make(chan vlog.Record, 8)
	cl := newCapturingVL(t, recs)
	s := newTestSink(t, []MonitorConfig{
		{ID: "t1", Kind: "trace", Target: "9.9.9.9", IntervalS: 10},
	}, cl)

	// A trace that fails before any cycle ships nothing.
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Error{
		Error: &pb.JobError{Code: pb.JobError_CODE_RESOLVE, Message: "no such host"},
	}})
	expectNoMore(t, recs)

	// One that got a cycle in and then hit an engine error ships what it has,
	// marked unreached, with the error attached.
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{
		Started: &pb.JobStarted{Resolved: "9.9.9.9"},
	}})
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 1, Hops: []*pb.Hop{
			{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1"}}, Sent: 1, Received: 1, AvgUs: 1000},
			{Ttl: 2, Sent: 1, Received: 0, LossPct: 100},
		}},
	}})
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Error{
		Error: &pb.JobError{Code: pb.JobError_CODE_ENGINE, Message: "socket closed"},
	}})
	got := drain(t, recs, 1)
	expectNoMore(t, recs)
	if len(got) != 1 {
		t.Fatalf("shipped %d records, want 1", len(got))
	}
	r := got[0]
	if r["error"] != "socket closed" || r["reached"] != false || r["loss_pct"] != 100.0 || r["hops"] != 2.0 {
		t.Errorf("partial record wrong: %+v", r)
	}
	if _, has := r["avg_ms"]; has {
		t.Errorf("an unreached target has no RTT: %+v", r)
	}
	if msg, _ := r["_msg"].(string); !strings.Contains(msg, "2.|-- ???") {
		t.Errorf("unanswered hop not in report:\n%s", msg)
	}
}

func TestSinkDropsCancelledAndStaleTraces(t *testing.T) {
	recs := make(chan vlog.Record, 8)
	cl := newCapturingVL(t, recs)
	s := newTestSink(t, []MonitorConfig{
		{ID: "t1", Kind: "trace", Target: "1.2.3.4", IntervalS: 10},
	}, cl)
	cycle := func() *pb.JobEvent {
		return &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
			Cycle: &pb.Cycle{Number: 1, ReachedAt: 1, Hops: []*pb.Hop{
				{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4"}}, Sent: 1, Received: 1, AvgUs: 1000},
			}},
		}}
	}

	// Cancelled mid-run (a reload, an agent shutdown): partial, so not a report.
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{Started: &pb.JobStarted{Resolved: "1.2.3.4"}}})
	s.OnMonitorEvent("fra", cycle())
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Finished{
		Finished: &pb.JobFinished{Reason: pb.JobFinished_REASON_CANCELLED},
	}})
	expectNoMore(t, recs)

	// A run whose end never arrived (the agent's stream was replaced), then a
	// resolve failure on the next tick: the stale cycle must not be shipped
	// under that error.
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{Started: &pb.JobStarted{Resolved: "1.2.3.4"}}})
	s.OnMonitorEvent("fra", cycle())
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Error{
		Error: &pb.JobError{Code: pb.JobError_CODE_RESOLVE, Message: "no such host"},
	}})
	expectNoMore(t, recs)

	// The page shows the failure as a full loss, not a healthy-looking 0%.
	st := s.AllStatuses()["t1"]
	if len(st) != 1 || st[0].Up || st[0].LossPct != 100 || st[0].Error != "no such host" {
		t.Fatalf("status after resolve failure: %+v", st)
	}

	// A clean run afterwards ships as usual.
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{Started: &pb.JobStarted{Resolved: "1.2.3.4"}}})
	s.OnMonitorEvent("fra", cycle())
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Finished{
		Finished: &pb.JobFinished{Reason: pb.JobFinished_REASON_COMPLETED},
	}})
	if got := drain(t, recs, 1); len(got) != 1 || got[0]["reached"] != true {
		t.Fatalf("completed run: %+v", got)
	}
}

// --- test helpers -----------------------------------------------------------

// expectNoMore fails if anything else is shipped within a short grace period.
func expectNoMore(t *testing.T, ch chan vlog.Record) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("unexpected record shipped: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
}

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
