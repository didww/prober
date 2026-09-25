package agent

import (
	"context"
	"testing"
)

func TestJobTable(t *testing.T) {
	session, stop := context.WithCancel(context.Background())
	defer stop()
	jobs := jobTable{m: map[string]*jobHandle{}}

	c1, cancel1 := context.WithCancel(session)
	h1 := jobs.add("j1", cancel1)
	if jobs.count() != 1 || !jobs.has("j1") {
		t.Fatal("job not tracked")
	}

	// A second job under the same id, as an assignment re-applied mid-tick
	// can produce, takes the entry; the first job's exit must not remove
	// it, and must not touch the second job's context.
	c2, cancel2 := context.WithCancel(session)
	h2 := jobs.add("j1", cancel2)
	jobs.remove("j1", h1)
	if !jobs.has("j1") {
		t.Fatal("the newer job was removed by the older one's exit")
	}
	if c2.Err() != nil {
		t.Fatal("the newer job was cancelled by the older one's exit")
	}
	if c1.Err() != nil {
		t.Fatal("the table cancelled a context it does not own")
	}

	// cancel stops the registered job; the second job's own exit removes it.
	jobs.cancel("j1")
	if c2.Err() == nil {
		t.Fatal("cancel did not stop the job")
	}
	jobs.remove("j1", h2)
	if jobs.count() != 0 || jobs.has("j1") {
		t.Fatal("job still tracked after remove")
	}
	jobs.remove("j1", h2)
	jobs.remove("nope", nil)

	c3, cancel3 := context.WithCancel(session)
	jobs.add("j3", cancel3)
	jobs.cancelAll()
	if c3.Err() == nil {
		t.Fatal("cancelAll left a job running")
	}
}
