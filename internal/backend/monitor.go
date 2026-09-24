package backend

import (
	"sync"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// MonitorRegistry holds the current set of predefined monitors and a version
// that increments on every reload. Agents receive the whole list as a versioned
// Assignment; they replace their schedule when the version changes, so a reload
// is a single atomic swap here plus a re-push to connected agents.
type MonitorRegistry struct {
	mu      sync.RWMutex
	version uint64
	byID    map[string]MonitorConfig
	order   []string // stable order for deterministic assignments
}

// NewMonitorRegistry builds a registry from the initial config. The first
// version is 1 (0 means "no assignment sent yet" on the agent side).
func NewMonitorRegistry(mons []MonitorConfig) *MonitorRegistry {
	r := &MonitorRegistry{}
	r.set(mons, 1)
	return r
}

func (r *MonitorRegistry) set(mons []MonitorConfig, version uint64) {
	byID := make(map[string]MonitorConfig, len(mons))
	order := make([]string, 0, len(mons))
	for _, m := range mons {
		byID[m.ID] = m
		order = append(order, m.ID)
	}
	r.version = version
	r.byID = byID
	r.order = order
}

// Replace swaps in a new monitor list and bumps the version, returning the new
// version. Called on SIGHUP reload.
func (r *MonitorRegistry) Replace(mons []MonitorConfig) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set(mons, r.version+1)
	return r.version
}

// Version is the current assignment version.
func (r *MonitorRegistry) Version() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
}

// List returns every configured monitor, in config order.
func (r *MonitorRegistry) List() []MonitorConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]MonitorConfig, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// lookup returns a monitor's config by id (for the metrics/log sink).
func (r *MonitorRegistry) lookup(id string) (MonitorConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byID[id]
	return m, ok
}

// AssignmentFor builds the assignment for one site: every monitor whose Sites
// list is empty or contains the site, in stable order.
func (r *MonitorRegistry) AssignmentFor(site string) *pb.Assignment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := &pb.Assignment{Version: r.version}
	for _, id := range r.order {
		m := r.byID[id]
		if !siteMatches(m.Sites, site) {
			continue
		}
		out.Monitors = append(out.Monitors, monitorToProto(m))
	}
	return out
}

func siteMatches(sites []string, site string) bool {
	if len(sites) == 0 {
		return true
	}
	for _, s := range sites {
		if s == site {
			return true
		}
	}
	return false
}

// monitorToProto converts a config monitor to the wire Monitor. A "ping" monitor
// is a TraceSpec with mode PING; "trace" is mode MTR; "sip" is a SipOptionsSpec.
func monitorToProto(m MonitorConfig) *pb.Monitor {
	pm := &pb.Monitor{
		Id:        m.ID,
		IntervalS: m.IntervalS,
		Labels:    m.Labels,
	}
	switch m.Kind {
	case "sip":
		sp := m.SIP
		if sp == nil {
			sp = &SipParams{}
		}
		pm.Spec = &pb.Monitor_SipOptions{SipOptions: &pb.SipOptionsSpec{
			Target:     m.Target,
			Port:       sp.Port,
			Transport:  sipTransportOf(sp.Transport),
			Family:     familyOf(sp.Family),
			Cycles:     sp.Cycles,
			IntervalMs: sp.IntervalMS,
			TimeoutMs:  sp.TimeoutMS,
		}}
	default: // trace or ping
		tp := m.Trace
		if tp == nil {
			tp = &TraceParams{}
		}
		mode := pb.TraceMode_TRACE_MODE_MTR
		if m.Kind == "ping" {
			mode = pb.TraceMode_TRACE_MODE_PING
		}
		pm.Spec = &pb.Monitor_Trace{Trace: &pb.TraceSpec{
			Target:       m.Target,
			Protocol:     protoOf(tp.Protocol),
			Family:       familyOf(tp.Family),
			Port:         tp.Port,
			ResolveNames: tp.ResolveNames,
			Cycles:       tp.Cycles,
			IntervalMs:   tp.IntervalMS,
			FirstTtl:     tp.FirstTTL,
			MaxTtl:       tp.MaxTTL,
			Mode:         mode,
		}}
	}
	return pm
}
