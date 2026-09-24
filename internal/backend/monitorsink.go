package backend

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/vlog"
)

// monitorJobPrefix tags a job id as a self-scheduled monitor rather than an
// interactive run. The gateway routes "mon:<id>" events to the MonitorSink; the
// run manager owns everything else.
const monitorJobPrefix = "mon:"

func monitorJobID(id string) string { return monitorJobPrefix + id }

func isMonitorJob(jobID string) bool { return strings.HasPrefix(jobID, monitorJobPrefix) }

func monitorIDOf(jobID string) string { return strings.TrimPrefix(jobID, monitorJobPrefix) }

// rttBuckets are histogram bucket bounds in seconds, tuned for network RTT
// (1ms to 5s) rather than the client_golang defaults (which start at 5ms).
var rttBuckets = []float64{0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5}

// baseLabels are the fixed dimensions on every monitor metric. Operator-defined
// labels (supplier, …) vary per monitor and are exposed on prober_monitor_info,
// joined to these on (site, monitor) in Grafana — a fixed-label vector cannot
// hold varying label names, and this keeps histograms idiomatic.
var baseLabels = []string{"site", "monitor", "kind", "target", "transport"}

// MonitorSink turns self-scheduled monitor events into Prometheus metrics and,
// for trace/mtr, one VictoriaLogs record per completed trace carrying the mtr
// report. It also keeps each monitor's aggregate state across its sites for
// the monitors page and publishes changes to it. It is both an event handler
// (OnMonitorEvent) and a prometheus.Collector (for the info metric).
type MonitorSink struct {
	reg *MonitorRegistry
	vl  *vlog.Client

	probesTotal  *prometheus.CounterVec
	successTotal *prometheus.CounterVec
	rttSeconds   *prometheus.HistogramVec
	lossRatio    *prometheus.GaugeVec
	up           *prometheus.GaugeVec
	sipResponses *prometheus.CounterVec
	infoDesc     *prometheus.Desc

	mu        sync.Mutex
	state     map[monKey]*monState
	byMonitor map[string]map[string]*monState // monitor id -> site -> its state
	agg       map[string]Aggregate            // monitor id -> state across sites
	subs      map[*statusSub]struct{}
}

type monKey struct{ site, monitor string }

type monState struct {
	kind      string
	target    string
	transport string
	resolved  string
	source    string
	protocol  string
	labels    map[string]string
	intervalS uint32

	// The trace in progress, for the log record shipped when it ends: when it
	// started and its latest cycle, whose hops carry the running aggregates
	// over every cycle so far.
	startedAt time.Time
	lastCycle *pb.Cycle

	// The newest outcome, for the monitors page. Zero until the first result.
	last Outcome
}

// Outcome is one probe's result as the monitors page shows it: when it came,
// what the target resolved to at the time, whether it counts as healthy, the
// numbers behind that, and for SIP the response. A failed probe carries its
// message in Error.
type Outcome struct {
	At        time.Time
	Resolved  string
	Source    string
	Up        bool
	LossPct   float64
	RTTUs     uint32
	HasRTT    bool
	Reached   bool
	Cycles    uint32
	Hops      int
	Code      int
	Reason    string
	Responded bool
	Error     string
}

// MonitorStatus is a site's latest outcome for a monitor. Stale means the
// outcome is older than the monitor's staleAfter: the agent stopped
// reporting, so it proves nothing about the target.
type MonitorStatus struct {
	Site  string
	Stale bool
	Outcome
}

// Statuses returns a monitor's latest outcome at every site that has
// reported one, sorted by site.
func (s *MonitorSink) Statuses(id string) []MonitorStatus {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MonitorStatus
	for site, st := range s.byMonitor[id] {
		if st.last.At.IsZero() {
			continue
		}
		out = append(out, MonitorStatus{Site: site, Stale: now.Sub(st.last.At) > staleAfter(st.intervalS), Outcome: st.last})
	}
	slices.SortFunc(out, func(a, b MonitorStatus) int { return strings.Compare(a.Site, b.Site) })
	return out
}

