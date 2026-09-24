package agent

import (
	"time"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/dns"
)

// dnsSpecFromProto turns a DnsSpec into an engine Spec. The name is not
// resolved by the caller: the lookups are the job.
func dnsSpecFromProto(t *pb.DnsSpec) dns.Spec {
	s := dns.Spec{Name: t.Name}
	if t.TimeoutMs > 0 {
		s.Timeout = time.Duration(t.TimeoutMs) * time.Millisecond
	}
	return s
}

// dnsEventToProto turns a DNS engine event into the JobEvent sent on the
// stream. It reuses JobStarted/JobFinished and adds DnsResult.
func dnsEventToProto(jobID string, seq uint64, ev dns.Event) *pb.JobEvent {
	je := &pb.JobEvent{JobId: jobID, Seq: seq, Time: newTimestamp(ev.Time)}
	switch ev.Kind {
	case dns.Started:
		je.Event = &pb.JobEvent_Started{Started: &pb.JobStarted{
			Target:      ev.Spec.Name,
			Nameservers: ev.Nameservers,
		}}
	case dns.ResultDone:
		r := ev.Result
		dr := &pb.DnsResult{
			Type:   r.Query.Type,
			Name:   r.Query.Name,
			RttUs:  uint32(r.RTT.Microseconds()),
			Status: r.Status,
			Server: r.Server,
		}
		if r.Err != nil {
			dr.Error = r.Err.Error()
		}
		for _, rec := range r.Records {
			dr.Records = append(dr.Records, &pb.DnsRecord{
				Value:    rec.Value,
				Priority: uint32(rec.Priority),
				Weight:   uint32(rec.Weight),
				Port:     uint32(rec.Port),
				Ttl:      rec.TTL,

				Addresses:     rec.Addresses,
				AddressStatus: rec.AddressStatus,
			})
		}
		je.Event = &pb.JobEvent_DnsResult{DnsResult: dr}
	case dns.Finished:
		reason := pb.JobFinished_REASON_COMPLETED
		if ev.Cancelled {
			reason = pb.JobFinished_REASON_CANCELLED
		}
		je.Event = &pb.JobEvent_Finished{Finished: &pb.JobFinished{Reason: reason}}
	}
	return je
}
