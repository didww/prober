package trace

import (
	"net"
	"net/netip"
	"time"
)

// sendUDP sends one datagram from a socket of its own. The socket's port is
// the probe's identity in the ICMP error a hop returns, and the socket is
// also where an application on an open target port would answer.
func (s *session) sendUDP(ttl int) (*probe, error) {
	network := "udp4"
	if s.v6 {
		network = "udp6"
	}
	laddr := &net.UDPAddr{}
	if s.spec.Source.IsValid() {
		laddr.IP = s.spec.Source.AsSlice()
	}
	c, err := net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	if err := setHopOptions(c, s.v6, ttl, s.tos); err != nil {
		c.Close()
		return nil, err
	}

	port := uint16(c.LocalAddr().(*net.UDPAddr).Port)
	key := probeKey{proto: UDP, v6: s.v6, id: port}
	p := &probe{key: key, closer: c}
	s.e.register(key, s)

	dst := &net.UDPAddr{IP: s.spec.Target.AsSlice(), Port: int(s.spec.Port), Zone: s.spec.Target.Zone()}
	deadline := time.Now().Add(s.spec.ProbeTimeout)
	p.sentAt = time.Now()
	if _, err := c.WriteToUDP(s.payload, dst); err != nil {
		s.e.unregister(key)
		c.Close()
		return nil, err
	}

	// Data back on the socket means the target port is open and something
	// answered: reached, with the answer's RTT.
	go func() {
		_ = c.SetReadDeadline(deadline)
		buf := make([]byte, 2048)
		_, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		addr, ok := netip.AddrFromSlice(from.IP)
		if !ok {
			return
		}
		s.deliver(reply{key: key, from: addr.Unmap(), kind: replyFromTarget, at: time.Now()})
	}()
	return p, nil
}