// --- aggregate state and its stream ---------------------------------------------

// MonitorState is a monitor's health across its sites.
type MonitorState string

const (
	StateUp      MonitorState = "up"      // every reporting site is up
	StateDown    MonitorState = "down"    // every reporting site is down
	StatePartial MonitorState = "partial" // some sites up, some down or stale
	StateNoData  MonitorState = "nodata"  // nothing reported, or every site stale
)

// Aggregate is a monitor's state across its sites, for the monitors list.
// Sites is how many have reported; Up, Down and Stale split them. Since is
// when the state last changed, and is left out while nothing has reported.
type Aggregate struct {
	State MonitorState `json:"state"`
	Up    int          `json:"up"`
	Down  int          `json:"down"`
	Stale int          `json:"stale"`
	Sites int          `json:"sites"`
	Since time.Time    `json:"since,omitzero"`
}

// staleAfter is how long a site's outcome stays current: three intervals, at
// least a minute, so a quick agent restart does not flip its monitors.
func staleAfter(intervalS uint32) time.Duration {
	return max(3*time.Duration(intervalS)*time.Second, time.Minute)
}

const (
	// sweepInterval is how often staleness is re-evaluated, since no event
	// marks the moment a site's outcome goes stale.
	sweepInterval = 10 * time.Second
	// forgetAfter is how long a stale site is kept before it is dropped from
	// its monitors, at least. An agent silent this long is retired or in
	// real trouble, and either way its last result should stop shaping the
	// state; the Agents page is where its absence shows.
	forgetAfter = time.Hour
)

// forgetAfterFor scales the forget window with the monitor's interval, so a
// monitor that runs every hour or two is not wiped between its own runs.
func forgetAfterFor(intervalS uint32) time.Duration {
	return max(forgetAfter, 2*staleAfter(intervalS))
}

func (s *MonitorSink) aggregateLocked(id string, now time.Time) Aggregate {
	var a Aggregate
	for _, st := range s.byMonitor[id] {
		l := st.last
		if l.At.IsZero() {
			continue
		}
		a.Sites++
		switch {
		case now.Sub(l.At) > staleAfter(st.intervalS):
			a.Stale++
		case l.Up:
			a.Up++
		default:
			a.Down++
		}
	}
	switch {
	case a.Sites == 0 || a.Stale == a.Sites:
		a.State = StateNoData
	case a.Up == a.Sites:
		a.State = StateUp
	case a.Up == 0:
		a.State = StateDown
	default:
		a.State = StatePartial
	}
	return a
}

// refreshLocked recomputes a monitor's aggregate and reports whether anything
// in it changed. Since carries over while the state holds.
func (s *MonitorSink) refreshLocked(id string, now time.Time) (Aggregate, bool) {
	a := s.aggregateLocked(id, now)
	prev, had := s.agg[id]
	a.Since = prev.Since
	if !had || a.State != prev.State {
		a.Since = now.UTC()
	}
	changed := !had || a.State != prev.State || a.Up != prev.Up || a.Down != prev.Down || a.Stale != prev.Stale || a.Sites != prev.Sites
	s.agg[id] = a
	return a, changed
}

// refresh re-evaluates one monitor after an outcome landed and publishes the
// aggregate if it changed. Publishing happens under the lock, as everywhere,
// so subscribers see aggregates in the order they were computed.
func (s *MonitorSink) refresh(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, changed := s.refreshLocked(id, now); changed {
		s.publishLocked(StatusEvent{ID: id, Aggregate: a})
	}
}

// keepLocked makes sure st is the registered state for its site, putting it
// back if a sweep or reload removed the entry while a handler held it, so the
// outcome it is about to record is not written into an orphan.
func (s *MonitorSink) keepLocked(site, id string, st *monState) {
	k := monKey{site: site, monitor: id}
	if s.state[k] == st {
		return
	}
	s.state[k] = st
	if s.byMonitor[id] == nil {
		s.byMonitor[id] = map[string]*monState{}
	}
	s.byMonitor[id][site] = st
}

