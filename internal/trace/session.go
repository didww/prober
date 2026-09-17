package trace

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// probe is one packet in flight.
type probe struct {
	key      probeKey
	ttl      int
	cycle    int
	sentAt   time.Time
	deadline time.Time
	settled  bool
	// closer is the UDP probe socket, closed when the probe settles. TCP
	// probes own their descriptor in the goroutine that waits on connect.
	closer io.Closer
}

// cycleState is a cycle that has not been reported yet.
type cycleState struct {
	number int
	ttls   []int
	// sending is true until the cycle's last probe has left; a cycle whose
	// early probes were all answered is not complete while later ones are
	// still due.
	sending     bool
	outstanding int
	// samples has an entry for every TTL sent: nil once lost, an RTT once
	// answered, absent while in flight.
	samples    map[int]*time.Duration
	targetSeen bool
}

// session is one run. Every field is owned by the run loop goroutine; the
// only concurrent entry point is deliver, which hands a reply to the loop
// over a channel.
type session struct {
	e    *Engine
	f    *family
	spec Spec
	emit func(Event)
	v6   bool
	tos  int
	zone int

	icmpID  uint16
	seq     uint16
	payload []byte

	replies chan reply
	dropped atomic.Int64

	probes map[probeKey]*probe
	// queue is every unsettled probe in send order, which is also deadline
	// order because every probe gets the same timeout.
	queue  []*probe
	hops   map[int]*hopStats
	cycles map[int]*cycleState

	cycle       int // number of the cycle being sent, 0 before the first
	nextEmit    int
	sendIdx     int
	cycleTTLs   []int
	cycleStart  time.Time
	nextSendAt  time.Time
	sendingDone bool

	reachedAt    int
	missedTarget int
}

// missedTargetLimit is how many reported cycles without a target reply it
// takes to go back to probing the full TTL range: the path may have grown.
const missedTargetLimit = 3

func newSession(e *Engine, f *family, spec Spec, emit func(Event)) *session {
	s := &session{
		e:        e,
		f:        f,
		spec:     spec,
		emit:     emit,
		v6:       spec.Target.Is6(),
		tos:      spec.DSCP << 2,
		replies:  make(chan reply, 4096),
		probes:   make(map[probeKey]*probe),
		hops:     make(map[int]*hopStats),
		cycles:   make(map[int]*cycleState),
		nextEmit: 1,
	}
	s.payload = make([]byte, spec.PacketSize)
	for i := range s.payload {
		s.payload[i] = byte(i)
	}
	if z := spec.Target.Zone(); z != "" {
		if ifi, err := net.InterfaceByName(z); err == nil {
			s.zone = ifi.Index
		}
	}
	return s
}

func (s *session) deliver(r reply) {
	select {
	case s.replies <- r:
	default:
		s.dropped.Add(1)
	}
}

func (s *session) run(ctx context.Context) error {
	if s.spec.Protocol == ICMP {
		id, err := s.e.allocID()
		if err != nil {
			s.emit(Event{Kind: Failed, Time: time.Now(), Err: err})
			return err
		}
		s.icmpID = id
		defer s.e.freeID(id)
	}
	defer s.cleanup()

	now := time.Now()
	s.emit(Event{Kind: Started, Time: now, Spec: &s.spec})
	s.cycleStart = now
	s.nextSendAt = now

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		now = time.Now()
		if !s.sendingDone && !now.Before(s.nextSendAt) {
			if err := s.sendDue(now); err != nil {
				s.emit(Event{Kind: Failed, Time: time.Now(), Err: err})
				return err
			}
		}
		s.expire(now)
		s.tryEmit()

		if s.sendingDone && len(s.queue) == 0 {
			break
		}

		wait := s.nextWake(now)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			s.flush(true)
			return nil
		case r := <-s.replies:
			s.handleReply(r)
			// Drain what else arrived; each is cheap and replies cluster.
			for range len(s.replies) {
				s.handleReply(<-s.replies)
			}
		case <-timer.C:
		}
	}

	s.flush(false)
	return nil
}

