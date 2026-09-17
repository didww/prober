package trace

import (
	"net/netip"
	"time"
)

type EventKind uint8

const (
	// Started is emitted once probing has begun.
	Started EventKind = iota + 1
	// CycleDone carries one completed cycle with every hop's sample and
	// running aggregates.
	CycleDone
	// Finished is the last event of a run that ran to completion or was
	// cancelled.
	Finished
	// Failed is the last event of a run the engine could not carry on with.
	Failed
)

// Event is what a run reports. Exactly one of Cycle and Err is set for the
// kinds that carry data.
type Event struct {
	Kind EventKind
	Time time.Time

	// Started: the effective spec, after defaults.
	Spec *Spec
	// CycleDone.
	Cycle *Cycle
	// Finished.
	Cancelled bool
	// Failed.
	Err error
}

// Cycle is one round of probes across all hops.
type Cycle struct {
	// Number counts from 1.
	Number int
	// ReachedAt is the TTL at which the target answered, 0 if not yet.
	ReachedAt int
	// Hops runs from FirstTTL up to the last TTL worth showing: the target's
	// once reached, MaxTTL before that.
	Hops []Hop
}

// Hop is one TTL's sample for this cycle plus its aggregates over the run.
type Hop struct {
	TTL int
	// Addresses that have answered at this TTL, in order of first sight.
	Addresses []HopAddress

	// Sample is this cycle's RTT; nil when the probe was lost or had not
	// answered when the cycle was reported.
	Sample *time.Duration

	Sent     int
	Received int
	// LossPct excludes probes still in flight, as mtr does.
	LossPct float64

	Last   time.Duration
	Best   time.Duration
	Avg    time.Duration
	Worst  time.Duration
	Stdev  time.Duration
	Jitter time.Duration
}

type HopAddress struct {
	Addr netip.Addr
	// Name is the reverse name, empty until resolved or when ResolveNames
	// is off.
	Name string
	// Count is how many replies came from this address.
	Count int
}