// forgetLocked drops one site's state for a monitor, with the metric series
// it produced, so nothing of a gone site lingers in the exposition.
func (s *MonitorSink) forgetLocked(k monKey) {
	p := prometheus.Labels{"site": k.site, "monitor": k.monitor}
	s.probesTotal.DeletePartialMatch(p)
	s.successTotal.DeletePartialMatch(p)
	s.rttSeconds.DeletePartialMatch(p)
	s.lossRatio.DeletePartialMatch(p)
	s.up.DeletePartialMatch(p)
	s.sipResponses.DeletePartialMatch(p)
	delete(s.state, k)
	if sites := s.byMonitor[k.monitor]; sites != nil {
		delete(sites, k.site)
		if len(sites) == 0 {
			delete(s.byMonitor, k.monitor)
		}
	}
}

// monitorIDsLocked is every monitor with state or an aggregate: both need
// re-evaluating, since a monitor can hold an aggregate after its last site
// was dropped.
func (s *MonitorSink) monitorIDsLocked() map[string]struct{} {
	ids := make(map[string]struct{}, len(s.agg)+len(s.byMonitor))
	for id := range s.byMonitor {
		ids[id] = struct{}{}
	}
	for id := range s.agg {
		ids[id] = struct{}{}
	}
	return ids
}

// Sweep drops sites silent for longer than their forget window, re-evaluates
// every monitor for staleness, and publishes what changed as one batch, so a
// mass change (an agent dropping) is one message, not one per monitor. Run
// calls it periodically; tests call it with a chosen time.
func (s *MonitorSink) Sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, st := range s.state {
		if !st.last.At.IsZero() && now.Sub(st.last.At) > forgetAfterFor(st.intervalS) {
			s.forgetLocked(k)
		}
	}
	changed := map[string]Aggregate{}
	for id := range s.monitorIDsLocked() {
		if a, ok := s.refreshLocked(id, now); ok {
			changed[id] = a
		}
	}
	if len(changed) > 0 {
		s.publishLocked(StatusEvent{Changes: changed})
	}
}

// Run sweeps for stale sites until ctx ends.
func (s *MonitorSink) Run(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.Sweep(now)
		}
	}
}

// Aggregate returns one monitor's current aggregate, if it has ever reported.
func (s *MonitorSink) Aggregate(id string) (Aggregate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agg[id]
	return a, ok
}

// Aggregates returns every monitor's current aggregate. A monitor that has
// never reported is absent.
func (s *MonitorSink) Aggregates() map[string]Aggregate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Aggregate, len(s.agg))
	for id, a := range s.agg {
		out[id] = a
	}
	return out
}

// StatusEvent is one message on the status stream: one monitor's new
// aggregate (ID set), a batch of them from a sweep (Changes set), or a
// configuration reload after which the client should fetch the monitor list
// again (neither set, Version the new one).
type StatusEvent struct {
	ID        string
	Aggregate Aggregate
	Changes   map[string]Aggregate
	Version   uint64
}

// statusSub is one stream subscriber. Sends and the close are serialised so
// a publisher can never send on a channel another has just closed.
type statusSub struct {
	mu     sync.Mutex
	ch     chan StatusEvent
	closed bool
}

// send delivers without blocking. A subscriber that has fallen a full buffer
// behind is closed instead: it can no longer be trusted with deltas, and a
// fresh subscription brings a snapshot.
func (sub *statusSub) send(ev StatusEvent) bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return false
	}
	select {
	case sub.ch <- ev:
		return true
	default:
		sub.closed = true
		close(sub.ch)
		return false
	}
}

func (sub *statusSub) close() {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
}

