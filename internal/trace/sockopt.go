package trace

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setHopOptions sets the TTL and TOS on a net socket.
func setHopOptions(c interface {
	SyscallConn() (syscall.RawConn, error)
}, v6 bool, ttl, tos int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = setHopOptionsFD(int(fd), v6, ttl, tos)
	}); err != nil {
		return err
	}
	return serr
}

func setHopOptionsFD(fd int, v6 bool, ttl, tos int) error {
	if v6 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, ttl); err != nil {
			return err
		}
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_TCLASS, tos)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl); err != nil {
		return err
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TOS, tos)
}
