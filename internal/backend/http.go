package backend

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/auth"
	"github.com/didww/prober/internal/vlog"
)

// API is the browser-facing HTTP surface: the prober list, starting and
// cancelling runs, and the SSE event stream. Auth (OIDC) is not wired here
// yet.
type API struct {
	gw      *Gateway
	mgr     *Manager
	auth    *auth.Auth // nil when authentication is disabled
	log     *slog.Logger
	version string
	commit  string

	// Monitoring, for the monitors page. Set via SetMonitoring; nil means
	// the page lists nothing.
	reg  *MonitorRegistry
	sink *MonitorSink
	vl   *vlog.Client
}

func NewAPI(gw *Gateway, mgr *Manager, a *auth.Auth, log *slog.Logger, version, commit string) *API {
	return &API{gw: gw, mgr: mgr, auth: a, log: log, version: version, commit: commit}
}

// SetMonitoring wires the monitor registry, the sink holding each site's latest
// outcome, and the VictoriaLogs client the trace history is read from.
func (a *API) SetMonitoring(reg *MonitorRegistry, sink *MonitorSink, vl *vlog.Client) {
	a.reg, a.sink, a.vl = reg, sink, vl
}

func (a *API) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	// JSON compresses about tenfold; the monitor list is the one that
	// matters, at hundreds of kilobytes for a large configuration. Event
	// streams are not in the compressed types and pass through untouched.
	r.Use(middleware.Compress(5))

	// version and config are unauthenticated: the SPA shell asks who it is
	// talking to (config.user) before it shows anything, and the version feeds
	// the rail even on the login screen.
	r.Get("/version", a.versionInfo)
	r.Get("/config", a.appConfig)

	// The OIDC endpoints sit OUTSIDE the middleware — logging in cannot require
	// being logged in.
	if a.auth != nil {
		r.Mount("/auth", a.auth.Routes())
	}

	// Everything else is behind the session, when auth is on.
	r.Group(func(r chi.Router) {
		if a.auth != nil {
			r.Use(a.auth.Middleware)
		}
		r.Get("/probers", a.probers)
		r.Get("/agents", a.agents)
		r.Get("/monitors", a.listMonitors)
		// Outside /monitors/{id} so no monitor id can shadow it.
		r.Get("/monitors-status", a.monitorStatusStream)
		r.Get("/monitors/{id}", a.monitorDetail)
		r.Get("/monitors/{id}/reports", a.monitorReports)
		r.Post("/runs", a.startRun)
		r.Post("/sip-runs", a.startSipRun)
		r.Post("/dns-runs", a.startDnsRun)
		r.Get("/runs/{id}/events", a.runEvents)
		r.Delete("/runs/{id}", a.cancelRun)
	})
	return r
}

// appConfig is what the SPA fetches at boot: whether auth is on and, if so, who
// the current user is. Reads the cookie directly rather than the middleware,
// since it is unauthenticated.
func (a *API) appConfig(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Version     string     `json:"version"`
		Commit      string     `json:"commit"`
		AuthEnabled bool       `json:"auth_enabled"`
		User        *auth.User `json:"user"`
	}{Version: a.version, Commit: a.commit, AuthEnabled: a.auth != nil}
	if a.auth != nil {
		if u, ok := a.auth.Session(r); ok {
			out.User = &u
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) versionInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": a.version, "commit": a.commit})
}

func (a *API) probers(w http.ResponseWriter, r *http.Request) {
	type prober struct {
		Site     string `json:"site"`
		Version  string `json:"version"`
		Hostname string `json:"hostname"`
		IPv4     bool   `json:"ipv4"`
		IPv6     bool   `json:"ipv6"`
	}
	var out []prober
	for _, h := range a.gw.Sites() {
		p := prober{Site: h.Site, Version: h.Version, Hostname: h.Hostname}
		if c := h.Capabilities; c != nil {
			p.IPv4, p.IPv6 = c.Ipv4, c.Ipv6
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b prober) int { return strings.Compare(a.Site, b.Site) })
	writeJSON(w, http.StatusOK, out)
}

