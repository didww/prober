package trace

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// resolver is the reverse-DNS cache shared by every run on an engine. A
// lookup is started the first time an address is seen and never blocks a
// cycle: names appear in later events as answers come in.
type resolver struct {
	mu      sync.Mutex
	entries map[netip.Addr]*rdnsEntry
	sem     chan struct{}
	lookup  func(ctx context.Context, addr string) ([]string, error)
}

type rdnsEntry struct {
	name    string
	done    bool
	expires time.Time
}

const (
	rdnsTimeout    = 2 * time.Second
	rdnsTTL        = 10 * time.Minute
	rdnsConcurrent = 8
)

func newResolver() *resolver {
	return &resolver{
		entries: make(map[netip.Addr]*rdnsEntry),
		sem:     make(chan struct{}, rdnsConcurrent),
		lookup:  net.DefaultResolver.LookupAddr,
	}
}

// name returns what is known now and starts a lookup if nothing is.
func (r *resolver) name(addr netip.Addr) string {
	addr = addr.WithZone("")
	r.mu.Lock()
	e := r.entries[addr]
	if e != nil && (!e.done || time.Now().Before(e.expires)) {
		name := e.name
		r.mu.Unlock()
		return name
	}
	e = &rdnsEntry{}
	r.entries[addr] = e
	r.mu.Unlock()

	go r.resolve(addr, e)
	return ""
}

func (r *resolver) resolve(addr netip.Addr, e *rdnsEntry) {
	r.sem <- struct{}{}
	defer func() { <-r.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), rdnsTimeout)
	defer cancel()
	names, err := r.lookup(ctx, addr.String())

	r.mu.Lock()
	defer r.mu.Unlock()
	e.done = true
	e.expires = time.Now().Add(rdnsTTL)
	if err == nil && len(names) > 0 {
		e.name = strings.TrimSuffix(names[0], ".")
	}
}
