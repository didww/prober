package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/vlog"
)

func TestReportsQuery(t *testing.T) {
	cases := []struct {
		name         string
		streamFields []string
		site         string
		want         string
	}{
		{"stream fields, one site", []string{"site", "monitor"}, "fra",
			`{monitor="m1",site="fra"} _time:24h | sort by (_time) desc | limit 50`},
		{"stream fields, all sites", []string{"site", "monitor"}, "",
			`{monitor="m1"} _time:24h | sort by (_time) desc | limit 50`},
		{"no stream fields", nil, "fra",
			`monitor:="m1" site:="fra" _time:24h | sort by (_time) desc | limit 50`},
		{"mixed", []string{"site"}, "fra",
			`{site="fra"} monitor:="m1" _time:24h | sort by (_time) desc | limit 50`},
	}
	for _, c := range cases {
		if got := reportsQuery(c.streamFields, "m1", c.site, "24h", 50); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	// Values are quoted, so an id with a quote or backslash cannot break out
	// of the filter.
	if got := reportsQuery([]string{"monitor"}, `we"ird\`, "", "1h", 5); !strings.HasPrefix(got, `{monitor="we\"ird\\"}`) {
		t.Errorf("unescaped id: %s", got)
	}
}

// fakeVL serves the two VictoriaLogs endpoints the backend uses: it swallows
// ingestion and answers queries with fixed lines, remembering the last query.
type fakeVL struct {
	mu    sync.Mutex
	query string
	lines []string
}

func (f *fakeVL) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/insert/jsonline":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case "/select/logsql/query":
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.query = r.Form.Get("query")
			lines := f.lines
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/stream+json")
			for _, l := range lines {
				_, _ = io.WriteString(w, l+"\n")
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeVL) lastQuery() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.query
}

func TestMonitorsAPI(t *testing.T) {
	fv := &fakeVL{lines: []string{
		`{"_time":"2026-09-24T10:00:00Z","_msg":"Start: x\nHOST: fra (10.0.0.9) ...","site":"fra","monitor":"t1","target":"1.2.3.4","resolved":"1.2.3.4","source":"10.0.0.9","reached":"true","loss_pct":"0","avg_ms":"12.5","cycles":"3","hops":"7"}`,
		`{"_time":"2026-09-24T09:00:00Z","_msg":"Start: y\n...","site":"fra","monitor":"t1","target":"1.2.3.4","reached":"false","loss_pct":"100","cycles":"3","hops":"9","error":"socket closed"}`,
	}}
	log := sinkLogger()
	vl := vlog.New(vlog.Options{URL: fv.serve(t), StreamFields: []string{"site", "monitor"}}, log)
	reg := NewMonitorRegistry([]MonitorConfig{
		{ID: "t1", Kind: "trace", Target: "1.2.3.4", IntervalS: 60, Labels: map[string]string{"supplier": "acme"},
			Trace: &TraceParams{Protocol: "icmp", Cycles: 3, MaxTTL: 30}},
		{ID: "s1", Kind: "sip", Target: "5.5.5.5", IntervalS: 10, Sites: []string{"ams", "fra"},
			SIP: &SipParams{Transport: "udp", Port: 5060}},
		{ID: "p1", Kind: "ping", Target: "8.8.8.8", IntervalS: 15},
		{ID: "q1", Kind: "ping", Target: "9.9.9.9", IntervalS: 15},
	})
	sink := NewMonitorSink(reg, vl, prometheus.NewRegistry())
	gw := NewGateway(Config{}, log)
	api := NewAPI(gw, NewManager(gw, time.Minute), nil, log, "test", "abc")
	api.SetMonitoring(reg, sink, vl)
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()

	// fra ran the trace and reached the target; ams answered the SIP probe.
	sink.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Started{
		Started: &pb.JobStarted{Resolved: "1.2.3.4", Source: "10.0.0.9", Protocol: pb.Protocol_PROTOCOL_ICMP},
	}})
	sink.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Cycle{
		Cycle: &pb.Cycle{Number: 3, ReachedAt: 2, Hops: []*pb.Hop{
			{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1"}}, Sent: 3, Received: 3, AvgUs: 1000},
			{Ttl: 2, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4"}}, Sent: 3, Received: 2, LossPct: 33.3, AvgUs: 12000},
		}},
	}})
	sink.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:t1", Event: &pb.JobEvent_Finished{Finished: &pb.JobFinished{}}})
	rtt := uint32(30000)
	sink.OnMonitorEvent("ams", &pb.JobEvent{JobId: "mon:s1", Event: &pb.JobEvent_SipResult{
		SipResult: &pb.SipResult{Cycle: 1, StatusCode: 200, Reason: "OK", Responded: true, RttUs: &rtt},
	}})
	sink.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:s1", Event: &pb.JobEvent_SipResult{
		SipResult: &pb.SipResult{Cycle: 1, Responded: false},
	}})
	sink.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:p1", Event: &pb.JobEvent_Error{
		Error: &pb.JobError{Code: pb.JobError_CODE_RESOLVE, Message: "no such host"},
	}})

	// The list is the configuration plus one aggregate per monitor.
	var list monitorListJSON
	getJSON(t, srv.URL+"/monitors", http.StatusOK, &list)
	mons := list.Monitors
	if list.Version != 1 || len(mons) != 4 || mons[0].ID != "t1" || mons[1].ID != "s1" || mons[2].ID != "p1" || mons[3].ID != "q1" {
		t.Fatalf("monitors: %+v", list)
	}
	// A monitor that never reported has no data and no "since" at all, so
	// the page cannot render the zero time as an age.
	if q := mons[3]; q.State.State != StateNoData || !q.State.Since.IsZero() {
		t.Errorf("silent monitor: %+v", q.State)
	}
	var raw struct {
		Monitors []struct {
			ID    string         `json:"id"`
			State map[string]any `json:"state"`
		} `json:"monitors"`
	}
	getJSON(t, srv.URL+"/monitors", http.StatusOK, &raw)
	if _, has := raw.Monitors[3].State["since"]; has {
		t.Errorf("silent monitor carries since: %v", raw.Monitors[3].State)
	}
	if _, has := raw.Monitors[0].State["since"]; !has {
		t.Errorf("reporting monitor lacks since: %v", raw.Monitors[0].State)
	}
	tr := mons[0]
	if tr.Kind != "trace" || !tr.History || tr.Labels["supplier"] != "acme" || tr.Trace == nil || tr.Trace.MaxTTL != 30 || len(tr.Sites) != 0 {
		t.Errorf("trace monitor: %+v", tr)
	}
	if tr.State.State != StateUp || tr.State.Up != 1 || tr.State.Sites != 1 {
		t.Errorf("trace aggregate: %+v", tr.State)
	}
	// ams answered, fra timed out: partial.
	if sp := mons[1]; sp.Kind != "sip" || sp.History || sp.SIP == nil || sp.SIP.Port != 5060 || sp.State.State != StatePartial || sp.State.Up != 1 || sp.State.Down != 1 {
		t.Errorf("sip monitor: %+v", sp)
	}
	if p := mons[2]; p.State.State != StateDown || p.State.Down != 1 {
		t.Errorf("ping monitor (error): %+v", p.State)
	}

	// The detail carries every site's outcome.
	var det monitorDetailJSON
	getJSON(t, srv.URL+"/monitors/t1", http.StatusOK, &det)
	if det.ID != "t1" || det.State.State != StateUp {
		t.Fatalf("detail: %+v", det)
	}
	// No agent is connected, so the only site listed is the one that reported.
	if len(det.Status) != 1 || det.Status[0].Site != "fra" || det.Status[0].Connected {
		t.Fatalf("trace status: %+v", det.Status)
	}
	fs := det.Status[0]
	if fs.Up == nil || !*fs.Up || fs.Stale || !fs.Reached || fs.Hops != 2 || fs.Cycles != 3 || fs.LossPct != 33.3 || fs.RTTMs == nil || *fs.RTTMs != 12 || fs.Resolved != "1.2.3.4" || fs.At == "" {
		t.Errorf("fra trace status: %+v", fs)
	}
	getJSON(t, srv.URL+"/monitors/s1", http.StatusOK, &det)
	// Configured sites are listed even without data or an agent, in order.
	if len(det.Status) != 2 || det.Status[0].Site != "ams" || det.Status[1].Site != "fra" {
		t.Fatalf("sip status: %+v", det.Status)
	}
	if a := det.Status[0]; a.Up == nil || !*a.Up || a.Code != 200 || a.Reason != "OK" || !a.Responded || a.RTTMs == nil || *a.RTTMs != 30 {
		t.Errorf("ams sip status: %+v", a)
	}
	if f := det.Status[1]; f.Up == nil || *f.Up || f.Responded || f.RTTMs != nil {
		t.Errorf("fra sip status (timeout): %+v", f)
	}
	getJSON(t, srv.URL+"/monitors/p1", http.StatusOK, &det)
	if p := det.Status; len(p) != 1 || p[0].Up == nil || *p[0].Up || p[0].Error != "no such host" || p[0].LossPct != 100 {
		t.Errorf("ping status (error): %+v", p)
	}
	getJSON(t, srv.URL+"/monitors/nope", http.StatusNotFound, nil)

	// The stream opens with a snapshot, then carries changes.
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	sreq, _ := http.NewRequestWithContext(sctx, http.MethodGet, srv.URL+"/monitors-status", nil)
	sresp, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	events := make(chan [2]string, 16)
	go func() {
		sc := bufio.NewScanner(sresp.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				events <- [2]string{name, v}
			}
		}
	}()
	nextEvent := func() [2]string {
		t.Helper()
		select {
		case ev := <-events:
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("no stream event")
		}
		return [2]string{}
	}
	if ev := nextEvent(); ev[0] != "snapshot" {
		t.Fatalf("first event: %v", ev)
	} else {
		var snap struct {
			Version uint64               `json:"version"`
			States  map[string]Aggregate `json:"states"`
		}
		if err := json.Unmarshal([]byte(ev[1]), &snap); err != nil || snap.Version != 1 || snap.States["t1"].State != StateUp || snap.States["s1"].State != StatePartial {
			t.Fatalf("snapshot: %v (%v)", ev[1], err)
		}
	}
	// fra's SIP probe answers: s1 goes from partial to up.
	sink.OnMonitorEvent("fra", &pb.JobEvent{JobId: "mon:s1", Event: &pb.JobEvent_SipResult{
		SipResult: &pb.SipResult{Cycle: 2, StatusCode: 200, Reason: "OK", Responded: true},
	}})
	if ev := nextEvent(); ev[0] != "change" {
		t.Fatalf("change event: %v", ev)
	} else {
		var ch struct {
			ID string `json:"id"`
			Aggregate
		}
		if err := json.Unmarshal([]byte(ev[1]), &ch); err != nil || ch.ID != "s1" || ch.State != StateUp || ch.Up != 2 {
			t.Fatalf("change: %v (%v)", ev[1], err)
		}
	}
	// A sweep's changes arrive as one batch: two minutes on, the 10s and 15s
	// monitors are stale (t1's 60s interval allows three minutes).
	sink.Sweep(time.Now().Add(2 * time.Minute))
	if ev := nextEvent(); ev[0] != "changes" {
		t.Fatalf("batch event: %v", ev)
	} else {
		var batch struct {
			States map[string]Aggregate `json:"states"`
		}
		if err := json.Unmarshal([]byte(ev[1]), &batch); err != nil || len(batch.States) != 2 || batch.States["s1"].State != StateNoData || batch.States["s1"].Stale != 2 || batch.States["p1"].State != StateNoData {
			t.Fatalf("batch: %v (%v)", ev[1], err)
		}
	}
	scancel()

	// History for the trace monitor comes from VictoriaLogs, newest first,
	// with the strings it returns parsed back.
	var reps []traceReportJSON
	getJSON(t, srv.URL+"/monitors/t1/reports?site=fra&range=1h&limit=10", http.StatusOK, &reps)
	if q := fv.lastQuery(); q != `{monitor="t1",site="fra"} _time:1h | sort by (_time) desc | limit 10` {
		t.Errorf("query sent: %s", q)
	}
	if len(reps) != 2 {
		t.Fatalf("reports: %+v", reps)
	}
	if r := reps[0]; r.Time != "2026-09-24T10:00:00Z" || r.Site != "fra" || !r.Reached || r.LossPct != 0 || r.AvgMs == nil || *r.AvgMs != 12.5 || r.Cycles != 3 || r.Hops != 7 || !strings.HasPrefix(r.Report, "Start: x") {
		t.Errorf("report 0: %+v", r)
	}
	if r := reps[1]; r.Reached || r.LossPct != 100 || r.AvgMs != nil || r.Error != "socket closed" {
		t.Errorf("report 1: %+v", r)
	}

	// Defaults and validation.
	getJSON(t, srv.URL+"/monitors/t1/reports", http.StatusOK, &reps)
	if q := fv.lastQuery(); q != `{monitor="t1"} _time:24h | sort by (_time) desc | limit 50` {
		t.Errorf("default query: %s", q)
	}
	getJSON(t, srv.URL+"/monitors/t1/reports?range=2h", http.StatusBadRequest, nil)
	getJSON(t, srv.URL+"/monitors/t1/reports?limit=x", http.StatusBadRequest, nil)
	getJSON(t, srv.URL+"/monitors/s1/reports", http.StatusBadRequest, nil)
	getJSON(t, srv.URL+"/monitors/nope/reports", http.StatusNotFound, nil)

	// Without VictoriaLogs the page still lists monitors, without history.
	api.SetMonitoring(reg, sink, vlog.New(vlog.Options{}, log))
	getJSON(t, srv.URL+"/monitors", http.StatusOK, &list)
	if list.Monitors[0].History {
		t.Error("history offered without victorialogs")
	}
	getJSON(t, srv.URL+"/monitors/t1/reports", http.StatusServiceUnavailable, nil)
}

func getJSON(t *testing.T, url string, wantStatus int, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: HTTP %d, want %d: %s", url, resp.StatusCode, wantStatus, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("GET %s: bad JSON: %v: %s", url, err, body)
		}
	}
}