// Subscribe returns every monitor's current aggregate and the registry
// version, then a channel of changes. The channel is closed when the
// subscriber falls too far behind, at which point it should subscribe again
// rather than trust what it has. cancel ends the subscription.
//
// The snapshot is taken and the subscriber registered under the one lock
// every publish holds, so no change can fall between them. The version is
// read after registering: a reload before that point is reflected in it, and
// one after it reaches the subscriber as an event.
func (s *MonitorSink) Subscribe() (snapshot map[string]Aggregate, version uint64, changes <-chan StatusEvent, cancel func()) {
	sub := &statusSub{ch: make(chan StatusEvent, 1024)}
	s.mu.Lock()
	snapshot = make(map[string]Aggregate, len(s.agg))
	for id, a := range s.agg {
		snapshot[id] = a
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	return snapshot, s.reg.Version(), sub.ch, func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
		sub.close()
	}
}

// publishLocked delivers an event to every subscriber, under s.mu so events
// go out in the order their aggregates were computed. Sends never block; a
// subscriber that cannot keep up is dropped.
func (s *MonitorSink) publishLocked(ev StatusEvent) {
	for sub := range s.subs {
		if !sub.send(ev) {
			delete(s.subs, sub)
		}
	}
}

func (s *MonitorSink) publish(ev StatusEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked(ev)
}

// NewMonitorSink builds the sink and registers its vector metrics on reg. The
// sink itself is registered as a collector for prober_monitor_info.
func NewMonitorSink(reg *MonitorRegistry, vl *vlog.Client, promReg prometheus.Registerer) *MonitorSink {
	s := &MonitorSink{
		reg:       reg,
		vl:        vl,
		state:     map[monKey]*monState{},
		byMonitor: map[string]map[string]*monState{},
		agg:       map[string]Aggregate{},
		subs:      map[*statusSub]struct{}{},
		probesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prober_monitor_probes_total",
			Help: "Total monitor probes attempted.",
		}, baseLabels),
		successTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prober_monitor_probes_success_total",
			Help: "Monitor probes that reached the target (trace/ping) or got any final response (sip).",
		}, baseLabels),
		rttSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "prober_monitor_rtt_seconds",
			Help:    "Round-trip time to the monitor target.",
			Buckets: rttBuckets,
		}, baseLabels),
		lossRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "prober_monitor_loss_ratio",
			Help: "Packet loss to the target on the last cycle, 0..1 (trace/ping).",
		}, baseLabels),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "prober_monitor_up",
			Help: "1 if the last probe was healthy, else 0.",
		}, baseLabels),
		sipResponses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prober_monitor_sip_responses_total",
			Help: "SIP OPTIONS responses by status code (\"timeout\" when none).",
		}, append(append([]string{}, baseLabels...), "code")),
	}
	promReg.MustRegister(s.probesTotal, s.successTotal, s.rttSeconds, s.lossRatio, s.up, s.sipResponses, s)
	return s
}

// Describe sends no descriptors, marking this an "unchecked" collector: the
// info metric's label names vary per monitor (each carries its own operator
// labels), which a fixed descriptor cannot express.
func (s *MonitorSink) Describe(chan<- *prometheus.Desc) {
	// Intentionally empty: an unchecked collector announces no descriptors, which
	// is what lets Collect emit info metrics whose label names vary per monitor.
}

// Collect emits one prober_monitor_info series per known (site, monitor) with
// its operator labels, so those labels are queryable alongside the fixed-label
// metrics via a join.
func (s *MonitorSink) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	states := make([]*monState, 0, len(s.state))
	keys := make([]monKey, 0, len(s.state))
	for k, st := range s.state {
		keys = append(keys, k)
		states = append(states, st)
	}
	s.mu.Unlock()

	for i, st := range states {
		names := []string{"site", "monitor", "kind", "target"}
		vals := []string{keys[i].site, keys[i].monitor, st.kind, st.target}
		for k, v := range st.labels {
			names = append(names, sanitizeLabel(k))
			vals = append(vals, v)
		}
		desc := prometheus.NewDesc(
			"prober_monitor_info",
			"Monitor metadata (operator labels); value is always 1. Join on (site, monitor).",
			nil,
			labelMap(names, vals),
		)
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)
	}
}

