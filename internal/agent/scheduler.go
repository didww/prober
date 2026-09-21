package agent

import (
	"context"
	"sync"
	"time"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// monitorJobPrefix must match the backend's: it tags an event as coming from a
// self-scheduled monitor so the backend routes it to the metrics/log sink rather
// than an interactive run.
const monitorJobPrefix = "mon:"

func monitorJobID(id string) string { return monitorJobPrefix + id }

// scheduler runs the agent's self-scheduled monitors for one session. The
// backend pushes a versioned Assignment; the scheduler starts one ticker per
// monitor, staggered, and fires probes through the same engines the interactive
// path uses. A new session builds a fresh scheduler (version 0), so the first
// assignment always applies; reconnect needs no reconciliation.
type scheduler struct {
	a    *Agent
	send func(*pb.AgentMessage) error

	mu      sync.Mutex
	version uint64
	cancels []context.CancelFunc
}

func newScheduler(a *Agent, send func(*pb.AgentMessage) error) *scheduler {
	return &scheduler{a: a, send: send}
}

// apply replaces the schedule with the monitors in as, unless the version is
// unchanged. Stopping the old tickers cancels their in-flight probes. ctx bounds
// every monitor started here (the session context); a new session builds a fresh
// scheduler, so ctx is stable across the apply calls of one session.
func (s *scheduler) apply(ctx context.Context, as *pb.Assignment) {
	if as == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if as.Version != 0 && as.Version == s.version {
		return
	}
	s.stopLocked()
	s.version = as.Version
	n := len(as.Monitors)
	s.a.log.Info("applying monitor assignment", "version", as.Version, "monitors", n)
	for i, m := range as.Monitors {
		mctx, cancel := context.WithCancel(ctx)
		s.cancels = append(s.cancels, cancel)
		go s.runMonitor(mctx, i, n, m)
	}
}

func (s *scheduler) stopLocked() {
	for _, c := range s.cancels {
		c()
	}
	s.cancels = nil
}

// runMonitor fires one monitor on its interval, staggered by its position so all
// monitors do not fire on the same tick.
func (s *scheduler) runMonitor(ctx context.Context, i, n int, m *pb.Monitor) {
	interval := time.Duration(m.IntervalS) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	if n > 1 {
		offset := time.Duration(int64(interval) * int64(i) / int64(n))
		select {
		case <-ctx.Done():
			return
		case <-time.After(offset):
		}
	}
	s.fire(ctx, m)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.fire(ctx, m)
		}
	}
}

// fire starts one probe for a monitor, unless its previous probe is still
// running (interval shorter than the probe): loss should show as a timeout in
// the probe, not as overlapping jobs.
func (s *scheduler) fire(ctx context.Context, m *pb.Monitor) {
	jid := monitorJobID(m.Id)
	if s.a.jobs.has(jid) {
		s.a.log.Debug("monitor still running, skipping tick", "monitor", m.Id)
		return
	}
	job := startJobFromMonitor(m)
	if job == nil {
		s.a.log.Warn("monitor has no spec", "monitor", m.Id)
		return
	}
	s.a.startJob(ctx, job, s.send)
}

// startJobFromMonitor builds the StartJob for a monitor tick. The engines are
// shared with the interactive path; only the job id (mon:<id>) marks it as a
// monitor so the backend routes its events to the metrics/log sink.
func startJobFromMonitor(m *pb.Monitor) *pb.StartJob {
	job := &pb.StartJob{JobId: monitorJobID(m.Id)}
	switch sp := m.Spec.(type) {
	case *pb.Monitor_Trace:
		job.Spec = &pb.StartJob_Trace{Trace: sp.Trace}
	case *pb.Monitor_SipOptions:
		job.Spec = &pb.StartJob_SipOptions{SipOptions: sp.SipOptions}
	default:
		return nil
	}
	return job
}
