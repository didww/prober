package vlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestClientPostsNDJSON(t *testing.T) {
	var (
		mu       sync.Mutex
		lines    []map[string]any
		gotPath  string
		gotQuery string
		gotCT    string
		gotAcct  string
		reqs     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		reqs++
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotCT = r.Header.Get("Content-Type")
		gotAcct = r.Header.Get("AccountID")
		sc := bufio.NewScanner(strings.NewReader(string(body)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Errorf("bad NDJSON line %q: %v", line, err)
				continue
			}
			lines = append(lines, m)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Options{
		URL:          srv.URL,
		StreamFields: []string{"site", "monitor"},
		BatchMax:     2,
		Flush:        20 * time.Millisecond,
		AccountID:    7,
	}, testLogger())
	if !c.Enabled() {
		t.Fatal("client should be enabled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	for i := 0; i < 3; i++ {
		c.Write(Record{"_time": Now(), "_msg": "hop", "site": "fra", "monitor": "m1", "n": i})
	}
	// Let the size-based flush (batch of 2) and a tick fire, then stop for the
	// final drain of the remaining record.
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 3 {
		t.Fatalf("got %d records, want 3", len(lines))
	}
	if gotPath != "/insert/jsonline" {
		t.Errorf("path = %q, want /insert/jsonline", gotPath)
	}
	if !strings.Contains(gotQuery, "_stream_fields=site%2Cmonitor") {
		t.Errorf("query = %q, want _stream_fields=site,monitor", gotQuery)
	}
	if gotCT != "application/x-ndjson" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotAcct != "7" {
		t.Errorf("AccountID header = %q, want 7", gotAcct)
	}
	if reqs < 2 {
		t.Errorf("expected at least 2 requests (a size flush + a drain), got %d", reqs)
	}
}

func TestDisabledClientIsNoop(t *testing.T) {
	c := New(Options{URL: ""}, testLogger())
	if c.Enabled() {
		t.Fatal("empty URL should be disabled")
	}
	c.Write(Record{"_msg": "x"}) // must not panic or block
	c.Run(context.Background())  // must return immediately
}

func TestClientQuery(t *testing.T) {
	var gotPath, gotQuery, gotAccount, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = r.ParseForm()
		gotQuery = r.Form.Get("query")
		gotAccount = r.Header.Get("AccountID")
		gotUser, _, _ = r.BasicAuth()
		// Two lines, one typed value to prove flattening, then a trailing newline.
		_, _ = io.WriteString(w, `{"_time":"2026-09-24T10:00:00Z","_msg":"first","n":"1"}`+"\n")
		_, _ = io.WriteString(w, `{"_time":"2026-09-24T09:00:00Z","_msg":"second","n":2,"ok":true}`+"\n")
	}))
	defer srv.Close()

	c := New(Options{URL: srv.URL + "/", Username: "u", Password: "p", AccountID: 7}, testLogger())
	recs, err := c.Query(context.Background(), `{monitor="m"} _time:1h | limit 2`)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/select/logsql/query" || gotQuery != `{monitor="m"} _time:1h | limit 2` || gotAccount != "7" || gotUser != "u" {
		t.Errorf("request: path=%q query=%q account=%q user=%q", gotPath, gotQuery, gotAccount, gotUser)
	}
	if len(recs) != 2 || recs[0]["_msg"] != "first" || recs[1]["_msg"] != "second" || recs[1]["n"] != "2" || recs[1]["ok"] != "true" {
		t.Errorf("records: %+v", recs)
	}

	// A non-2xx is an error carrying the body; a disabled client refuses.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cannot parse query", http.StatusBadRequest)
	}))
	defer bad.Close()
	if _, err := New(Options{URL: bad.URL}, testLogger()).Query(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "cannot parse query") {
		t.Errorf("bad status: err = %v", err)
	}
	if _, err := New(Options{}, testLogger()).Query(context.Background(), "x"); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled: err = %v", err)
	}
}