// Retain reconciles the state with a reloaded configuration: monitors no
// longer configured, and sites a monitor no longer runs at, lose their metric
// series, state and aggregate; surviving state picks up the new interval its
// staleness is judged by; every surviving monitor's aggregate is recomputed,
// since dropping a site changes it. It publishes those changes, then tells
// the status stream to fetch the new list.
func (s *MonitorSink) Retain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, st := range s.state {
		mc, ok := s.reg.lookup(k.monitor)
		if ok && siteMatches(mc.Sites, k.site) {
			st.intervalS = mc.IntervalS
			continue
		}
		s.forgetLocked(k)
	}
	// Aggregates are pruned by the registry, not through state entries: a
	// monitor whose sites were all dropped has none left to walk.
	for id := range s.agg {
		if _, ok := s.reg.lookup(id); !ok {
			delete(s.agg, id)
		}
	}
	now := time.Now()
	changed := map[string]Aggregate{}
	for id := range s.monitorIDsLocked() {
		if a, ok := s.refreshLocked(id, now); ok {
			changed[id] = a
		}
	}
	if len(changed) > 0 {
		s.publishLocked(StatusEvent{Changes: changed})
	}
	s.publishLocked(StatusEvent{Version: s.reg.Version()})
}

// OnMonitorEvent handles one event for a "mon:<id>" job from a given site. It
// dispatches to a per-kind handler; the handlers own the metric and log updates.
func (s *MonitorSink) OnMonitorEvent(site string, ev *pb.JobEvent) {
	id := monitorIDOf(ev.JobId)
	st := s.stateFor(site, id)
	if st == nil {
		return // unknown monitor (removed between dispatch and event)
	}
	lv := []string{site, id, st.kind, st.target, st.transport}

	switch e := ev.Event.(type) {
	case *pb.JobEvent_Started:
		s.onStarted(st, ev, e.Started)
	case *pb.JobEvent_Cycle:
		s.onCycle(site, id, st, lv, e.Cycle)
	case *pb.JobEvent_SipResult:
		s.onSipResult(site, id, st, lv, e.SipResult)
	case *pb.JobEvent_Finished:
		// A cancelled trace (reload, agent shutdown) is not a report: its
		// aggregates cover part of a run, so it is dropped rather than
		// shipped looking complete.
		if e.Finished.Reason == pb.JobFinished_REASON_CANCELLED {
			s.dropReport(st)
		} else {
			s.shipReport(site, id, st, "")
		}
	case *pb.JobEvent_Error:
		// A failed probe (resolve/policy/engine): count it and mark down.
		s.probesTotal.WithLabelValues(lv...).Inc()
		s.up.WithLabelValues(lv...).Set(0)
		s.mu.Lock()
		s.keepLocked(site, id, st)
		st.last = Outcome{At: time.Now(), Resolved: st.resolved, Source: st.source, Error: e.Error.Message}
		if st.kind != "sip" {
			st.last.LossPct = 100
		}
		s.mu.Unlock()
		// Only an engine error can follow cycles of the same run; the other
		// codes fail before Started, so a buffered cycle would belong to an
		// earlier run whose end never arrived. Report the former with the
		// error attached, drop the latter.
		if e.Error.Code == pb.JobError_CODE_ENGINE {
			s.shipReport(site, id, st, e.Error.Message)
		} else {
			s.dropReport(st)
		}
	}
	switch ev.Event.(type) {
	case *pb.JobEvent_Cycle, *pb.JobEvent_SipResult, *pb.JobEvent_Error:
		s.refresh(id, time.Now())
	}
}

// dropReport discards the buffered cycle of a trace that did not end cleanly.
func (s *MonitorSink) dropReport(st *monState) {
	s.mu.Lock()
	st.lastCycle = nil
	s.mu.Unlock()
}

