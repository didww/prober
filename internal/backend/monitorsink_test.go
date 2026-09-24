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

	// Any final response is reachability: a 403 keeps the monitor up and
	// counts as success, while its code is still counted on its own.
	s.OnMonitorEvent("ams", &pb.JobEvent{JobId: "mon:s1", Event: &pb.JobEvent_SipResult{
		SipResult: &pb.SipResult{Cycle: 2, StatusCode: 403, Reason: "Forbidden", Responded: true, RttUs: &rtt},
	}})
	if v := testutil.ToFloat64(s.successTotal.WithLabelValues(lv...)); v != 2 {
		t.Errorf("success_total after 403 = %v, want 2", v)
	}
	if v := testutil.ToFloat64(s.up.WithLabelValues(lv...)); v != 1 {
		t.Errorf("up after 403 = %v, want 1", v)
	}
	code403 := append(append([]string{}, lv...), "403")
	if v := testutil.ToFloat64(s.sipResponses.WithLabelValues(code403...)); v != 1 {
		t.Errorf("sip_responses{code=403} = %v, want 1", v)
	}
	if st := s.Statuses("s1"); len(st) != 1 || !st[0].Up || st[0].Code != 403 {
		t.Errorf("status after 403: %+v", st)
	}

	// No response at all is the only failure.
	s.OnMonitorEvent("ams", &pb.JobEvent{JobId: "mon:s1", Event: &pb.JobEvent_SipResult{
		SipResult: &pb.SipResult{Cycle: 3, Responded: false},
	}})
	if v := testutil.ToFloat64(s.up.WithLabelValues(lv...)); v != 0 {
		t.Errorf("up after timeout = %v, want 0", v)
	}
	if v := testutil.ToFloat64(s.successTotal.WithLabelValues(lv...)); v != 2 {
		t.Errorf("success_total after timeout = %v, want 2", v)
	}
	timeout := append(append([]string{}, lv...), "timeout")
	if v := testutil.ToFloat64(s.sipResponses.WithLabelValues(timeout...)); v != 1 {
		t.Errorf("sip_responses{code=timeout} = %v, want 1", v)
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
	st := s.Statuses("t1")
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

func TestSinkAggregateAndStream(t *testing.T) {
	s := newTestSink(t, []MonitorConfig{
		{ID: "t1", Kind: "ping", Target: "1.2.3.4", IntervalS: 10},
		{ID: "t2", Kind: "ping", Target: "5.6.7.8", IntervalS: 10},
	}, nil)
	snapshot, _, changes, cancel := s.Subscribe()
	defer cancel()
	if len(snapshot) != 0 {
		t.Fatalf("snapshot before any result: %v", snapshot)
	}
	next := func() StatusEvent {
		t.Helper()
		select {
		case ev, ok := <-changes:
			if !ok {
				t.Fatal("stream closed")
			}
			return ev
		case <-time.After(2 * time.Second):
			t.Fatal("no status event")
		}
		return StatusEvent{}
	}
	none := func() {
		t.Helper()
		select {
		case ev := <-changes:
			t.Fatalf("unexpected status event: %+v", ev)
		case <-time.After(50 * time.Millisecond):
		}
	}
	cycle := func(id, site string, reached bool) {
		c := &pb.Cycle{Number: 1, Hops: []*pb.Hop{{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4"}}, Sent: 3, Received: 3, AvgUs: 1000}}}
		if reached {
			c.ReachedAt = 1
		} else {
			c.Hops[0].Received, c.Hops[0].LossPct = 0, 100
		}
		s.OnMonitorEvent(site, &pb.JobEvent{JobId: "mon:" + id, Event: &pb.JobEvent_Cycle{Cycle: c}})
	}

	// One site up: UP. A second site down: PARTIAL. The same again: no
	// event, nothing changed.
	cycle("t1", "fra", true)
	if ev := next(); ev.ID != "t1" || ev.Aggregate.State != StateUp || ev.Aggregate.Up != 1 || ev.Aggregate.Sites != 1 || ev.Aggregate.Since.IsZero() {
		t.Fatalf("first result: %+v", ev)
	}
	cycle("t1", "ams", false)
	ev := next()
	if ev.Aggregate.State != StatePartial || ev.Aggregate.Up != 1 || ev.Aggregate.Down != 1 {
		t.Fatalf("second site down: %+v", ev)
	}
	partialSince := ev.Aggregate.Since
	cycle("t1", "ams", false)
	none()

	// Both down: DOWN, with a new Since. Since holds while the state does.
	cycle("t1", "fra", false)
	ev = next()
	if ev.Aggregate.State != StateDown || ev.Aggregate.Down != 2 || !ev.Aggregate.Since.After(partialSince) {
		t.Fatalf("both down: %+v", ev)
	}
	downSince := ev.Aggregate.Since
	if got, ok := s.Aggregate("t1"); !ok || got.State != StateDown || !got.Since.Equal(downSince) {
		t.Fatalf("aggregate: %+v", got)
	}

	// Sites go stale with time, which only a sweep notices, and a sweep
	// reports everything it moved in one batch. A sweep that changes nothing
	// publishes nothing.
	cycle("t1", "fra", true)
	if ev = next(); ev.Aggregate.State != StatePartial {
		t.Fatalf("fra back up: %+v", ev)
	}
	s.Sweep(time.Now())
	none()
	s.Sweep(time.Now().Add(2 * time.Minute))
	// Both outcomes are now older than staleAfter (max(30s, 1m)).
	if ev = next(); ev.ID != "" || ev.Changes["t1"].State != StateNoData || ev.Changes["t1"].Stale != 2 {
		t.Fatalf("after a stale sweep: %+v", ev)
	}
	// Statuses judges staleness by the clock, not the sweep's chosen time,
	// so the sites still read as current here.
	if st := s.Statuses("t1"); len(st) != 2 || st[0].Site != "ams" || st[0].Stale || st[1].Site != "fra" {
		t.Fatalf("statuses: %+v", st)
	}

	// A reload that narrows t1 to fra and slows it: ams is dropped and the
	// aggregate recomputed at once, by the clock, where fra reported seconds
	// ago: UP at one site. Then the reload is announced. Staleness follows
	// the new interval, so the sweep that found fra stale before now finds
	// nothing to change.
	s.reg.Replace([]MonitorConfig{
		{ID: "t1", Kind: "ping", Target: "1.2.3.4", IntervalS: 600, Sites: []string{"fra"}},
		{ID: "t2", Kind: "ping", Target: "5.6.7.8", IntervalS: 10},
	})
	s.Retain()
	if ev = next(); ev.Changes == nil || ev.Changes["t1"].State != StateUp || ev.Changes["t1"].Sites != 1 || ev.Changes["t1"].Stale != 0 {
		t.Fatalf("recomputed on reload: %+v", ev)
	}
	if ev = next(); ev.ID != "" || ev.Changes != nil || ev.Version != 2 {
		t.Fatalf("reload event: %+v", ev)
	}
	if st := s.Statuses("t1"); len(st) != 1 || st[0].Site != "fra" {
		t.Fatalf("statuses after narrowing: %+v", st)
	}
	s.Sweep(time.Now().Add(2 * time.Minute))
	none()

	// A site silent for longer than its forget window is dropped altogether,
	// metric series included: no retired agent pins a monitor. The window
	// scales with the interval: at ten minutes it is two stale periods, an
	// hour, so an hour and a minute is enough.
	before := testutil.CollectAndCount(s.up)
	s.Sweep(time.Now().Add(forgetAfter + time.Minute))
	if ev = next(); ev.Changes["t1"].State != StateNoData || ev.Changes["t1"].Sites != 0 {
		t.Fatalf("after forgetting: %+v", ev)
	}
	if st := s.Statuses("t1"); len(st) != 0 {
		t.Fatalf("statuses after forgetting: %+v", st)
	}
	if after := testutil.CollectAndCount(s.up); after != before-1 {
		t.Fatalf("up series after forgetting: %d, want %d", after, before-1)
	}
	// A result arriving afterwards registers the site again.
	cycle("t1", "fra", true)
	if ev = next(); ev.Aggregate.State != StateUp || ev.Aggregate.Sites != 1 {
		t.Fatalf("after re-registering: %+v", ev)
	}

	// A reload that moves t1 to a site with no data recomputes it to NO
	// DATA rather than leaving the old aggregate in place.
	s.reg.Replace([]MonitorConfig{
		{ID: "t1", Kind: "ping", Target: "1.2.3.4", IntervalS: 600, Sites: []string{"nyc"}},
		{ID: "t2", Kind: "ping", Target: "5.6.7.8", IntervalS: 10},
	})
	s.Retain()
	if ev = next(); ev.Changes["t1"].State != StateNoData || ev.Changes["t1"].Sites != 0 {
		t.Fatalf("moved to a silent site: %+v", ev)
	}
	if ev = next(); ev.Version != 3 {
		t.Fatalf("reload event: %+v", ev)
	}

	// A removed monitor loses its aggregate on reload.
	s.reg.Replace([]MonitorConfig{{ID: "t2", Kind: "ping", Target: "5.6.7.8", IntervalS: 10}})
	s.Retain()
	if ev = next(); ev.Version != 4 {
		t.Fatalf("third reload: %+v", ev)
	}
	if _, has := s.Aggregate("t1"); has {
		t.Fatal("removed monitor still has an aggregate")
	}

	// A subscriber that stops reading is cut off rather than blocking the
	// sink, and a new subscription starts from a snapshot.
	cycle("t2", "fra", true)
	next()
	for i := 0; i < 1100; i++ {
		s.publish(StatusEvent{ID: "t2", Aggregate: Aggregate{State: StateUp}})
	}
	closed := false
	for !closed {
		select {
		case _, ok := <-changes:
			closed = !ok
		case <-time.After(2 * time.Second):
			t.Fatal("slow subscriber was not cut off")
		}
	}
	snapshot, version, _, cancel2 := s.Subscribe()
	defer cancel2()
	if version != 4 || snapshot["t2"].State != StateUp {
		t.Fatalf("fresh snapshot: version=%d %v", version, snapshot)
	}
}

func TestSinkSlowMonitorIsNotForgottenBetweenRuns(t *testing.T) {
	s := newTestSink(t, []MonitorConfig{
		{ID: "slow", Kind: "trace", Target: "1.2.3.4", IntervalS: 3600},
	}, nil)
	s.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:slow", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 1, ReachedAt: 1, Hops: []*pb.Hop{{Ttl: 1, Sent: 1, Received: 1}}},
	}})
	// Two hours on: not yet stale (three intervals) and, since the forget
	// window is two stale periods, nowhere near forgotten.
	s.Sweep(time.Now().Add(2 * time.Hour))
	if st := s.Statuses("slow"); len(st) != 1 || st[0].Stale {
		t.Fatalf("after two hours: %+v", st)
	}
	if a, _ := s.Aggregate("slow"); a.State != StateUp {
		t.Fatalf("aggregate after two hours: %+v", a)
	}
	// Seven hours on it is gone.
	s.Sweep(time.Now().Add(7 * time.Hour))
	if st := s.Statuses("slow"); len(st) != 0 {
		t.Fatalf("after seven hours: %+v", st)
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
