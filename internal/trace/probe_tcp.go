package trace

import (
	"time"

	"golang.org/x/sys/unix"
)

// sendTCP starts a non-blocking connect from a socket of its own, with the
// TTL set, and lets the kernel do the rest. This is the half-open method as
// mtr does it, without crafting packets:
//
//   - a hop on the way answers with time exceeded, which the raw ICMP socket
//     sees with our source port inside, and which the kernel also turns into
//     an EHOSTUNREACH on the connect, so the socket resolves itself;
//   - the target answers SYN-ACK (connect succeeds) or RST (ECONNREFUSED);
//     both mean reached.
//
// One retransmission is allowed (TCP_SYNCNT=1) so a probe does not hammer
// a hop that is rate-limiting; a duplicate answer is ignored by the session.
func (s *session) sendTCP(ttl int) (*probe, error) {
	af := unix.AF_INET
	if s.v6 {
		af = unix.AF_INET6
	}
	fd, err := unix.Socket(af, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := setHopOptionsFD(fd, s.v6, ttl, s.tos); err != nil {
		unix.Close(fd)
		return nil, err
	}
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_SYNCNT, 1)

	// Bind first so the port exists before the SYN leaves and the probe can
	// be registered under it.
	var local, remote unix.Sockaddr
	if s.v6 {
		l := &unix.SockaddrInet6{ZoneId: uint32(s.zone)}
		if s.spec.Source.IsValid() {
			l.Addr = s.spec.Source.As16()
		}
		local = l
		remote = &unix.SockaddrInet6{Port: int(s.spec.Port), Addr: s.spec.Target.As16(), ZoneId: uint32(s.zone)}
	} else {
		l := &unix.SockaddrInet4{}
		if s.spec.Source.IsValid() {
			l.Addr = s.spec.Source.As4()
		}
		local = l
		remote = &unix.SockaddrInet4{Port: int(s.spec.Port), Addr: s.spec.Target.As4()}
	}
	if err := unix.Bind(fd, local); err != nil {
		unix.Close(fd)
		return nil, err
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	var port uint16
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		port = uint16(a.Port)
	case *unix.SockaddrInet6:
		port = uint16(a.Port)
	}

	key := probeKey{proto: TCP, v6: s.v6, id: port}
	p := &probe{key: key}
	s.e.register(key, s)

	p.sentAt = time.Now()
	if err := unix.Connect(fd, remote); err != nil && err != unix.EINPROGRESS {
		s.e.unregister(key)
		unix.Close(fd)
		return nil, err
	}
	go s.awaitTCP(fd, key, p.sentAt.Add(s.spec.ProbeTimeout))
	return p, nil
}

// awaitTCP waits for the connect to resolve and reports the outcomes that
// mean the target answered. Hop answers are reported by the ICMP read loop;
// here they show up as EHOSTUNREACH and are simply the end of this socket.
func (s *session) awaitTCP(fd int, key probeKey, deadline time.Time) {
	defer unix.Close(fd)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(fds, int(remaining/time.Millisecond)+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return
		}
		soerr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return
		}
		if soerr == 0 || soerr == int(unix.ECONNREFUSED) {
			s.deliver(reply{key: key, from: s.spec.Target, kind: replyFromTarget, at: time.Now()})
		}
		return
	}
}
