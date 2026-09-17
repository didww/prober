package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// Engine owns what every run shares: one raw ICMP socket per address family
// and the table that maps an in-flight probe back to the run that sent it.
//
// Raw ICMP sockets receive a copy of every ICMP packet the host sees, so one
// per family is the right number: a socket per run would hand the kernel N
// copies of every packet to deliver. Sends go through the same sockets, which
// for IPv4 means serialising the TTL socket option with the write.
type Engine struct {
	log *slog.Logger

	v4 *family
	v6 *family

	mu     sync.Mutex
	probes map[probeKey]*session
	// ICMP identifiers in use. Allocated at random rather than sequentially
	// so two engines on one host, or a stray ping, rarely collide.
	ids map[uint16]struct{}

	rdns *resolver

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// family is one address family's raw ICMP socket.
type family struct {
	conn *net.IPConn
	v4   *ipv4.PacketConn // exactly one of v4, v6 is set
	v6   *ipv6.PacketConn
	// sendMu serialises SetTTL/SetTOS with WriteTo on IPv4, where the TTL
	// is a socket option rather than a per-packet control message.
	sendMu sync.Mutex
}

// Capabilities is what the host let the engine open.
type Capabilities struct {
	IPv4 bool
	IPv6 bool
}

// New opens the raw sockets. It fails only if neither family can be opened,
// which on Linux means the process lacks CAP_NET_RAW.
func New(log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		log:    log,
		probes: make(map[probeKey]*session),
		ids:    make(map[uint16]struct{}),
		rdns:   newResolver(),
		closed: make(chan struct{}),
	}

	// A family is usable only if the host has an address of it — otherwise the
	// raw socket opens but no probe can leave. A node with net.ipv6.conf.all.
	// disable_ipv6=1 has no IPv6 address at all (not even ::1), so its ICMPv6
	// socket opens yet IPv6 is dead; reporting it available would make the
	// engine prefer IPv6 for a dual-stack target and every probe would fail.
	var errs []error
	if hasFamilyAddr(false) {
		if f4, err := openFamily(false); err != nil {
			errs = append(errs, fmt.Errorf("ipv4: %w", err))
		} else {
			e.v4 = f4
		}
	} else {
		log.Debug("trace: IPv4 unavailable (no IPv4 address on any interface)")
	}
	if hasFamilyAddr(true) {
		if f6, err := openFamily(true); err != nil {
			errs = append(errs, fmt.Errorf("ipv6: %w", err))
		} else {
			e.v6 = f6
		}
	} else {
		log.Debug("trace: IPv6 unavailable (no IPv6 address; disable_ipv6?)")
	}
	if e.v4 == nil && e.v6 == nil {
		if len(errs) == 0 {
			return nil, errors.New("trace: no usable address family (no IPv4 or IPv6 address on any interface)")
		}
		return nil, fmt.Errorf("trace: no raw ICMP socket could be opened (CAP_NET_RAW?): %w", errors.Join(errs...))
	}
	for _, err := range errs {
		log.Warn("trace: address family unavailable", "err", err)
	}

	if e.v4 != nil {
		e.wg.Add(1)
		go e.readLoop(e.v4)
	}
	if e.v6 != nil {
		e.wg.Add(1)
		go e.readLoop(e.v6)
	}
	return e, nil
}

// hasFamilyAddr reports whether any interface carries an address of the given
// family. It counts loopback (127.0.0.1 / ::1), which is enough to trace
// loopback and on-link targets; disabling IPv6 removes even ::1, so this is
// false there. If the interfaces cannot be read, it assumes available rather
// than wrongly disabling a family.
func hasFamilyAddr(v6 bool) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(ipn.IP); ok && ip.Unmap().Is6() == v6 {
			return true
		}
	}
	return false
}

func openFamily(v6 bool) (*family, error) {
	network, addr := "ip4:icmp", "0.0.0.0"
	if v6 {
		network, addr = "ip6:ipv6-icmp", "::"
	}
	c, err := net.ListenPacket(network, addr)
	if err != nil {
		return nil, err
	}
	ipc := c.(*net.IPConn)
	// Replies from every run on the host land here; a burst of them must
	// not be dropped for want of buffer.
	_ = ipc.SetReadBuffer(4 << 20)

	f := &family{conn: ipc}
	if v6 {
		p := ipv6.NewPacketConn(ipc)
		var filt ipv6.ICMPFilter
		filt.SetAll(true)
		filt.Accept(ipv6.ICMPTypeEchoReply)
		filt.Accept(ipv6.ICMPTypeTimeExceeded)
		filt.Accept(ipv6.ICMPTypeDestinationUnreachable)
		if err := p.SetICMPFilter(&filt); err != nil {
			ipc.Close()
			return nil, fmt.Errorf("icmp6 filter: %w", err)
		}
		f.v6 = p
	} else {
		f.v4 = ipv4.NewPacketConn(ipc)
		// IPv4 raw sockets have no type filter, so attach a BPF program that
		// keeps only echo reply, destination unreachable and time exceeded.
		// Best effort: without it the read loop just parses and drops more.
		if err := attachICMP4Filter(ipc); err != nil {
			slog.Default().Debug("trace: icmp4 bpf filter not attached", "err", err)
		}
	}
	return f, nil
}

