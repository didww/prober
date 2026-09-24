package backend

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// The monitors page API: every configured monitor with what it probes and each
// site's latest outcome, and for trace monitors the reports shipped to
// VictoriaLogs.

type monitorJSON struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	IntervalS uint32 `json:"interval_s"`
	// Sites as configured; empty means every connected agent runs it.
	Sites  []string          `json:"sites"`
	Labels map[string]string `json:"labels"`
	Trace  *TraceParams      `json:"trace,omitempty"`
	SIP    *SipParams        `json:"sip,omitempty"`
	// History is whether reports can be fetched: a trace monitor with
	// VictoriaLogs configured.
	History bool             `json:"history"`
	Status  []siteStatusJSON `json:"status"`
}

// siteStatusJSON is one site's latest outcome. Up is null and At empty until
// the site has reported a result.
type siteStatusJSON struct {
	Site      string   `json:"site"`
	Connected bool     `json:"connected"`
	At        string   `json:"at"`
	Up        *bool    `json:"up"`
	Resolved  string   `json:"resolved"`
	Source    string   `json:"source"`
	LossPct   float64  `json:"loss_pct"`
	RTTMs     *float64 `json:"rtt_ms"`
	Reached   bool     `json:"reached"`
	Cycles    uint32   `json:"cycles"`
	Hops      int      `json:"hops"`
	Code      int      `json:"code"`
	Reason    string   `json:"reason"`
	Responded bool     `json:"responded"`
	Error     string   `json:"error"`
}

func (a *API) listMonitors(w http.ResponseWriter, r *http.Request) {
	out := []monitorJSON{}
	if a.reg == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	connected := map[string]bool{}
	for _, h := range a.gw.Sites() {
		connected[h.Site] = true
	}
	history := a.vl != nil && a.vl.Enabled()
	statuses := map[string][]MonitorStatus{}
	if a.sink != nil {
		statuses = a.sink.AllStatuses()
	}

	for _, m := range a.reg.List() {
		mj := monitorJSON{
			ID: m.ID, Kind: monitorKind(m), Target: m.Target, IntervalS: m.IntervalS,
			Sites: m.Sites, Labels: m.Labels, Trace: m.Trace, SIP: m.SIP,
			History: history && monitorKind(m) == "trace",
			Status:  []siteStatusJSON{},
		}
		if mj.Sites == nil {
			mj.Sites = []string{}
		}
		if mj.Labels == nil {
			mj.Labels = map[string]string{}
		}

		// The sites the monitor runs at: the configured ones, else every
		// connected agent; plus any site that has reported, so one whose
		// agent has since gone still shows its last outcome.
		sites := map[string]bool{}
		if len(m.Sites) > 0 {
			for _, s := range m.Sites {
				sites[s] = true
			}
		} else {
			for s := range connected {
				sites[s] = true
			}
		}
		latest := map[string]MonitorStatus{}
		for _, st := range statuses[m.ID] {
			latest[st.Site] = st
			sites[st.Site] = true
		}
		names := make([]string, 0, len(sites))
		for s := range sites {
			names = append(names, s)
		}
		slices.Sort(names)
		for _, site := range names {
			sj := siteStatusJSON{Site: site, Connected: connected[site]}
			if st, ok := latest[site]; ok {
				up := st.Up
				sj.Up = &up
				sj.At = st.At.UTC().Format(time.RFC3339)
				sj.Resolved, sj.Source = st.Resolved, st.Source
				sj.LossPct = st.LossPct
				if st.HasRTT {
					rtt := usToMs(st.RTTUs)
					sj.RTTMs = &rtt
				}
				sj.Reached, sj.Cycles, sj.Hops = st.Reached, st.Cycles, st.Hops
				sj.Code, sj.Reason, sj.Responded = st.Code, st.Reason, st.Responded
				sj.Error = st.Error
			}
			mj.Status = append(mj.Status, sj)
		}
		out = append(out, mj)
	}
	writeJSON(w, http.StatusOK, out)
}

