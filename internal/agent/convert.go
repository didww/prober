package agent

import (
	"net/netip"
	"time"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/trace"
)

// specFromProto turns a TraceSpec into an engine Spec. The target is resolved
// here, on the agent, because which resolver answers is a property of the
// site. It returns the resolved address for the Started event.
func specFromProto(t *pb.TraceSpec, resolved netip.Addr) trace.Spec {
	s := trace.Spec{
		Target:       resolved,
		Port:         uint16(t.Port),
		ResolveNames: t.ResolveNames,
		Cycles:       int(t.Cycles),
		FirstTTL:     int(t.FirstTtl),
		MaxTTL:       int(t.MaxTtl),
		PacketSize:   int(t.PacketSize),
		DSCP:         int(t.Dscp),
	}
	switch t.Protocol {
	case pb.Protocol_PROTOCOL_UDP:
		s.Protocol = trace.UDP
	case pb.Protocol_PROTOCOL_TCP:
		s.Protocol = trace.TCP
	default:
		s.Protocol = trace.ICMP
	}
	if t.Mode == pb.TraceMode_TRACE_MODE_PING {
		s.Mode = trace.Ping
	}
	if t.IntervalMs > 0 {
		s.Interval = time.Duration(t.IntervalMs) * time.Millisecond
	}
	if t.ProbeTimeoutMs > 0 {
		s.ProbeTimeout = time.Duration(t.ProbeTimeoutMs) * time.Millisecond
	}
	if t.Source != "" {
		if a, err := netip.ParseAddr(t.Source); err == nil {
			s.Source = a
		}
	}
	return s
}

// eventToProto turns an engine event into the JobEvent sent on the stream.
func eventToProto(jobID string, seq uint64, ev trace.Event) *pb.JobEvent {
	je := &pb.JobEvent{JobId: jobID, Seq: seq, Time: tspb(ev.Time)}
	switch ev.Kind {
	case trace.Started:
		je.Event = &pb.JobEvent_Started{Started: &pb.JobStarted{
			Target:   ev.Spec.Target.String(),
			Resolved: ev.Spec.Target.String(),
			Source:   sourceStr(ev.Spec.Source),
			Protocol: protoToProto(ev.Spec.Protocol),
			Family:   familyOf(ev.Spec.Target),
		}}
	case trace.CycleDone:
		je.Event = &pb.JobEvent_Cycle{Cycle: cycleToProto(ev.Cycle)}
	case trace.Finished:
		reason := pb.JobFinished_REASON_COMPLETED
		if ev.Cancelled {
			reason = pb.JobFinished_REASON_CANCELLED
		}
		je.Event = &pb.JobEvent_Finished{Finished: &pb.JobFinished{Reason: reason}}
	case trace.Failed:
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		je.Event = &pb.JobEvent_Error{Error: &pb.JobError{Code: pb.JobError_CODE_ENGINE, Message: msg}}
	}
	return je
}

func cycleToProto(c *trace.Cycle) *pb.Cycle {
	out := &pb.Cycle{Number: uint32(c.Number), ReachedAt: uint32(c.ReachedAt)}
	for _, h := range c.Hops {
		ph := &pb.Hop{
			Ttl:      uint32(h.TTL),
			Sent:     uint32(h.Sent),
			Received: uint32(h.Received),
			LossPct:  h.LossPct,
			LastUs:   us(h.Last),
			BestUs:   us(h.Best),
			AvgUs:    us(h.Avg),
			WorstUs:  us(h.Worst),
			StdevUs:  us(h.Stdev),
			JitterUs: us(h.Jitter),
		}
		if h.Sample != nil {
			s := us(*h.Sample)
			ph.SampleUs = &s
		}
		for _, a := range h.Addresses {
			ph.Addresses = append(ph.Addresses, &pb.HopAddress{Ip: a.Addr.String(), Name: a.Name, Count: uint32(a.Count)})
		}
		out.Hops = append(out.Hops, ph)
	}
	return out
}

func us(d time.Duration) uint32 { return uint32(d.Microseconds()) }

func tspb(t time.Time) *pbTimestamp { return newTimestamp(t) }

func sourceStr(a netip.Addr) string {
	if a.IsValid() {
		return a.String()
	}
	return ""
}

func protoToProto(p trace.Protocol) pb.Protocol {
	switch p {
	case trace.UDP:
		return pb.Protocol_PROTOCOL_UDP
	case trace.TCP:
		return pb.Protocol_PROTOCOL_TCP
	default:
		return pb.Protocol_PROTOCOL_ICMP
	}
}

func familyOf(a netip.Addr) pb.AddressFamily {
	if a.Is6() {
		return pb.AddressFamily_ADDRESS_FAMILY_IPV6
	}
	return pb.AddressFamily_ADDRESS_FAMILY_IPV4
}