// attachICMP4Filter installs a classic BPF program on a raw IPv4 socket. The
// packet BPF sees starts at the IP header, so the ICMP type is one byte past
// the header length.
func attachICMP4Filter(c *net.IPConn) error {
	prog, err := bpf.Assemble([]bpf.Instruction{
		bpf.LoadMemShift{Off: 0},                               // X = IHL * 4
		bpf.LoadIndirect{Off: 0, Size: 1},                      // A = ICMP type
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 0, SkipTrue: 2},   // echo reply
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 3, SkipTrue: 1},   // dest unreachable
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 11, SkipFalse: 1}, // time exceeded
		bpf.RetConstant{Val: 0xffff},
		bpf.RetConstant{Val: 0},
	})
	if err != nil {
		return err
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = rc.Control(func(fd uintptr) {
		fprog := unix.SockFprog{
			Len:    uint16(len(prog)),
			Filter: (*unix.SockFilter)(unsafe.Pointer(&prog[0])),
		}
		serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &fprog)
	})
	if err != nil {
		return err
	}
	return serr
}

// Capabilities reports which families opened.
func (e *Engine) Capabilities() Capabilities {
	return Capabilities{IPv4: e.v4 != nil, IPv6: e.v6 != nil}
}

// Close stops the read loops and closes the sockets. Runs in progress fail
// on their next send.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		close(e.closed)
		if e.v4 != nil {
			e.v4.conn.Close()
		}
		if e.v6 != nil {
			e.v6.conn.Close()
		}
	})
	e.wg.Wait()
	return nil
}

// Run executes one trace and calls emit for every event, from a single
// goroutine, in order. It returns when the run has finished, failed, or ctx
// was cancelled; the terminal event has already been emitted by then.
//
// emit must not block for long: the run's own loop is what calls it, and a
// slow consumer delays probe scheduling.
func (e *Engine) Run(ctx context.Context, spec Spec, emit func(Event)) error {
	spec, err := spec.Normalize()
	if err != nil {
		return err
	}
	fam := e.v4
	if spec.Target.Is6() {
		fam = e.v6
	}
	if fam == nil {
		return fmt.Errorf("trace: address family of %v is not available on this host", spec.Target)
	}
	s := newSession(e, fam, spec, emit)
	return s.run(ctx)
}

// --- probe registry ---------------------------------------------------------

// probeKey identifies an in-flight probe from what an ICMP reply carries: the
// identifier and sequence of an echo request, or the local port of a UDP or
// TCP probe, which is unique per probe because each one has its own socket.
type probeKey struct {
	proto Protocol
	v6    bool
	id    uint16
	seq   uint16
}

func (e *Engine) register(k probeKey, s *session) {
	e.mu.Lock()
	e.probes[k] = s
	e.mu.Unlock()
}

func (e *Engine) unregister(k probeKey) {
	e.mu.Lock()
	delete(e.probes, k)
	e.mu.Unlock()
}

func (e *Engine) owner(k probeKey) *session {
	e.mu.Lock()
	s := e.probes[k]
	e.mu.Unlock()
	return s
}

// allocID takes an ICMP identifier for a run. Identifiers are 16-bit, so
// there is a hard ceiling of 65535 concurrent ICMP runs, which is not one
// anybody will reach before running out of something else.
func (e *Engine) allocID() (uint16, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ids) >= 60000 {
		return 0, errors.New("trace: no free ICMP identifier")
	}
	for {
		id := uint16(rand.UintN(65536))
		if _, used := e.ids[id]; used || id == 0 {
			continue
		}
		e.ids[id] = struct{}{}
		return id, nil
	}
}

func (e *Engine) freeID(id uint16) {
	e.mu.Lock()
	delete(e.ids, id)
	e.mu.Unlock()
}

// --- receive path ------------------------------------------------------------

type replyKind uint8

const (
	// replyFromTarget is anything that means the target itself answered:
	// an echo reply, a port unreachable from the target, a completed or
	// refused TCP connect, or data on a UDP probe socket.
	replyFromTarget replyKind = iota + 1
	// replyExceeded is a time exceeded from a hop on the way.
	replyExceeded
	// replyUnreachable is a destination unreachable other than port
	// unreachable from the target: a router refusing to forward.
	replyUnreachable
)

type reply struct {
	key  probeKey
	from netip.Addr
	kind replyKind
	code int
	at   time.Time
}

func (e *Engine) readLoop(f *family) {
	defer e.wg.Done()
	buf := make([]byte, 65536)
	v6 := f.v6 != nil
	for {
		n, peer, err := f.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-e.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			e.log.Warn("trace: icmp read", "v6", v6, "err", err)
			// A persistent read error would spin; a short pause bounds it.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		at := time.Now()
		ipa, ok := peer.(*net.IPAddr)
		if !ok {
			continue
		}
		from, ok := netip.AddrFromSlice(ipa.IP)
		if !ok {
			continue
		}
		from = from.Unmap()
		if ipa.Zone != "" {
			from = from.WithZone(ipa.Zone)
		}
		r, ok := parseReply(buf[:n], v6)
		if !ok {
			continue
		}
		r.from = from
		r.at = at
		if s := e.owner(r.key); s != nil {
			s.deliver(r)
		}
	}
}
