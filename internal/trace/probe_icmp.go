package trace

import (
	"net"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// sendICMP writes one echo request with the run's identifier and the next
// sequence number through the family's shared raw socket.
func (s *session) sendICMP(ttl int) (*probe, error) {
	s.seq++
	key := probeKey{proto: ICMP, v6: s.v6, id: s.icmpID, seq: s.seq}
	p := &probe{key: key}

	body := &icmp.Echo{ID: int(s.icmpID), Seq: int(s.seq), Data: s.payload}
	dst := &net.IPAddr{IP: s.spec.Target.AsSlice(), Zone: s.spec.Target.Zone()}

	// Registered before the write: a loopback reply can arrive before
	// WriteTo returns.
	s.e.register(key, s)

	var err error
	if s.v6 {
		m := icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: body}
		// Checksum left zero: the kernel fills it for ICMPv6 raw sockets.
		b, merr := m.Marshal(nil)
		if merr != nil {
			s.e.unregister(key)
			return nil, merr
		}
		cm := &ipv6.ControlMessage{HopLimit: ttl, TrafficClass: s.tos}
		if s.spec.Source.IsValid() {
			cm.Src = s.spec.Source.AsSlice()
		}
		p.sentAt = time.Now()
		_, err = s.f.v6.WriteTo(b, cm, dst)
	} else {
		m := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: body}
		b, merr := m.Marshal(nil)
		if merr != nil {
			s.e.unregister(key)
			return nil, merr
		}
		var cm *ipv4.ControlMessage
		if s.spec.Source.IsValid() {
			cm = &ipv4.ControlMessage{Src: s.spec.Source.AsSlice()}
		}
		s.f.sendMu.Lock()
		_ = s.f.v4.SetTTL(ttl)
		_ = s.f.v4.SetTOS(s.tos)
		p.sentAt = time.Now()
		_, err = s.f.v4.WriteTo(b, cm, dst)
		s.f.sendMu.Unlock()
	}
	if err != nil {
		s.e.unregister(key)
		return nil, err
	}
	return p, nil
}
