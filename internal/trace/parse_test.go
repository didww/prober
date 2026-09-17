package trace

import (
	"encoding/binary"
	"net"
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// inner4 builds the quoted datagram of an IPv4 ICMP error: an IP header
// followed by the first eight bytes of the transport header.
func inner4(t *testing.T, proto int, transport []byte) []byte {
	t.Helper()
	h := ipv4.Header{
		Version:  4,
		Len:      ipv4.HeaderLen,
		TotalLen: ipv4.HeaderLen + len(transport),
		TTL:      1,
		Protocol: proto,
		Src:      net.IPv4(192, 0, 2, 10),
		Dst:      net.IPv4(198, 51, 100, 1),
	}
	b, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return append(b, transport...)
}

func inner6(proto int, transport []byte) []byte {
	b := make([]byte, ipv6.HeaderLen)
	b[0] = 6 << 4
	binary.BigEndian.PutUint16(b[4:6], uint16(len(transport)))
	b[6] = byte(proto)
	b[7] = 1
	copy(b[8:24], net.ParseIP("2001:db8::10"))
	copy(b[24:40], net.ParseIP("2001:db8::1"))
	return append(b, transport...)
}

func echoRequest(v6 bool, id, seq uint16) []byte {
	b := make([]byte, 8)
	b[0] = 8
	if v6 {
		b[0] = 128
	}
	binary.BigEndian.PutUint16(b[4:6], id)
	binary.BigEndian.PutUint16(b[6:8], seq)
	return b
}

func ports(src, dst uint16) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:2], src)
	binary.BigEndian.PutUint16(b[2:4], dst)
	return b
}

func marshal(t *testing.T, m icmp.Message) []byte {
	t.Helper()
	b, err := m.Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseReply(t *testing.T) {
	cases := []struct {
		name string
		v6   bool
		pkt  []byte
		want reply
		ok   bool
	}{
		{
			name: "v4 echo reply",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 0x1234, Seq: 7}}),
			want: reply{key: probeKey{proto: ICMP, id: 0x1234, seq: 7}, kind: replyFromTarget},
			ok:   true,
		},
		{
			name: "v4 echo request is not a reply",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 1, Seq: 1}}),
		},
		{
			name: "v4 time exceeded quoting echo",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: inner4(t, protoICMP, echoRequest(false, 0xbeef, 42))}}),
			want: reply{key: probeKey{proto: ICMP, id: 0xbeef, seq: 42}, kind: replyExceeded},
			ok:   true,
		},
		{
			name: "v4 time exceeded quoting udp",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: inner4(t, protoUDP, ports(40000, 33434))}}),
			want: reply{key: probeKey{proto: UDP, id: 40000}, kind: replyExceeded},
			ok:   true,
		},
		{
			name: "v4 port unreachable quoting udp is the target",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 3, Body: &icmp.DstUnreach{Data: inner4(t, protoUDP, ports(40001, 33434))}}),
			want: reply{key: probeKey{proto: UDP, id: 40001}, kind: replyFromTarget, code: 3},
			ok:   true,
		},
		{
			name: "v4 host unreachable quoting udp is a router",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: inner4(t, protoUDP, ports(40002, 33434))}}),
			want: reply{key: probeKey{proto: UDP, id: 40002}, kind: replyUnreachable, code: 1},
			ok:   true,
		},
		{
			name: "v4 time exceeded quoting tcp",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: inner4(t, protoTCP, ports(50000, 80))}}),
			want: reply{key: probeKey{proto: TCP, id: 50000}, kind: replyExceeded},
			ok:   true,
		},
		{
			name: "v4 time exceeded quoting something else",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: inner4(t, 47, ports(1, 2))}}),
		},
		{
			name: "v4 time exceeded with truncated quote",
			pkt:  marshal(t, icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: inner4(t, protoUDP, ports(1, 2))[:22]}}),
		},
		{
			name: "v6 echo reply",
			v6:   true,
			pkt:  marshal(t, icmp.Message{Type: ipv6.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 9, Seq: 10}}),
			want: reply{key: probeKey{proto: ICMP, v6: true, id: 9, seq: 10}, kind: replyFromTarget},
			ok:   true,
		},
		{
			name: "v6 time exceeded quoting echo",
			v6:   true,
			pkt:  marshal(t, icmp.Message{Type: ipv6.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: inner6(protoICMPv6, echoRequest(true, 0x0102, 3))}}),
			want: reply{key: probeKey{proto: ICMP, v6: true, id: 0x0102, seq: 3}, kind: replyExceeded},
			ok:   true,
		},
		{
			name: "v6 port unreachable quoting udp",
			v6:   true,
			pkt:  marshal(t, icmp.Message{Type: ipv6.ICMPTypeDestinationUnreachable, Code: 4, Body: &icmp.DstUnreach{Data: inner6(protoUDP, ports(40003, 33434))}}),
			want: reply{key: probeKey{proto: UDP, v6: true, id: 40003}, kind: replyFromTarget, code: 4},
			ok:   true,
		},
		{
			name: "garbage",
			pkt:  []byte{1, 2, 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseReply(tc.pkt, tc.v6)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v (reply %+v)", ok, tc.ok, got)
			}
			if !ok {
				return
			}
			if got.key != tc.want.key || got.kind != tc.want.kind || got.code != tc.want.code {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
