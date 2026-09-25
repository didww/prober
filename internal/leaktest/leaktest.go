// Package leaktest is the shared harness for the engines' leak checks: run a
// thing many times, then see that goroutines and live heap came back to
// where they started. The checks take seconds and depend on timing, so they
// run only when asked for with PROBER_LEAK_TESTS=1 (the Makefile's test-leak
// and test-trace targets set it).
package leaktest

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// SkipUnlessEnabled skips the test unless PROBER_LEAK_TESTS is set.
func SkipUnlessEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("PROBER_LEAK_TESTS") == "" {
		t.Skip("leak check; set PROBER_LEAK_TESTS=1")
	}
}

// HeapInUse is the live heap after a full collection, in bytes.
func HeapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

// Baseline is a measurement to compare against.
type Baseline struct {
	Goroutines int
	Heap       uint64
}

// Mark waits settle for transient goroutines to finish, then measures.
func Mark(settle time.Duration) Baseline {
	time.Sleep(settle)
	return Baseline{Goroutines: runtime.NumGoroutine(), Heap: HeapInUse()}
}

// Check fails the test if, after runs repetitions, goroutines grew by more
// than a handful or the live heap by more than perRun bytes per repetition.
// The per-run budget is what makes a small leak visible: a kilobyte per run
// is hundreds of megabytes a day at a monitor's cadence, so the budget is
// well below that.
func (b Baseline) Check(t *testing.T, what string, runs int, perRun uint64) {
	t.Helper()
	time.Sleep(500 * time.Millisecond)
	goroutines := runtime.NumGoroutine()
	heap := HeapInUse()
	if goroutines > b.Goroutines+5 {
		t.Errorf("%s: goroutines %d -> %d after %d runs", what, b.Goroutines, goroutines, runs)
	}
	var grew uint64
	if heap > b.Heap {
		grew = heap - b.Heap
	}
	if grew > perRun*uint64(runs) {
		t.Errorf("%s: live heap grew %d KB over %d runs, %d B per run, budget %d B", what, grew>>10, runs, grew/uint64(runs), perRun)
	}
	t.Logf("%s: goroutines %d -> %d, live heap %d KB -> %d KB over %d runs (%d B per run)", what, b.Goroutines, goroutines, b.Heap>>10, heap>>10, runs, grew/uint64(runs))
}
