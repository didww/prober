package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

func schedLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// minimalAgent builds an Agent without opening the engine's raw sockets. With
// MaxConcurrentJobs = 0 every startJob short-circuits at the limit and emits an
// error event, so the scheduler's dispatch path runs without a real probe.
func minimalAgent() *Agent {
	return &Agent{
		log:  schedLogger(),
		cfg:  Config{Limits: Limits{MaxConcurrentJobs: 0}},
		jobs: jobTable{m: map[string]*jobHandle{}},
	}
}

func TestSchedulerFiresEachMonitor(t *testing.T) {
	a := minimalAgent()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan *pb.AgentMessage, 64)
	send := func(m *pb.AgentMessage) error { events <- m; return nil }

	sched := newScheduler(a, send)
	sched.apply(ctx, &pb.Assignment{Version: 1, Monitors: []*pb.Monitor{
		{Id: "m1", IntervalS: 1, Spec: &pb.Monitor_Trace{Trace: &pb.TraceSpec{Target: "1.1.1.1"}}},
		{Id: "m2", IntervalS: 1, Spec: &pb.Monitor_SipOptions{SipOptions: &pb.SipOptionsSpec{Target: "2.2.2.2"}}},
	}})

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case m := <-events:
			if ev := m.GetEvent(); ev != nil {
				seen[ev.JobId] = true
			}
		case <-deadline:
			t.Fatalf("did not see both monitors fire; saw %v", seen)
		}
	}
	if !seen["mon:m1"] || !seen["mon:m2"] {
		t.Fatalf("expected mon:m1 and mon:m2, saw %v", seen)
	}
}

func TestSchedulerNoopOnSameVersion(t *testing.T) {
	a := minimalAgent()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched := newScheduler(a, func(*pb.AgentMessage) error { return nil })

	as := &pb.Assignment{Version: 5, Monitors: []*pb.Monitor{
		{Id: "m1", IntervalS: 60, Spec: &pb.Monitor_Trace{Trace: &pb.TraceSpec{Target: "1.1.1.1"}}},
	}}
	sched.apply(ctx, as)
	sched.mu.Lock()
	n1 := len(sched.cancels)
	sched.mu.Unlock()

	sched.apply(ctx, as) // same version: must not restart
	sched.mu.Lock()
	n2 := len(sched.cancels)
	v := sched.version
	sched.mu.Unlock()

	if n1 != 1 || n2 != 1 {
		t.Fatalf("cancels: first=%d second=%d, want 1 and 1", n1, n2)
	}
	if v != 5 {
		t.Fatalf("version = %d, want 5", v)
	}
}

func TestSchedulerReplacesOnNewVersion(t *testing.T) {
	a := minimalAgent()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched := newScheduler(a, func(*pb.AgentMessage) error { return nil })

	sched.apply(ctx, &pb.Assignment{Version: 1, Monitors: []*pb.Monitor{
		{Id: "m1", IntervalS: 60, Spec: &pb.Monitor_Trace{Trace: &pb.TraceSpec{Target: "1.1.1.1"}}},
	}})
	sched.apply(ctx, &pb.Assignment{Version: 2, Monitors: []*pb.Monitor{
		{Id: "m1", IntervalS: 60, Spec: &pb.Monitor_Trace{Trace: &pb.TraceSpec{Target: "1.1.1.1"}}},
		{Id: "m2", IntervalS: 60, Spec: &pb.Monitor_Trace{Trace: &pb.TraceSpec{Target: "2.2.2.2"}}},
	}})
	sched.mu.Lock()
	defer sched.mu.Unlock()
	if sched.version != 2 || len(sched.cancels) != 2 {
		t.Fatalf("version=%d cancels=%d, want 2 and 2", sched.version, len(sched.cancels))
	}
}

func TestStartJobFromMonitor(t *testing.T) {
	tr := startJobFromMonitor(&pb.Monitor{Id: "t", Spec: &pb.Monitor_Trace{Trace: &pb.TraceSpec{Target: "1.1.1.1"}}})
	if tr.JobId != "mon:t" || tr.GetTrace() == nil {
		t.Fatalf("trace mapping wrong: %+v", tr)
	}
	sp := startJobFromMonitor(&pb.Monitor{Id: "s", Spec: &pb.Monitor_SipOptions{SipOptions: &pb.SipOptionsSpec{Target: "2.2.2.2"}}})
	if sp.JobId != "mon:s" || sp.GetSipOptions() == nil {
		t.Fatalf("sip mapping wrong: %+v", sp)
	}
	if startJobFromMonitor(&pb.Monitor{Id: "empty"}) != nil {
		t.Fatal("monitor with no spec should map to nil")
	}
}
