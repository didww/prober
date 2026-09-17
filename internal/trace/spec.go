// Package trace is the probing engine: a native traceroute and ping that
// sends ICMP, UDP or TCP probes with increasing TTL and reports, once per
// cycle, what every hop answered.
//
// It replaces the mtr binary the previous application shelled out to. The
// shape of what it reports is deliberately close to mtr's, since operators
// know those columns, but it keeps every individual sample and every address
// seen at a hop, which mtr's split output never printed.
//
// Privileges: the engine opens raw ICMP sockets, so the process needs
// CAP_NET_RAW. Nothing else in it is privileged.
package trace

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

type Protocol uint8

const (
	ICMP Protocol = iota + 1
	UDP
	TCP
)

func (p Protocol) String() string {
	switch p {
	case ICMP:
		return "icmp"
	case UDP:
		return "udp"
	case TCP:
		return "tcp"
	}
	return fmt.Sprintf("protocol(%d)", uint8(p))
}

type Mode uint8

const (
	// MTR probes every TTL from FirstTTL up to the one the target answers at.
	MTR Mode = iota + 1
	// Ping probes only the target, at MaxTTL.
	Ping
)

// Spec is one trace as the engine runs it. The target is already an address:
// resolving a name is the caller's job, because which resolver answers is a
// property of the site, not of the engine.
type Spec struct {
	Target   netip.Addr
	Protocol Protocol
	Mode     Mode

	// Port is the destination port for UDP and TCP. Zero picks the
	// protocol's default: 33434 for UDP (the traceroute convention, chosen
	// to be closed) and 80 for TCP.
	Port uint16

	// ResolveNames reverse-resolves hop addresses. Names are filled in as
	// lookups complete, so early cycles may carry addresses only.
	ResolveNames bool

	Cycles       int
	Interval     time.Duration
	FirstTTL     int
	MaxTTL       int
	PacketSize   int
	DSCP         int
	Source       netip.Addr
	ProbeTimeout time.Duration
}

// Defaults for the zero values of Spec. Interval and Cycles follow mtr;
// ProbeTimeout is shorter than mtr's ten seconds because every cycle's
// report waits on it for a silent hop, and three seconds is already well
// beyond any RTT a hop that answers at all will show.
const (
	DefaultCycles       = 300
	DefaultInterval     = time.Second
	DefaultMaxTTL       = 30
	DefaultPingTTL      = 64
	DefaultPacketSize   = 56
	DefaultProbeTimeout = 3 * time.Second
	DefaultUDPPort      = 33434
	DefaultTCPPort      = 80
)

// Normalize fills defaults and validates. It is what Engine.Run calls first,
// exported so a caller can show the effective values before running.
func (s Spec) Normalize() (Spec, error) {
	if !s.Target.IsValid() {
		return s, errors.New("trace: target address is required")
	}
	s.Target = s.Target.Unmap()
	if s.Source.IsValid() {
		s.Source = s.Source.Unmap()
		if s.Source.Is4() != s.Target.Is4() {
			return s, errors.New("trace: source and target address families differ")
		}
	}
	switch s.Protocol {
	case 0:
		s.Protocol = ICMP
	case ICMP, UDP, TCP:
	default:
		return s, fmt.Errorf("trace: unknown protocol %d", s.Protocol)
	}
	switch s.Mode {
	case 0:
		s.Mode = MTR
	case MTR, Ping:
	default:
		return s, fmt.Errorf("trace: unknown mode %d", s.Mode)
	}
	if s.Port == 0 {
		switch s.Protocol {
		case UDP:
			s.Port = DefaultUDPPort
		case TCP:
			s.Port = DefaultTCPPort
		}
	}
	if s.Cycles <= 0 {
		s.Cycles = DefaultCycles
	}
	if s.Interval <= 0 {
		s.Interval = DefaultInterval
	}
	if s.MaxTTL <= 0 {
		if s.Mode == Ping {
			s.MaxTTL = DefaultPingTTL
		} else {
			s.MaxTTL = DefaultMaxTTL
		}
	}
	if s.MaxTTL > 255 {
		return s, fmt.Errorf("trace: max ttl %d exceeds 255", s.MaxTTL)
	}
	if s.FirstTTL <= 0 {
		s.FirstTTL = 1
	}
	if s.Mode == Ping {
		s.FirstTTL = s.MaxTTL
	}
	if s.FirstTTL > s.MaxTTL {
		return s, fmt.Errorf("trace: first ttl %d exceeds max ttl %d", s.FirstTTL, s.MaxTTL)
	}
	if s.PacketSize <= 0 {
		s.PacketSize = DefaultPacketSize
	}
	if s.PacketSize > 1400 {
		return s, fmt.Errorf("trace: packet size %d exceeds 1400", s.PacketSize)
	}
	if s.DSCP < 0 || s.DSCP > 63 {
		return s, fmt.Errorf("trace: dscp %d out of range 0-63", s.DSCP)
	}
	if s.ProbeTimeout <= 0 {
		s.ProbeTimeout = DefaultProbeTimeout
	}
	return s, nil
}

// hopCount is how many TTLs a cycle probes at most.
func (s Spec) hopCount() int {
	return s.MaxTTL - s.FirstTTL + 1
}
