package sip

import (
	"context"
	"log/slog"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// UseLogger routes sipgo's own log output, which otherwise goes to the
// process default logger in its own format, through log, with one known
// artefact dropped.
//
// Every probe run builds and closes its own user agent. Closing it
// hard-closes the probe's UDP socket and zeroes the connection's reference
// count, then the socket's reader goroutine wakes on the closed socket and
// decrements once more, and sipgo warns "ref went negative on try close".
// Nothing is left open; it is a bookkeeping quirk of the library's pool. At
// hundreds of probes a minute the line would drown the agent log, so it is
// filtered here rather than surfaced.
func UseLogger(log *slog.Logger) {
	sip.SetDefaultLogger(slog.New(&quietHandler{Handler: log.Handler()}))
}

// quietHandler drops the reference-count warning and passes everything else
// to the wrapped handler.
type quietHandler struct {
	slog.Handler
}

func (h *quietHandler) Handle(ctx context.Context, r slog.Record) error {
	// The wording has shifted between library versions ("UDP ref went
	// negative", then "... on try close"); match the part that has not.
	if strings.Contains(r.Message, "went negative") {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h *quietHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &quietHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *quietHandler) WithGroup(name string) slog.Handler {
	return &quietHandler{Handler: h.Handler.WithGroup(name)}
}