// nextWake is how long the loop may sleep: until the next send or the
// earliest probe deadline, whichever is first.
func (s *session) nextWake(now time.Time) time.Duration {
	wait := time.Hour
	if !s.sendingDone {
		wait = s.nextSendAt.Sub(now)
	}
	for _, p := range s.queue {
		if p.settled {
			continue
		}
		if d := p.deadline.Sub(now); d < wait {
			wait = d
		}
		break
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

// ttlsForCycle is what the next cycle probes: everything up to the target
// once it has answered, the full range until then. Ping mode is one TTL.
func (s *session) ttlsForCycle() []int {
	if s.spec.Mode == Ping {
		return []int{s.spec.MaxTTL}
	}
	upTo := s.spec.MaxTTL
	if s.reachedAt > 0 && s.reachedAt < upTo {
		upTo = s.reachedAt
	}
	ttls := make([]int, 0, upTo-s.spec.FirstTTL+1)
	for ttl := s.spec.FirstTTL; ttl <= upTo; ttl++ {
		ttls = append(ttls, ttl)
	}
	return ttls
}

// sendDue sends the probes whose time has come. Probes are spread evenly
// across the interval, so a thirty-hop cycle at one second is a packet every
// 33 ms rather than a burst that trips ICMP rate limits on the first router.
func (s *session) sendDue(now time.Time) error {
	for !s.sendingDone && !now.Before(s.nextSendAt) {
		if s.sendIdx == 0 {
			s.cycle++
			// Everything before this cycle is reported now, answered or not,
			// so the consumer sees one event per interval at worst.
			s.forceEmit(s.cycle - 1)
			s.cycleTTLs = s.ttlsForCycle()
			s.cycles[s.cycle] = &cycleState{
				number:  s.cycle,
				ttls:    s.cycleTTLs,
				sending: true,
				samples: make(map[int]*time.Duration, len(s.cycleTTLs)),
			}
		}
		ttl := s.cycleTTLs[s.sendIdx]
		if s.reachedAt > 0 && ttl > s.reachedAt {
			// The target answered lower down while this cycle was being
			// sent. Nothing beyond it is worth a packet; the cycle ends
			// here. sendIdx is at least 1: the reply that set reachedAt
			// came from a TTL this cycle has already sent, or from an
			// earlier cycle, in which case ttlsForCycle already stopped
			// at it.
			c := s.cycles[s.cycle]
			c.ttls = s.cycleTTLs[:s.sendIdx]
			s.endCycle()
			continue
		}
		if err := s.send(ttl, now); err != nil {
			return err
		}
		s.sendIdx++
		if s.sendIdx < len(s.cycleTTLs) {
			s.nextSendAt = s.cycleStart.Add(s.spec.Interval * time.Duration(s.sendIdx) / time.Duration(len(s.cycleTTLs)))
			continue
		}
		s.endCycle()
	}
	return nil
}

// endCycle closes the sending side of the current cycle and schedules the
// next one on the interval grid, so a cycle cut short does not pull the
// following ones earlier.
func (s *session) endCycle() {
	s.cycles[s.cycle].sending = false
	s.sendIdx = 0
	if s.cycle >= s.spec.Cycles {
		s.sendingDone = true
		return
	}
	s.cycleStart = s.cycleStart.Add(s.spec.Interval)
	s.nextSendAt = s.cycleStart
}

func (s *session) send(ttl int, now time.Time) error {
	hs := s.hop(ttl)
	c := s.cycles[s.cycle]

	var p *probe
	var err error
	switch s.spec.Protocol {
	case ICMP:
		p, err = s.sendICMP(ttl)
	case UDP:
		p, err = s.sendUDP(ttl)
	case TCP:
		p, err = s.sendTCP(ttl)
	}
	hs.sendProbe()
	if err != nil {
		if !isUnreachable(err) {
			return err
		}
		// No route, or the source cannot reach this target: the probe is
		// lost, which the report shows as loss rather than as a dead run,
		// since the other family or another target from this site may work.
		hs.lost()
		c.samples[ttl] = nil
		return nil
	}
	p.ttl = ttl
	p.cycle = s.cycle
	p.deadline = p.sentAt.Add(s.spec.ProbeTimeout)
	s.probes[p.key] = p
	s.queue = append(s.queue, p)
	c.outstanding++
	return nil
}

func (s *session) hop(ttl int) *hopStats {
	hs := s.hops[ttl]
	if hs == nil {
		hs = &hopStats{}
		s.hops[ttl] = hs
	}
	return hs
}

func (s *session) handleReply(r reply) {
	p := s.probes[r.key]
	if p == nil || p.settled {
		return // late, duplicate, or a retransmitted SYN answered twice
	}
	rtt := r.at.Sub(p.sentAt)
	if rtt < 0 {
		rtt = 0
	}
	s.settle(p)
	s.hop(p.ttl).reply(r.from, rtt)

	if c := s.cycles[p.cycle]; c != nil {
		c.samples[p.ttl] = &rtt
		c.outstanding--
		if r.kind == replyFromTarget {
			c.targetSeen = true
		}
	}
	if r.kind == replyFromTarget {
		s.reachedAt = p.ttl
		s.missedTarget = 0
	}
}

// settle takes a probe out of the registry and the in-flight count. The
// queue entry stays, flagged, until it reaches the head.
func (s *session) settle(p *probe) {
	p.settled = true
	delete(s.probes, p.key)
	s.e.unregister(p.key)
	if p.closer != nil {
		p.closer.Close()
	}
}

// expire settles every probe past its deadline as lost.
func (s *session) expire(now time.Time) {
	for len(s.queue) > 0 {
		p := s.queue[0]
		if !p.settled && now.Before(p.deadline) {
			break
		}
		s.queue = s.queue[1:]
		if p.settled {
			continue
		}
		s.settle(p)
		s.hop(p.ttl).lost()
		if c := s.cycles[p.cycle]; c != nil {
			c.samples[p.ttl] = nil
			c.outstanding--
		}
	}
}

// tryEmit reports every completed cycle that is next in order.
func (s *session) tryEmit() {
	for {
		c := s.cycles[s.nextEmit]
		if c == nil || c.sending || c.outstanding > 0 {
			return
		}
		s.report(c)
	}
}

// forceEmit reports every cycle up to and including n, complete or not.
func (s *session) forceEmit(n int) {
	for s.nextEmit <= n {
		c := s.cycles[s.nextEmit]
		if c == nil {
			s.nextEmit++
			continue
		}
		s.report(c)
	}
}

func (s *session) report(c *cycleState) {
	delete(s.cycles, c.number)
	s.nextEmit = c.number + 1

	if s.reachedAt > 0 {
		if c.targetSeen {
			s.missedTarget = 0
		} else if s.missedTarget++; s.missedTarget >= missedTargetLimit {
			s.reachedAt = 0
		}
	}

	last := c.ttls[len(c.ttls)-1]
	if s.reachedAt > 0 && s.reachedAt < last {
		last = s.reachedAt
	}
	var names func(netip.Addr) string
	if s.spec.ResolveNames {
		names = s.e.rdns.name
	}
	hops := make([]Hop, 0, len(c.ttls))
	for _, ttl := range c.ttls {
		if ttl > last {
			break
		}
		sample, sent := c.samples[ttl]
		if !sent {
			sample = nil
		}
		hops = append(hops, s.hop(ttl).snapshot(ttl, sample, names))
	}
	s.emit(Event{
		Kind: CycleDone,
		Time: time.Now(),
		Cycle: &Cycle{
			Number:    c.number,
			ReachedAt: s.reachedAt,
			Hops:      hops,
		},
	})
}

// flush ends the run: whatever cycles are still open are reported as they
// stand, then the terminal event.
func (s *session) flush(cancelled bool) {
	s.forceEmit(s.cycle)
	s.emit(Event{Kind: Finished, Time: time.Now(), Cancelled: cancelled})
}

func (s *session) cleanup() {
	for _, p := range s.probes {
		s.settle(p)
	}
	s.queue = nil
}

// isUnreachable is the class of send error that means "this probe cannot
// leave the host", which is loss from the target's point of view.
func isUnreachable(err error) bool {
	return errors.Is(err, unix.ENETUNREACH) ||
		errors.Is(err, unix.EHOSTUNREACH) ||
		errors.Is(err, unix.ENETDOWN) ||
		errors.Is(err, unix.EHOSTDOWN)
}
