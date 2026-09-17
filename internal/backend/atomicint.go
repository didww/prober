package backend

import "sync/atomic"

// atomicI64 is sync/atomic.Int64, aliased so agentConn can embed it as a value.
type atomicI64 = atomic.Int64
