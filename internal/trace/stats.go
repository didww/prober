package trace

import (
	"math"
	"net/netip"
	"time"
)

// hopStats is the running state of one TTL.
type hopStats struct {
	sent     int
	received int
	inFlight int

	last  time.Duration
	best  time.Duration
	worst time.Duration

	// Welford's online mean and variance, in float seconds. Exact enough at
	// any count and needs no history.
	mean float64
	m2   float64

	// Jitter is the mean absolute difference between consecutive RTTs, the
	// RFC 3550 idea without its smoothing, since a per-cycle report already
	// smooths by aggregation.
	prev      time.Duration
	jitterSum time.Duration
	jitterN   int

	addrs []HopAddress
}

func (h *hopStats) sendProbe() {
	h.sent++
	h.inFlight++
}

// lost settles a probe that timed out.
func (h *hopStats) lost() {
	h.inFlight--
}

// reply settles a probe that was answered, from addr, after rtt.
func (h *hopStats) reply(addr netip.Addr, rtt time.Duration) {
	h.inFlight--
	h.received++

	h.last = rtt
	if h.received == 1 || rtt < h.best {
		h.best = rtt
	}
	if rtt > h.worst {
		h.worst = rtt
	}

	x := rtt.Seconds()
	n := float64(h.received)
	delta := x - h.mean
	h.mean += delta / n
	h.m2 += delta * (x - h.mean)

	if h.received > 1 {
		d := rtt - h.prev
		if d < 0 {
			d = -d
		}
		h.jitterSum += d
		h.jitterN++
	}
	h.prev = rtt

	for i := range h.addrs {
		if h.addrs[i].Addr == addr {
			h.addrs[i].Count++
			return
		}
	}
	h.addrs = append(h.addrs, HopAddress{Addr: addr, Count: 1})
}

func (h *hopStats) avg() time.Duration {
	return time.Duration(h.mean * float64(time.Second))
}

func (h *hopStats) stdev() time.Duration {
	if h.received < 2 {
		return 0
	}
	return time.Duration(math.Sqrt(h.m2/float64(h.received-1)) * float64(time.Second))
}

func (h *hopStats) jitter() time.Duration {
	if h.jitterN == 0 {
		return 0
	}
	return h.jitterSum / time.Duration(h.jitterN)
}

// lossPct is over settled probes only, so a hop with one probe in flight
// does not flicker to 50% loss every cycle.
func (h *hopStats) lossPct() float64 {
	settled := h.sent - h.inFlight
	if settled <= 0 {
		return 0
	}
	return 100 * float64(settled-h.received) / float64(settled)
}

// snapshot renders the hop for an event. Addresses are copied so the caller
// may keep the event after the next cycle mutates the stats.
func (h *hopStats) snapshot(ttl int, sample *time.Duration, names func(netip.Addr) string) Hop {
	addrs := make([]HopAddress, len(h.addrs))
	copy(addrs, h.addrs)
	if names != nil {
		for i := range addrs {
			addrs[i].Name = names(addrs[i].Addr)
		}
	}
	return Hop{
		TTL:       ttl,
		Addresses: addrs,
		Sample:    sample,
		Sent:      h.sent,
		Received:  h.received,
		LossPct:   h.lossPct(),
		Last:      h.last,
		Best:      h.best,
		Avg:       h.avg(),
		Worst:     h.worst,
		Stdev:     h.stdev(),
		Jitter:    h.jitter(),
	}
}