// onStarted records the resolved address, source, and protocol for later
// events, and opens a fresh trace for the log record.
func (s *MonitorSink) onStarted(st *monState, ev *pb.JobEvent, started *pb.JobStarted) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.resolved = started.Resolved
	st.source = started.Source
	if started.Protocol != pb.Protocol_PROTOCOL_UNSPECIFIED {
		st.protocol = protoName(started.Protocol)
	}
	st.startedAt = time.Now()
	if ev.Time != nil {
		st.startedAt = ev.Time.AsTime()
	}
	st.lastCycle = nil
}

// onCycle updates RTT, loss, and up from a trace/ping cycle, and keeps the
// cycle for the report a trace monitor ships when it finishes.
func (s *MonitorSink) onCycle(site, id string, st *monState, lv []string, c *pb.Cycle) {
	hop, reached := destHop(c)
	s.probesTotal.WithLabelValues(lv...).Inc()
	if reached {
		s.successTotal.WithLabelValues(lv...).Inc()
		s.rttSeconds.WithLabelValues(lv...).Observe(float64(hop.AvgUs) / 1e6)
	}
	s.up.WithLabelValues(lv...).Set(boolGauge(reached))
	loss := 1.0
	if reached {
		loss = hop.LossPct / 100
	}
	s.lossRatio.WithLabelValues(lv...).Set(loss)

	s.mu.Lock()
	s.keepLocked(site, id, st)
	st.last = Outcome{
		At: time.Now(), Resolved: st.resolved, Source: st.source,
		Up: reached, LossPct: loss * 100, Reached: reached, Cycles: c.Number, Hops: len(c.Hops),
	}
	if reached {
		st.last.RTTUs, st.last.HasRTT = hop.AvgUs, true
	}
	if st.kind == "trace" {
		st.lastCycle = c
	}
	s.mu.Unlock()
}

// onSipResult updates the SIP counters, code label, up, and RTT for one
// probe. Any final response counts as success: an OPTIONS probe measures
// reachability, and a 403 or 503 proves the peer's SIP stack is up and
// answering just as a 200 does. The code is still counted per value, so an
// alert on a particular one remains possible.
func (s *MonitorSink) onSipResult(site, id string, st *monState, lv []string, r *pb.SipResult) {
	s.probesTotal.WithLabelValues(lv...).Inc()
	code := "timeout"
	success := r.Responded
	if r.Responded {
		code = strconv.Itoa(int(r.StatusCode))
	}
	codeLV := append(append([]string{}, lv...), code)
	s.sipResponses.WithLabelValues(codeLV...).Inc()
	if success {
		s.successTotal.WithLabelValues(lv...).Inc()
	}
	s.up.WithLabelValues(lv...).Set(boolGauge(success))
	if r.Responded && r.RttUs != nil {
		s.rttSeconds.WithLabelValues(lv...).Observe(float64(*r.RttUs) / 1e6)
	}

	s.mu.Lock()
	s.keepLocked(site, id, st)
	st.last = Outcome{
		At: time.Now(), Resolved: st.resolved, Source: st.source,
		Up: success, Code: int(r.StatusCode), Reason: r.Reason, Responded: r.Responded,
	}
	if r.Responded && r.RttUs != nil {
		st.last.RTTUs, st.last.HasRTT = *r.RttUs, true
	}
	s.mu.Unlock()
}