// traceReportJSON is one shipped trace as the history panel shows it: the
// summary fields for the row, the mtr text for the expanded view.
type traceReportJSON struct {
	Time     string   `json:"time"`
	Site     string   `json:"site"`
	Target   string   `json:"target"`
	Resolved string   `json:"resolved"`
	Source   string   `json:"source"`
	Reached  bool     `json:"reached"`
	LossPct  float64  `json:"loss_pct"`
	AvgMs    *float64 `json:"avg_ms"`
	Cycles   int      `json:"cycles"`
	Hops     int      `json:"hops"`
	Error    string   `json:"error"`
	Report   string   `json:"report"`
}

// reportRanges are the look-back windows the history panel offers, in LogsQL
// duration syntax. A fixed set keeps the query shape predictable.
var reportRanges = map[string]bool{"1h": true, "6h": true, "24h": true, "7d": true, "30d": true}

const (
	defaultReportLimit = 50
	maxReportLimit     = 500
)

func (a *API) monitorReports(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.reg == nil {
		writeErr(w, http.StatusNotFound, "no such monitor")
		return
	}
	m, ok := a.reg.lookup(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such monitor")
		return
	}
	if monitorKind(m) != "trace" {
		writeErr(w, http.StatusBadRequest, "only trace monitors have reports")
		return
	}
	if a.vl == nil || !a.vl.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "victorialogs is not configured")
		return
	}

	q := r.URL.Query()
	rng := q.Get("range")
	if rng == "" {
		rng = "24h"
	}
	if !reportRanges[rng] {
		writeErr(w, http.StatusBadRequest, "range must be one of 1h, 6h, 24h, 7d, 30d")
		return
	}
	limit := defaultReportLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, maxReportLimit)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	recs, err := a.vl.Query(ctx, reportsQuery(a.vl.StreamFields(), id, q.Get("site"), rng, limit))
	if err != nil {
		// The detail (URL, LogsQL) is for the log, not the browser.
		a.log.Warn("victorialogs query failed", "monitor", id, "err", err)
		writeErr(w, http.StatusBadGateway, "victorialogs query failed; see the backend log")
		return
	}
	out := make([]traceReportJSON, 0, len(recs))
	for _, rec := range recs {
		out = append(out, reportFromRecord(rec))
	}
	writeJSON(w, http.StatusOK, out)
}

// reportsQuery builds the LogsQL for a monitor's reports, newest first. Fields
// the records are keyed by on ingestion go in the stream filter, which is the
// indexed path; any other goes in an exact filter. site is optional.
func reportsQuery(streamFields []string, id, site, rng string, limit int) string {
	isStream := map[string]bool{}
	for _, f := range streamFields {
		isStream[f] = true
	}
	fields := [][2]string{{"monitor", id}}
	if site != "" {
		fields = append(fields, [2]string{"site", site})
	}
	var stream, plain []string
	for _, f := range fields {
		if isStream[f[0]] {
			stream = append(stream, f[0]+"="+strconv.Quote(f[1]))
		} else {
			plain = append(plain, f[0]+":="+strconv.Quote(f[1]))
		}
	}
	var parts []string
	if len(stream) > 0 {
		parts = append(parts, "{"+strings.Join(stream, ",")+"}")
	}
	parts = append(parts, plain...)
	parts = append(parts, "_time:"+rng)
	return strings.Join(parts, " ") + " | sort by (_time) desc | limit " + strconv.Itoa(limit)
}

// reportFromRecord reads a shipped record back into a report. VictoriaLogs
// returns every value as a string; a field that is absent or unparsable is
// left at its zero value.
func reportFromRecord(rec map[string]string) traceReportJSON {
	out := traceReportJSON{
		Time: rec["_time"], Site: rec["site"], Target: rec["target"],
		Resolved: rec["resolved"], Source: rec["source"], Error: rec["error"], Report: rec["_msg"],
	}
	out.Reached = rec["reached"] == "true"
	out.LossPct, _ = strconv.ParseFloat(rec["loss_pct"], 64)
	if v, err := strconv.ParseFloat(rec["avg_ms"], 64); err == nil {
		out.AvgMs = &v
	}
	out.Cycles, _ = strconv.Atoi(rec["cycles"])
	out.Hops, _ = strconv.Atoi(rec["hops"])
	return out
}
