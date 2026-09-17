package backend

import "sync/atomic"

// readyFlag is a small boolean: server readiness, flipped by the lifecycle
// and read by the readiness probe.
type readyFlag struct{ v atomic.Bool }

func (r *readyFlag) set(b bool) { r.v.Store(b) }
func (r *readyFlag) get() bool  { return r.v.Load() }