// agents is the detailed view for the Agents page: every connected agent with
// version, hostname, uptime and connection time, and the source addresses its
// probes leave from.
func (a *API) agents(w http.ResponseWriter, r *http.Request) {
	type agent struct {
		Site        string   `json:"site"`
		Version     string   `json:"version"`
		Commit      string   `json:"commit"`
		Hostname    string   `json:"hostname"`
		IPv4        bool     `json:"ipv4"`
		IPv6        bool     `json:"ipv6"`
		Sources     []string `json:"sources"`
		StartedAt   string   `json:"started_at"`
		ConnectedAt string   `json:"connected_at"`
		RTTMicros   *int64   `json:"rtt_us"`
	}
	out := []agent{}
	for _, info := range a.gw.Agents() {
		h := info.Hello
		ag := agent{
			Site:        h.Site,
			Version:     h.Version,
			Commit:      h.Commit,
			Hostname:    h.Hostname,
			ConnectedAt: info.ConnectedAt.UTC().Format(time.RFC3339),
		}
		if c := h.Capabilities; c != nil {
			ag.IPv4, ag.IPv6, ag.Sources = c.Ipv4, c.Ipv6, c.SourceAddresses
		}
		if h.StartedAt != nil {
			ag.StartedAt = h.StartedAt.AsTime().UTC().Format(time.RFC3339)
		}
		if info.RTTMicros >= 0 {
			rtt := info.RTTMicros
			ag.RTTMicros = &rtt
		}
		out = append(out, ag)
	}
	slices.SortFunc(out, func(a, b agent) int {
		if c := strings.Compare(a.Site, b.Site); c != 0 {
			return c
		}
		return strings.Compare(a.Hostname, b.Hostname)
	})
	writeJSON(w, http.StatusOK, out)
}

type startRequest struct {
	Target   string   `json:"target"`
	Sites    []string `json:"sites"`
	Protocol string   `json:"protocol"`
	Family   string   `json:"family"`
	Port     uint32   `json:"port"`
	Resolve  bool     `json:"resolve_names"`
	Cycles   uint32   `json:"cycles"`
	Mode     string   `json:"mode"`
}

func (a *API) startRun(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Target == "" {
		writeErr(w, http.StatusBadRequest, "target is required")
		return
	}
	sites, ok := a.runSites(w, req.Sites)
	if !ok {
		return
	}

	spec := &pb.TraceSpec{
		Target:       req.Target,
		Protocol:     protoOf(req.Protocol),
		Family:       familyOf(req.Family),
		Port:         req.Port,
		ResolveNames: req.Resolve,
		Cycles:       req.Cycles,
		Mode:         modeOf(req.Mode),
	}
	run := a.mgr.Start(func(jobID string) *pb.StartJob {
		return &pb.StartJob{JobId: jobID, Spec: &pb.StartJob_Trace{Trace: spec}}
	}, sites)
	writeJSON(w, http.StatusCreated, map[string]any{"id": run.ID, "sites": sites})
}

type sipStartRequest struct {
	Target     string   `json:"target"`
	Sites      []string `json:"sites"`
	Transport  string   `json:"transport"`
	Family     string   `json:"family"`
	Port       uint32   `json:"port"`
	Cycles     uint32   `json:"cycles"`
	IntervalMS uint32   `json:"interval_ms"`
	TimeoutMS  uint32   `json:"timeout_ms"`
}

func (a *API) startSipRun(w http.ResponseWriter, r *http.Request) {
	var req sipStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Target == "" {
		writeErr(w, http.StatusBadRequest, "target is required")
		return
	}
	sites, ok := a.runSites(w, req.Sites)
	if !ok {
		return
	}
	spec := &pb.SipOptionsSpec{
		Target:     req.Target,
		Port:       req.Port,
		Transport:  sipTransportOf(req.Transport),
		Family:     familyOf(req.Family),
		Cycles:     req.Cycles,
		IntervalMs: req.IntervalMS,
		TimeoutMs:  req.TimeoutMS,
	}
	run := a.mgr.Start(func(jobID string) *pb.StartJob {
		return &pb.StartJob{JobId: jobID, Spec: &pb.StartJob_SipOptions{SipOptions: spec}}
	}, sites)
	writeJSON(w, http.StatusCreated, map[string]any{"id": run.ID, "sites": sites})
}

type dnsStartRequest struct {
	Name      string   `json:"name"`
	Sites     []string `json:"sites"`
	TimeoutMS uint32   `json:"timeout_ms"`
}

