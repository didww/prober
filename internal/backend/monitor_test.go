package backend

import (
	"testing"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

func TestAssignmentForFiltersBySite(t *testing.T) {
	reg := NewMonitorRegistry([]MonitorConfig{
		{ID: "a", Kind: "trace", Target: "1.1.1.1", IntervalS: 10, Sites: []string{"fra"}},
		{ID: "b", Kind: "sip", Target: "2.2.2.2", IntervalS: 10}, // no sites = all
		{ID: "c", Kind: "ping", Target: "3.3.3.3", IntervalS: 10, Sites: []string{"ams"}},
	})

	fra := reg.AssignmentFor("fra")
	if got := monitorIDs(fra); !equalStrings(got, []string{"a", "b"}) {
		t.Fatalf("fra monitors = %v, want [a b]", got)
	}
	ams := reg.AssignmentFor("ams")
	if got := monitorIDs(ams); !equalStrings(got, []string{"b", "c"}) {
		t.Fatalf("ams monitors = %v, want [b c]", got)
	}
	if fra.Version != 1 {
		t.Fatalf("initial version = %d, want 1", fra.Version)
	}
}

func TestReplaceBumpsVersion(t *testing.T) {
	reg := NewMonitorRegistry([]MonitorConfig{{ID: "a", Kind: "ping", Target: "1.1.1.1", IntervalS: 5}})
	if v := reg.Version(); v != 1 {
		t.Fatalf("version = %d, want 1", v)
	}
	v := reg.Replace([]MonitorConfig{{ID: "a", Kind: "ping", Target: "1.1.1.1", IntervalS: 5}})
	if v != 2 || reg.Version() != 2 {
		t.Fatalf("after replace version = %d, want 2", v)
	}
}

func TestPingMapsToPingMode(t *testing.T) {
	reg := NewMonitorRegistry([]MonitorConfig{
		{ID: "p", Kind: "ping", Target: "1.1.1.1", IntervalS: 5, Trace: &TraceParams{Protocol: "icmp"}},
		{ID: "t", Kind: "trace", Target: "1.1.1.1", IntervalS: 5},
		{ID: "s", Kind: "sip", Target: "1.1.1.1", IntervalS: 5, SIP: &SipParams{Transport: "tls"}},
	})
	as := reg.AssignmentFor("any")
	byID := map[string]*pb.Monitor{}
	for _, m := range as.Monitors {
		byID[m.Id] = m
	}
	if m := byID["p"].GetTrace(); m == nil || m.Mode != pb.TraceMode_TRACE_MODE_PING {
		t.Fatalf("ping monitor mode = %v, want PING", m.GetMode())
	}
	if m := byID["t"].GetTrace(); m == nil || m.Mode != pb.TraceMode_TRACE_MODE_MTR {
		t.Fatalf("trace monitor mode = %v, want MTR", m.GetMode())
	}
	if m := byID["s"].GetSipOptions(); m == nil || m.Transport != pb.SipTransport_SIP_TRANSPORT_TLS {
		t.Fatalf("sip monitor transport = %v, want TLS", m.GetTransport())
	}
}

func monitorIDs(as *pb.Assignment) []string {
	out := make([]string, 0, len(as.Monitors))
	for _, m := range as.Monitors {
		out = append(out, m.Id)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
