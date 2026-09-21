// Package vlog ships structured log records to VictoriaLogs via its JSON-stream
// ingestion endpoint (POST /insert/jsonline). It batches records and flushes on
// a size or time bound; shipping is best-effort, so a failed flush is logged and
// dropped rather than blocking the probe path.
package vlog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Record is one flat log line. It should carry "_time" (RFC3339Nano) and "_msg";
// VictoriaLogs defaults its time and message fields to those names.
type Record map[string]any

// Options configures the client. An empty URL disables shipping: Write is a
// no-op and Run returns at once.
type Options struct {
	URL          string
	Username     string
	Password     string
	AccountID    int
	ProjectID    int
	StreamFields []string
	BatchMax     int
	Flush        time.Duration
}

const (
	defaultBatchMax = 500
	defaultFlush    = 2 * time.Second
	queueDepth      = 4096
)

// Client batches records and posts them to VictoriaLogs.
type Client struct {
	base    string
	query   string
	opt     Options
	log     *slog.Logger
	http    *http.Client
	ch      chan Record
	dropped atomicCounter
}

// New builds a client. It never returns an error; if URL is empty the client is
// inert. Call Run to start the background flusher, and Write to enqueue records.
func New(opt Options, log *slog.Logger) *Client {
	if opt.BatchMax <= 0 {
		opt.BatchMax = defaultBatchMax
	}
	if opt.Flush <= 0 {
		opt.Flush = defaultFlush
	}
	q := url.Values{}
	if len(opt.StreamFields) > 0 {
		q.Set("_stream_fields", strings.Join(opt.StreamFields, ","))
	}
	return &Client{
		base:  strings.TrimRight(opt.URL, "/"),
		query: q.Encode(),
		opt:   opt,
		log:   log,
		// No Client.Timeout: a batch POST is bounded by the flush context instead.
		http: &http.Client{},
		ch:   make(chan Record, queueDepth),
	}
}

// Enabled reports whether shipping is configured.
func (c *Client) Enabled() bool { return c.base != "" }

// Write enqueues a record. It never blocks: if the queue is full the record is
// dropped and counted, so a slow or down VictoriaLogs cannot stall probing.
func (c *Client) Write(rec Record) {
	if !c.Enabled() {
		return
	}
	select {
	case c.ch <- rec:
	default:
		c.dropped.add(1)
	}
}

// Run batches queued records and flushes them until ctx is cancelled, then
// flushes what remains. Safe to call when disabled (returns immediately).
func (c *Client) Run(ctx context.Context) {
	if !c.Enabled() {
		return
	}
	t := time.NewTicker(c.opt.Flush)
	defer t.Stop()
	batch := make([]Record, 0, c.opt.BatchMax)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.post(ctx, batch)
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			// ctx is cancelled, so posting with it would fail at once. Detach
			// cancellation (keeping any values) so the final drain can still
			// flush what is buffered before returning.
			flushCtx := context.WithoutCancel(ctx)
			for {
				select {
				case rec := <-c.ch:
					batch = append(batch, rec)
					if len(batch) >= c.opt.BatchMax {
						c.post(flushCtx, batch)
						batch = batch[:0]
					}
				default:
					c.post(flushCtx, batch)
					if d := c.dropped.load(); d > 0 {
						c.log.Warn("victorialogs: dropped records (queue full)", "count", d)
					}
					return
				}
			}
		case rec := <-c.ch:
			batch = append(batch, rec)
			if len(batch) >= c.opt.BatchMax {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// post serializes a batch as NDJSON and sends it. Errors are logged and the
// batch dropped; VictoriaLogs ingestion must not become a probe dependency.
func (c *Client) post(ctx context.Context, batch []Record) {
	if len(batch) == 0 {
		return
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, rec := range batch {
		if err := enc.Encode(rec); err != nil { // Encode writes a trailing newline: NDJSON
			c.log.Warn("victorialogs: encode failed", "err", err)
			return
		}
	}
	endpoint := c.base + "/insert/jsonline"
	if c.query != "" {
		endpoint += "?" + c.query
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, &body)
	if err != nil {
		c.log.Warn("victorialogs: request build failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	c.applyAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("victorialogs: post failed", "err", err, "records", len(batch))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.log.Warn("victorialogs: non-2xx", "status", resp.StatusCode, "body", strings.TrimSpace(string(msg)))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

// applyAuth mirrors the vlui client: optional HTTP Basic plus tenant headers.
func (c *Client) applyAuth(req *http.Request) {
	if c.opt.Username != "" {
		req.SetBasicAuth(c.opt.Username, c.opt.Password)
	}
	req.Header.Set("AccountID", strconv.Itoa(c.opt.AccountID))
	req.Header.Set("ProjectID", strconv.Itoa(c.opt.ProjectID))
}

// RFC3339Nano is the timestamp format VictoriaLogs parses for _time.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// atomicCounter is a tiny lock-guarded counter (dropped records).
type atomicCounter struct {
	mu sync.Mutex
	n  int64
}

func (a *atomicCounter) add(n int64) { a.mu.Lock(); a.n += n; a.mu.Unlock() }
func (a *atomicCounter) load() int64 { a.mu.Lock(); defer a.mu.Unlock(); return a.n }
