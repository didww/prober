package backend

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// mtrReport renders a cycle's hops the way `mtr --report` prints them, in the
// layout the web UI's "Copy as text" uses (web/src/format.ts), so a log record
// reads like a pasted report. It differs in two small ways: the Start stamp
// is in UTC, and there is no trailing newline. The HOST line carries the site
// and the source address, since that is the vantage the trace ran from.
// Widths are in characters, not bytes, so a non-ASCII name still lines up.
func mtrReport(site, source string, start time.Time, hops []*pb.Hop) string {
	host := func(h *pb.Hop) string {
		if len(h.Addresses) == 0 {
			return "???"
		}
		if a := h.Addresses[0]; a.Name != "" {
			return a.Name
		}
		return h.Addresses[0].Ip
	}
	src := site
	if source != "" {
		src = site + " (" + source + ")"
	}
	hostW := max(20, utf8.RuneCountInString(src))
	for _, h := range hops {
		hostW = max(hostW, utf8.RuneCountInString(host(h)))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Start: %s\n", start.Format("2006-01-02T15:04:05-0700"))
	fmt.Fprintf(&b, "%-*s%6s%6s%6s%6s%6s%6s%6s%6s", 8+hostW, "HOST: "+src,
		"Loss%", "Snt", "Rcvd", "Last", "Avg", "Best", "Wrst", "StDev")
	for _, h := range hops {
		fmt.Fprintf(&b, "\n%3d.|-- %-*s%6s%6d%6d%6.1f%6.1f%6.1f%6.1f%6.1f",
			h.Ttl, hostW, host(h), fmt.Sprintf("%.1f%%", h.LossPct), h.Sent, h.Received,
			usToMs(h.LastUs), usToMs(h.AvgUs), usToMs(h.BestUs), usToMs(h.WorstUs), usToMs(h.StdevUs))
	}
	return b.String()
}