func (a *API) startDnsRun(w http.ResponseWriter, r *http.Request) {
	var req dnsStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	sites, ok := a.runSites(w, req.Sites)
	if !ok {
		return
	}
	spec := &pb.DnsSpec{Name: req.Name, TimeoutMs: req.TimeoutMS}
	run := a.mgr.Start(func(jobID string) *pb.StartJob {
		return &pb.StartJob{JobId: jobID, Spec: &pb.StartJob_Dns{Dns: spec}}
	}, sites)
	writeJSON(w, http.StatusCreated, map[string]any{"id": run.ID, "sites": sites})
}

// runSites is the sites a run fans out to: those requested, or every connected
// site when none were. When there is nothing to run on it writes the error
// response and reports false.
func (a *API) runSites(w http.ResponseWriter, requested []string) ([]string, bool) {
	sites := requested
	if len(sites) == 0 {
		for _, h := range a.gw.Sites() {
			sites = append(sites, h.Site)
		}
	}
	if len(sites) == 0 {
		writeErr(w, http.StatusServiceUnavailable, "no agents connected")
		return nil, false
	}
	return sites, true
}

func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	a.mgr.Cancel(chi.URLParam(r, "id"))
	w.WriteHeader(http.StatusNoContent)
}

// runEvents streams a run's events as SSE, replaying from Last-Event-ID.
func (a *API) runEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := a.mgr.Get(chi.URLParam(r, "id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such run")
		return
	}
	var after uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseUint(v, 10, 64)
	} else if v := r.URL.Query().Get("last_event_id"); v != "" {
		after, _ = strconv.ParseUint(v, 10, 64)
	}

	flusher, ok := startSSE(w)
	if !ok {
		return
	}
	backlog, live, cancel := run.Subscribe(after)
	defer cancel()

	for _, se := range backlog {
		writeSSE(w, se)
	}
	flusher.Flush()

	ping := time.NewTicker(sseKeepalive)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case se, ok := <-live:
			if !ok {
				writeSSEFrame(w, 0, "done", []byte("{}"))
				flusher.Flush()
				return
			}
			writeSSE(w, se)
			flusher.Flush()
		case <-ping.C:
			writeSSEComment(w, flusher)
		}
	}
}

// --- server-sent events ---------------------------------------------------------

// sseKeepalive is how often an idle stream sends a comment, so proxies and
// browsers see it alive.
const sseKeepalive = 15 * time.Second

// startSSE begins an event stream response: the headers that stop proxies
// buffering it, then the status. It writes the error itself when the server
// cannot stream.
func startSSE(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return flusher, true
}

// writeSSEFrame writes one event. An id of 0 means none: the stream has no
// replay and the browser should not send Last-Event-ID.
func writeSSEFrame(w http.ResponseWriter, id uint64, name string, data []byte) {
	if id != 0 {
		fmt.Fprintf(w, "id: %d\n", id)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}

func writeSSEComment(w http.ResponseWriter, flusher http.Flusher) {
	fmt.Fprintf(w, ": keepalive\n\n")
	flusher.Flush()
}

func writeSSE(w http.ResponseWriter, se *StreamEvent) {
	data, typ, _ := eventJSON(se)
	writeSSEFrame(w, se.Seq, typ, data)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func protoOf(s string) pb.Protocol {
	switch s {
	case "tcp", "TCP":
		return pb.Protocol_PROTOCOL_TCP
	case "udp", "UDP":
		return pb.Protocol_PROTOCOL_UDP
	default:
		return pb.Protocol_PROTOCOL_ICMP
	}
}

func familyOf(s string) pb.AddressFamily {
	switch s {
	case "4", "ipv4":
		return pb.AddressFamily_ADDRESS_FAMILY_IPV4
	case "6", "ipv6":
		return pb.AddressFamily_ADDRESS_FAMILY_IPV6
	default:
		return pb.AddressFamily_ADDRESS_FAMILY_UNSPECIFIED
	}
}

func modeOf(s string) pb.TraceMode {
	if s == "ping" {
		return pb.TraceMode_TRACE_MODE_PING
	}
	return pb.TraceMode_TRACE_MODE_MTR
}

func sipTransportOf(s string) pb.SipTransport {
	switch s {
	case "tcp":
		return pb.SipTransport_SIP_TRANSPORT_TCP
	case "tls":
		return pb.SipTransport_SIP_TRANSPORT_TLS
	case "wss":
		return pb.SipTransport_SIP_TRANSPORT_WSS
	default:
		return pb.SipTransport_SIP_TRANSPORT_UDP
	}
}
