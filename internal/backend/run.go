package backend

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// A run is one target executed across one or more sites. It is the unit the
// browser starts, watches over SSE, and cancels.
type Manager struct {
	gw  *Gateway
	ttl time.Duration

	mu   sync.Mutex
	runs map[string]*Run
}

func NewManager(gw *Gateway, ttl time.Duration) *Manager {
	return &Manager{gw: gw, ttl: ttl, runs: make(map[string]*Run)}
}

// Start creates a run, dispatches one job per site, and returns it. Sites
// with no connected agent get an immediate error event rather than failing
// the whole run.
func (m *Manager) Start(spec *pb.TraceSpec, sites []string) *Run {
	runID := uuid.NewString()
	r := &Run{
		ID:      runID,
		Sites:   sites,
		mgr:     m,
		created: time.Now(),
		pending: map[string]bool{},
	}
	m.mu.Lock()
	m.runs[runID] = r
	m.mu.Unlock()

	for _, site := range sites {
		jobID := runID + ":" + site
		r.pending[jobID] = true
		if !m.gw.Connected(site) {
			r.emit(&pb.JobEvent{
				JobId: jobID,
				Time:  timestamppb.Now(),
				Event: &pb.JobEvent_Error{Error: &pb.JobError{
					Code:    pb.JobError_CODE_UNSPECIFIED,
					Message: "site " + site + " has no connected agent",
				}},
			})
			delete(r.pending, jobID)
			continue
		}
		if err := m.gw.StartJob(site, jobID, spec, r); err != nil {
			r.emit(&pb.JobEvent{
				JobId: jobID,
				Time:  timestamppb.Now(),
				Event: &pb.JobEvent_Error{Error: &pb.JobError{Message: err.Error()}},
			})
			delete(r.pending, jobID)
		}
	}
	r.checkDone()
	return r
}

func (m *Manager) Get(id string) (*Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	return r, ok
}

func (m *Manager) Cancel(id string) {
	r, ok := m.Get(id)
	if !ok {
		return
	}
	for _, site := range r.Sites {
		m.gw.CancelJob(site, id+":"+site)
	}
}

// reap deletes runs whose TTL has passed since they finished.
func (m *Manager) reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, r := range m.runs {
		if r.finishedAt(now, m.ttl) {
			for _, site := range r.Sites {
				m.gw.EndJob(id + ":" + site)
			}
			delete(m.runs, id)
		}
	}
}

// Reap runs the TTL sweep until ctx is done.
func (m *Manager) Reap(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reap()
		}
	}
}

// Run holds one run's buffered events and its subscribers. Events carry a
// run-wide monotonic id used as the SSE id, so a reconnecting browser resumes
// with Last-Event-ID.
type Run struct {
	ID    string
	Sites []string

	mgr     *Manager
	created time.Time

	mu      sync.Mutex
	seq     uint64
	buffer  []*StreamEvent
	subs    map[*subscriber]struct{}
	pending map[string]bool // job id -> still running
	done    bool
	doneAt  time.Time
}

// subscriber is one open SSE stream. Its channel is closed exactly once,
// whether the run finishes or the client disconnects first, so the two racing
// closers cannot double-close it.
type subscriber struct {
	ch   chan *StreamEvent
	once sync.Once
}

func (s *subscriber) close() { s.once.Do(func() { close(s.ch) }) }

// StreamEvent is what the browser receives: the agent event plus the run-wide
// sequence and the site it came from.
type StreamEvent struct {
	Seq   uint64       `json:"seq"`
	Site  string       `json:"site"`
	Event *pb.JobEvent `json:"-"`
}

// OnEvent satisfies EventSink: the gateway calls it for every agent event.
func (r *Run) OnEvent(ev *pb.JobEvent) {
	site := siteOf(ev.JobId)
	r.emit(ev)
	switch ev.Event.(type) {
	case *pb.JobEvent_Finished, *pb.JobEvent_Error:
		r.mu.Lock()
		delete(r.pending, ev.JobId)
		r.mu.Unlock()
		_ = site
		r.checkDone()
	}
}

func (r *Run) emit(ev *pb.JobEvent) {
	r.mu.Lock()
	r.seq++
	se := &StreamEvent{Seq: r.seq, Site: siteOf(ev.JobId), Event: ev}
	r.buffer = append(r.buffer, se)
	subs := make([]*subscriber, 0, len(r.subs))
	for sub := range r.subs {
		subs = append(subs, sub)
	}
	r.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- se:
		default:
			// A slow subscriber misses live events but can still replay from
			// the buffer on reconnect; better than blocking every other one.
		}
	}
}

func (r *Run) checkDone() {
	r.mu.Lock()
	if !r.done && len(r.pending) == 0 {
		r.done = true
		r.doneAt = time.Now()
		subs := make([]*subscriber, 0, len(r.subs))
		for sub := range r.subs {
			subs = append(subs, sub)
		}
		r.subs = nil
		r.mu.Unlock()
		for _, sub := range subs {
			sub.close()
		}
		return
	}
	r.mu.Unlock()
}

// Subscribe returns the events after `after`, then a channel of live ones.
// The channel is closed when the run is done. cancel removes the subscriber.
func (r *Run) Subscribe(after uint64) (backlog []*StreamEvent, live <-chan *StreamEvent, cancel func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, se := range r.buffer {
		if se.Seq > after {
			backlog = append(backlog, se)
		}
	}
	if r.done {
		ch := make(chan *StreamEvent)
		close(ch)
		return backlog, ch, func() {}
	}
	sub := &subscriber{ch: make(chan *StreamEvent, 256)}
	if r.subs == nil {
		r.subs = make(map[*subscriber]struct{})
	}
	r.subs[sub] = struct{}{}
	return backlog, sub.ch, func() {
		r.mu.Lock()
		delete(r.subs, sub)
		r.mu.Unlock()
		sub.close()
	}
}

// Snapshot returns every event so far, for a page load before subscribing.
func (r *Run) Snapshot() []*StreamEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*StreamEvent, len(r.buffer))
	copy(out, r.buffer)
	return out
}

func (r *Run) Done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

func (r *Run) finishedAt(now time.Time, ttl time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done && now.Sub(r.doneAt) > ttl
}

func siteOf(jobID string) string {
	for i := len(jobID) - 1; i >= 0; i-- {
		if jobID[i] == ':' {
			return jobID[i+1:]
		}
	}
	return ""
}
