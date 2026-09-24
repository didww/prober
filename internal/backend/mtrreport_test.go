package backend

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// TestMtrReport pins the report text to the mtr --report layout the web UI's
// "Copy as text" produces: a Start line, a HOST line naming the vantage, and
// one row per hop with the host padded to the widest and eight right-aligned
// six-character columns.
func TestMtrReport(t *testing.T) {
	start := time.Date(2026, 9, 16, 23, 45, 12, 0, time.FixedZone("EEST", 3*3600))
	hops := []*pb.Hop{
		{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1"}}, Sent: 3, Received: 3,
			LastUs: 1000, AvgUs: 1100, BestUs: 900, WorstUs: 1500, StdevUs: 100},
		{Ttl: 2, Addresses: []*pb.HopAddress{{Ip: "1.2.3.4", Name: "core.example.net"}}, Sent: 3, Received: 2, LossPct: 33.3,
			LastUs: 12000, AvgUs: 12500, BestUs: 12000, WorstUs: 13000, StdevUs: 500},
		{Ttl: 3, Sent: 3, Received: 0, LossPct: 100},
	}
	want := "Start: 2026-09-16T23:45:12+0300\nHOST: fra (10.0.0.9)         Loss%   Snt  Rcvd  Last   Avg  Best  Wrst StDev\n  1.|-- 10.0.0.1              0.0%     3     3   1.0   1.1   0.9   1.5   0.1\n  2.|-- core.example.net     33.3%     3     2  12.0  12.5  12.0  13.0   0.5\n  3.|-- ???                 100.0%     3     0   0.0   0.0   0.0   0.0   0.0"
	if got := mtrReport("fra", "10.0.0.9", start, hops); got != want {
		t.Fatalf("report mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Without a source the HOST line is just the site, and a long site name
	// widens the host column.
	got := mtrReport("a-rather-long-site-name-here", "", start, hops[:1])
	wantHost := "HOST: a-rather-long-site-name-here   Loss%   Snt  Rcvd  Last   Avg  Best  Wrst StDev"
	wantRow := "  1.|-- 10.0.0.1                      0.0%     3     3   1.0   1.1   0.9   1.5   0.1"
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || lines[1] != wantHost || lines[2] != wantRow {
		t.Fatalf("wide site report:\n%s", got)
	}

	// Widths count characters, so a non-ASCII name keeps every column in the
	// same place as the header.
	got = mtrReport("zürich", "", start, []*pb.Hop{
		{Ttl: 1, Addresses: []*pb.HopAddress{{Ip: "10.0.0.1", Name: "gw-münchen.example.net"}}, Sent: 1, Received: 1},
	})
	lines = strings.Split(got, "\n")
	if len(lines) != 3 || utf8.RuneCountInString(lines[1]) != utf8.RuneCountInString(lines[2]) ||
		!strings.HasSuffix(lines[1], " StDev") || !strings.HasSuffix(lines[2], "   0.0") {
		t.Fatalf("non-ASCII report misaligned:\n%s", got)
	}
}
