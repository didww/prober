package backend

import (
	"encoding/json"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// eventJSON is the on-the-wire shape of an SSE event: a flat, discriminated
// union keyed by "type", which is what the TypeScript client consumes. It is
// hand-built rather than protojson so the field names and the timeline shape
// are the frontend's contract, not the proto's.
func eventJSON(se *StreamEvent) ([]byte, string, []byte) {
	ev := se.Event
	out := map[string]any{
		"seq":  se.Seq,
		"site": se.Site,
	}
	var typ string
	switch e := ev.Event.(type) {
	case *pb.JobEvent_Started:
		typ = "started"
		out["target"] = e.Started.Target
		out["resolved"] = e.Started.Resolved
		out["source"] = e.Started.Source
		out["protocol"] = e.Started.Protocol.String()
		out["family"] = e.Started.Family.String()
	case *pb.JobEvent_Cycle:
		typ = "cycle"
		out["number"] = e.Cycle.Number
		out["reached_at"] = e.Cycle.ReachedAt
		out["hops"] = hopsJSON(e.Cycle.Hops)
	case *pb.JobEvent_Finished:
		typ = "finished"
		out["cycles"] = e.Finished.Cycles
		out["reason"] = e.Finished.Reason.String()
	case *pb.JobEvent_Error:
		typ = "error"
		out["code"] = e.Error.Code.String()
		out["message"] = e.Error.Message
	default:
		typ = "unknown"
	}
	out["type"] = typ
	b, _ := json.Marshal(out)
	return b, typ, nil
}

func hopsJSON(hops []*pb.Hop) []map[string]any {
	out := make([]map[string]any, len(hops))
	for i, h := range hops {
		addrs := make([]map[string]any, len(h.Addresses))
		for j, a := range h.Addresses {
			addrs[j] = map[string]any{"ip": a.Ip, "name": a.Name, "count": a.Count}
		}
		m := map[string]any{
			"ttl":       h.Ttl,
			"addresses": addrs,
			"sent":      h.Sent,
			"received":  h.Received,
			"loss_pct":  h.LossPct,
			"last_us":   h.LastUs,
			"best_us":   h.BestUs,
			"avg_us":    h.AvgUs,
			"worst_us":  h.WorstUs,
			"stdev_us":  h.StdevUs,
			"jitter_us": h.JitterUs,
		}
		if h.SampleUs != nil {
			m["sample_us"] = *h.SampleUs
		} else {
			m["sample_us"] = nil
		}
		out[i] = m
	}
	return out
}
