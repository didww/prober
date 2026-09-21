package backend

import (
	"strconv"
	"strings"
	"sync"

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
// for trace/mtr, VictoriaLogs records. It is both an event handler
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
func (s *MonitorSink) Describe(chan<- *prometheus.Desc) {}

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

// OnMonitorEvent handles one event for a "mon:<id>" job from a given site.
func (s *MonitorSink) OnMonitorEvent(site string, ev *pb.JobEvent) {
	id := monitorIDOf(ev.JobId)
	st := s.stateFor(site, id)
	if st == nil {
		return // unknown monitor (removed between dispatch and event)
	}
	lv := []string{site, id, st.kind, st.target, st.transport}

	switch e := ev.Event.(type) {
	case *pb.JobEvent_Started:
		s.mu.Lock()
		st.resolved = e.Started.Resolved
		st.source = e.Started.Source
		if e.Started.Protocol != pb.Protocol_PROTOCOL_UNSPECIFIED {
			st.protocol = protoName(e.Started.Protocol)
		}
		s.mu.Unlock()

	case *pb.JobEvent_Cycle:
		hop, matched := targetHop(e.Cycle, st.resolvedLocked(s))
		reached := matched && hop != nil && hop.Received > 0
		s.probesTotal.WithLabelValues(lv...).Inc()
		if reached {
			s.successTotal.WithLabelValues(lv...).Inc()
			s.rttSeconds.WithLabelValues(lv...).Observe(float64(hop.AvgUs) / 1e6)
			s.up.WithLabelValues(lv...).Set(1)
		} else {
			s.up.WithLabelValues(lv...).Set(0)
		}
		loss := 1.0
		if matched && hop != nil {
			loss = hop.LossPct / 100
		}
		s.lossRatio.WithLabelValues(lv...).Set(loss)
		if st.kind == "trace" {
			s.shipCycle(site, id, st, e.Cycle)
		}

	case *pb.JobEvent_SipResult:
		r := e.SipResult
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
			s.up.WithLabelValues(lv...).Set(1)
		} else {
			s.up.WithLabelValues(lv...).Set(0)
		}
		if r.Responded && r.RttUs != nil {
			s.rttSeconds.WithLabelValues(lv...).Observe(float64(*r.RttUs) / 1e6)
		}

	case *pb.JobEvent_Error:
		// A failed probe (resolve/policy/engine): count it and mark down.
		s.probesTotal.WithLabelValues(lv...).Inc()
		s.up.WithLabelValues(lv...).Set(0)
	}
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

func (st *monState) resolvedLocked(s *MonitorSink) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return st.resolved
}

// shipCycle writes one VictoriaLogs record per hop for a trace/mtr cycle.
func (s *MonitorSink) shipCycle(site, id string, st *monState, c *pb.Cycle) {
	if s.vl == nil || !s.vl.Enabled() {
		return
	}
	for _, h := range c.Hops {
		ip, name, count := firstAddress(h)
		rec := vlog.Record{
			"_time":      vlog.Now(),
			"_msg":       "mtr " + site + " -> " + st.target + " ttl=" + strconv.Itoa(int(h.Ttl)),
			"site":       site,
			"monitor":    id,
			"target":     st.target,
			"resolved":   st.resolved,
			"source":     st.source,
			"protocol":   st.protocol,
			"cycle":      c.Number,
			"hop_ttl":    h.Ttl,
			"hop_host":   name,
			"hop_addr":   ip,
			"addr_count": count,
			"sent":       h.Sent,
			"received":   h.Received,
			"loss_pct":   h.LossPct,
			"best_ms":    usToMs(h.BestUs),
			"avg_ms":     usToMs(h.AvgUs),
			"worst_ms":   usToMs(h.WorstUs),
			"stdev_ms":   usToMs(h.StdevUs),
			"jitter_ms":  usToMs(h.JitterUs),
		}
		for k, v := range st.labels {
			if _, taken := rec[k]; !taken {
				rec[k] = v
			}
		}
		s.vl.Write(rec)
	}
}

// --- helpers ----------------------------------------------------------------

func targetHop(c *pb.Cycle, resolved string) (*pb.Hop, bool) {
	var last *pb.Hop
	for _, h := range c.Hops {
		last = h
		if resolved != "" {
			for _, a := range h.Addresses {
				if a.Ip == resolved {
					return h, true
				}
			}
		}
	}
	return last, false
}

func firstAddress(h *pb.Hop) (ip, name string, count uint32) {
	if len(h.Addresses) == 0 {
		return "", "", 0
	}
	a := h.Addresses[0]
	return a.Ip, a.Name, uint32(len(h.Addresses))
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
