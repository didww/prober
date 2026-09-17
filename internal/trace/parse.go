package trace

import (
	"encoding/binary"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	protoICMP   = 1
	protoTCP    = 6
	protoUDP    = 17
	protoICMPv6 = 58
)

// parseReply turns an ICMP message (IPv4 header already stripped by the net
// package) into the probe it answers. Everything the host receives that is
// not an answer to one of our probes fails here, cheaply.
func parseReply(b []byte, v6 bool) (reply, bool) {
	proto := protoICMP
	if v6 {
		proto = protoICMPv6
	}
	m, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return reply{}, false
	}

	switch body := m.Body.(type) {
	case *icmp.Echo:
		if v6 && m.Type != ipv6.ICMPTypeEchoReply || !v6 && m.Type != ipv4.ICMPTypeEchoReply {
			return reply{}, false
		}
		return reply{
			key:  probeKey{proto: ICMP, v6: v6, id: uint16(body.ID), seq: uint16(body.Seq)},
			kind: replyFromTarget,
		}, true

	case *icmp.TimeExceeded:
		k, ok := innerKey(body.Data, v6)
		if !ok {
			return reply{}, false
		}
		return reply{key: k, kind: replyExceeded, code: m.Code}, true

	case *icmp.DstUnreach:
		k, ok := innerKey(body.Data, v6)
		if !ok {
			return reply{}, false
		}
		// Port unreachable is the target answering a UDP probe. Every other
		// code is something on the way refusing to forward. The session
		// decides which it was by comparing the sender to the target.
		portUnreach := !v6 && m.Code == 3 || v6 && m.Code == 4
		kind := replyUnreachable
		if portUnreach && k.proto == UDP {
			kind = replyFromTarget
		}
		return reply{key: k, kind: kind, code: m.Code}, true
	}
	return reply{}, false
}

// innerKey reads the probe identity out of the original datagram an ICMP
// error quotes: the inner IP header, then the first eight bytes of the
// transport header, which every router includes.
func innerKey(data []byte, v6 bool) (probeKey, bool) {
	var proto int
	var tp []byte
	if v6 {
		if len(data) < ipv6.HeaderLen+8 {
			return probeKey{}, false
		}
		proto = int(data[6])
		tp = data[ipv6.HeaderLen:]
	} else {
		h, err := ipv4.ParseHeader(data)
		if err != nil || len(data) < h.Len+8 {
			return probeKey{}, false
		}
		proto = h.Protocol
		tp = data[h.Len:]
	}

	switch proto {
	case protoICMP, protoICMPv6:
		// Our echo requests only: type 8 for IPv4, 128 for IPv6.
		if !v6 && tp[0] != 8 || v6 && tp[0] != 128 {
			return probeKey{}, false
		}
		return probeKey{
			proto: ICMP,
			v6:    v6,
			id:    binary.BigEndian.Uint16(tp[4:6]),
			seq:   binary.BigEndian.Uint16(tp[6:8]),
		}, true
	case protoUDP:
		return probeKey{proto: UDP, v6: v6, id: binary.BigEndian.Uint16(tp[0:2])}, true
	case protoTCP:
		return probeKey{proto: TCP, v6: v6, id: binary.BigEndian.Uint16(tp[0:2])}, true
	}
	return probeKey{}, false
}
