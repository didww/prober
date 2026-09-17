package agent

import (
	"net/netip"
	"time"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/sip"
)

// sipSpecFromProto turns a SipOptionsSpec into an engine Spec. The target is
// already resolved to an address by the caller.
func sipSpecFromProto(t *pb.SipOptionsSpec, resolved netip.Addr) sip.Spec {
	s := sip.Spec{
		Target:    resolved,
		Port:      uint16(t.Port),
		Cycles:    int(t.Cycles),
		UserAgent: t.UserAgent,
	}
	switch t.Transport {
	case pb.SipTransport_SIP_TRANSPORT_TCP:
		s.Transport = sip.TCP
	case pb.SipTransport_SIP_TRANSPORT_TLS:
		s.Transport = sip.TLS
	case pb.SipTransport_SIP_TRANSPORT_WSS:
		s.Transport = sip.WSS
	default:
		s.Transport = sip.UDP
	}
	if t.IntervalMs > 0 {
		s.Interval = time.Duration(t.IntervalMs) * time.Millisecond
	}
	if t.TimeoutMs > 0 {
		s.Timeout = time.Duration(t.TimeoutMs) * time.Millisecond
	}
	if t.Source != "" {
		if a, err := netip.ParseAddr(t.Source); err == nil {
			s.Source = a
		}
	}
	return s
}

// sipEventToProto turns a SIP engine event into the JobEvent sent on the
// stream. It reuses JobStarted/JobFinished/JobError and adds SipResult.
func sipEventToProto(jobID string, seq uint64, ev sip.Event) *pb.JobEvent {
	je := &pb.JobEvent{JobId: jobID, Seq: seq, Time: newTimestamp(ev.Time)}
	switch ev.Kind {
	case sip.Started:
		je.Event = &pb.JobEvent_Started{Started: &pb.JobStarted{
			Target:    ev.Spec.Target.String(),
			Resolved:  ev.Spec.Target.String(),
			Source:    sourceStr(ev.Spec.Source),
			Family:    familyOf(ev.Spec.Target),
			Transport: ev.Spec.Transport.String(),
		}}
	case sip.ResultDone:
		r := ev.Result
		sr := &pb.SipResult{
			Cycle:      uint32(r.Cycle),
			StatusCode: uint32(r.StatusCode),
			Reason:     r.Reason,
			Responded:  r.Responded,
			Request:    r.Request,
			Response:   r.Response,
			Tls:        r.TLS,
			TlsValid:   r.TLSValid,
			TlsError:   r.TLSError,
		}
		if r.Responded {
			us := uint32(r.RTT.Microseconds())
			sr.RttUs = &us
		}
		je.Event = &pb.JobEvent_SipResult{SipResult: sr}
	case sip.Finished:
		reason := pb.JobFinished_REASON_COMPLETED
		if ev.Cancelled {
			reason = pb.JobFinished_REASON_CANCELLED
		}
		je.Event = &pb.JobEvent_Finished{Finished: &pb.JobFinished{Reason: reason}}
	case sip.Failed:
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		je.Event = &pb.JobEvent_Error{Error: &pb.JobError{Code: pb.JobError_CODE_ENGINE, Message: msg}}
	}
	return je
}