// boolGauge maps a health boolean to a gauge value.
func boolGauge(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

// stateFor returns the state for (site, monitor), creating it from the registry
// on first sight. Returns nil if the monitor is no longer configured.
func (s *MonitorSink) stateFor(site, id string) *monState {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := monKey{site: site, monitor: id}
	if st, ok := s.state[k]; ok {
		return st
	}
	mc, ok := s.reg.lookup(id)
	if !ok {
		return nil
	}
	st := &monState{
		kind:      monitorKind(mc),
		target:    mc.Target,
		transport: monitorTransport(mc),
		labels:    mc.Labels,
		protocol:  monitorProtocol(mc),
		intervalS: mc.IntervalS,
	}
	s.state[k] = st
	if s.byMonitor[id] == nil {
		s.byMonitor[id] = map[string]*monState{}
	}
	s.byMonitor[id][site] = st
	return st
}

// shipReport writes one VictoriaLogs record for the trace that just ended: the
// full mtr report as the message, with the destination hop's aggregates as
// fields so records can be filtered on loss or RTT. The last cycle's hops carry
// the running totals over every cycle, which is what `mtr --report` prints.
// Nothing is shipped for a trace that produced no cycle.
func (s *MonitorSink) shipReport(site, id string, st *monState, errMsg string) {
	s.mu.Lock()
	c := st.lastCycle
	st.lastCycle = nil
	startedAt, resolved, source, protocol := st.startedAt, st.resolved, st.source, st.protocol
	s.mu.Unlock()
	if c == nil || s.vl == nil || !s.vl.Enabled() {
		return
	}

	hop, reached := destHop(c)
	rec := vlog.Record{
		"_time":    vlog.Now(),
		"_msg":     mtrReport(site, source, startedAt, c.Hops),
		"site":     site,
		"monitor":  id,
		"target":   st.target,
		"resolved": resolved,
		"source":   source,
		"protocol": protocol,
		"cycles":   c.Number,
		"hops":     len(c.Hops),
		"reached":  reached,
		"loss_pct": 100.0,
	}
	if reached {
		rec["sent"] = hop.Sent
		rec["received"] = hop.Received
		rec["loss_pct"] = hop.LossPct
		rec["best_ms"] = usToMs(hop.BestUs)
		rec["avg_ms"] = usToMs(hop.AvgUs)
		rec["worst_ms"] = usToMs(hop.WorstUs)
		rec["stdev_ms"] = usToMs(hop.StdevUs)
		rec["jitter_ms"] = usToMs(hop.JitterUs)
	}
	if errMsg != "" {
		rec["error"] = errMsg
	}
	for k, v := range st.labels {
		if _, taken := rec[k]; !taken {
			rec[k] = v
		}
	}
	s.vl.Write(rec)
}

// --- helpers ----------------------------------------------------------------

// destHop is the hop a cycle's health is judged by: the one the target
// answered at, per the engine's reached-at TTL, or the last hop probed while
// the target has not answered. The flag is whether the target has.
func destHop(c *pb.Cycle) (*pb.Hop, bool) {
	if c.ReachedAt > 0 {
		for _, h := range c.Hops {
			if h.Ttl == c.ReachedAt {
				return h, h.Received > 0
			}
		}
	}
	if n := len(c.Hops); n > 0 {
		return c.Hops[n-1], false
	}
	return nil, false
}

func usToMs(us uint32) float64 { return float64(us) / 1000 }

func monitorKind(m MonitorConfig) string {
	if m.Kind == "" {
		return "trace"
	}
	return m.Kind
}

func monitorTransport(m MonitorConfig) string {
	if m.Kind == "sip" {
		if m.SIP != nil && m.SIP.Transport != "" {
			return m.SIP.Transport
		}
		return "udp"
	}
	return monitorProtocol(m)
}

func monitorProtocol(m MonitorConfig) string {
	if m.Kind == "sip" {
		return ""
	}
	if m.Trace != nil && m.Trace.Protocol != "" {
		return m.Trace.Protocol
	}
	return "icmp"
}

func protoName(p pb.Protocol) string {
	switch p {
	case pb.Protocol_PROTOCOL_TCP:
		return "tcp"
	case pb.Protocol_PROTOCOL_UDP:
		return "udp"
	case pb.Protocol_PROTOCOL_ICMP:
		return "icmp"
	}
	return ""
}

func labelMap(names, vals []string) prometheus.Labels {
	m := make(prometheus.Labels, len(names))
	for i := range names {
		m[names[i]] = vals[i]
	}
	return m
}

// sanitizeLabel maps an operator label key to a valid Prometheus label name
// (letters, digits, underscores; not starting with a digit).
func sanitizeLabel(k string) string {
	var b strings.Builder
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}
