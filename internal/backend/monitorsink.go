package backend

import (
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
// report. It is both an event handler (OnMonitorEvent) and a
// prometheus.Collector (for the info metric).
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

	mu    sync.Mutex
	state map[monKey]*monState
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

// MonitorStatus is a site's latest outcome for a monitor.
type MonitorStatus struct {
	Site string
	Outcome
}

// AllStatuses returns every site's latest outcome, grouped by monitor id and
// sorted by site, in one pass over the state.
func (s *MonitorSink) AllStatuses() map[string][]MonitorStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]MonitorStatus{}
	for k, st := range s.state {
		if st.last.At.IsZero() {
			continue
		}
		out[k.monitor] = append(out[k.monitor], MonitorStatus{Site: k.site, Outcome: st.last})
	}
	for _, list := range out {
		slices.SortFunc(list, func(a, b MonitorStatus) int { return strings.Compare(a.Site, b.Site) })
	}
	return out
}

// NewMonitorSink builds the sink and registers its vector metrics on reg. The
// sink itself is registered as a collector for prober_monitor_info.
func NewMonitorSink(reg *MonitorRegistry, vl *vlog.Client, promReg prometheus.Registerer) *MonitorSink {
	s := &MonitorSink{
		reg:   reg,
		vl:    vl,
		state: map[monKey]*monState{},
		probesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prober_monitor_probes_total",
			Help: "Total monitor probes attempted.",
		}, baseLabels),
		successTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prober_monitor_probes_success_total",
			Help: "Monitor probes that reached the target (trace/ping) or got a 2xx (sip).",
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

// Retain drops metric series and info state for monitors no longer configured,
// keeping the exposition clean after a reload removes targets.
func (s *MonitorSink) Retain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.state {
		if _, ok := s.reg.lookup(k.monitor); !ok {
			p := prometheus.Labels{"site": k.site, "monitor": k.monitor}
			s.probesTotal.DeletePartialMatch(p)
			s.successTotal.DeletePartialMatch(p)
			s.rttSeconds.DeletePartialMatch(p)
			s.lossRatio.DeletePartialMatch(p)
			s.up.DeletePartialMatch(p)
			s.sipResponses.DeletePartialMatch(p)
			delete(s.state, k)
		}
	}
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
		s.onCycle(st, lv, e.Cycle)
	case *pb.JobEvent_SipResult:
		s.onSipResult(st, lv, e.SipResult)
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
func (s *MonitorSink) onCycle(st *monState, lv []string, c *pb.Cycle) {
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

// onSipResult updates the SIP counters, code label, up, and RTT for one probe.
func (s *MonitorSink) onSipResult(st *monState, lv []string, r *pb.SipResult) {
	s.probesTotal.WithLabelValues(lv...).Inc()
	code := "timeout"
	success := false
	if r.Responded {
		code = strconv.Itoa(int(r.StatusCode))
		success = r.StatusCode/100 == 2
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
	}
	s.state[k] = st
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
