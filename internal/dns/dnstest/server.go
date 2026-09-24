// Package dnstest is a nameserver for tests: it listens on loopback over
// both UDP and TCP and answers from a table, so the engine and anything built
// on it can be exercised without a network.
package dnstest

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// SRV is one SRV record in an Answer.
type SRV struct {
	Target                 string
	Priority, Weight, Port uint16
}

// Answer is what the server says to one question. A name not in the table
// gets NXDOMAIN, or the server's default code when one is set.
type Answer struct {
	RCode dnsmessage.RCode
	// CNAME, when set, makes the records owned by that name, behind a CNAME
	// from the name asked.
	CNAME string
	Addrs []string // A and AAAA, by address family
	SRV   []SRV
	PTR   []string
	// TruncateUDP answers with the TC bit and no records over UDP, so the
	// client must retry over TCP; TruncateTCP does the same over TCP, which
	// no correct server does.
	TruncateUDP bool
	TruncateTCP bool
	// WrongQuestion echoes a different question than was asked.
	WrongQuestion bool
}

// Server is a running fake nameserver. Addr is its host:port, the same for
// UDP and TCP.
type Server struct {
	Addr string

	tb           testing.TB
	mu           sync.Mutex
	table        map[string]Answer
	defaultRCode dnsmessage.RCode
	dropAll      bool
	asked        []string
}

// Start serves table on loopback until the test ends. Keys are "TYPE name",
// e.g. "A example.com" or "SRV _sip._udp.example.com".
func Start(tb testing.TB, table map[string]Answer) *Server {
	tb.Helper()
	s := &Server{tb: tb, table: table, defaultRCode: dnsmessage.RCodeNameError}
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	s.Addr = tl.Addr().String()
	pc, err := net.ListenPacket("udp", s.Addr)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { tl.Close(); pc.Close() })
	go s.serveUDP(pc)
	go s.serveTCP(tl)
	return s
}

// SetDefaultRCode is the code for names not in the table, NXDOMAIN unless set.
func (s *Server) SetDefaultRCode(rc dnsmessage.RCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultRCode = rc
}

// SetDropAll makes the server ignore every question, like a dead one.
func (s *Server) SetDropAll(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropAll = drop
}

// Asked is how many times a question ("TYPE name") has been received.
func (s *Server) Asked(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.asked {
		if k == key {
			n++
		}
	}
	return n
}

// AskedTotal is how many questions have been received.
func (s *Server) AskedTotal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.asked)
}

func (s *Server) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := s.respond(buf[:n], true); resp != nil {
			_, _ = pc.WriteTo(resp, from)
		}
	}
}

func (s *Server) serveTCP(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var hdr [2]byte
			if _, err := io.ReadFull(c, hdr[:]); err != nil {
				return
			}
			msg := make([]byte, binary.BigEndian.Uint16(hdr[:]))
			if _, err := io.ReadFull(c, msg); err != nil {
				return
			}
			resp := s.respond(msg, false)
			if resp == nil {
				return
			}
			out := make([]byte, 2+len(resp))
			binary.BigEndian.PutUint16(out, uint16(len(resp)))
			copy(out[2:], resp)
			_, _ = c.Write(out)
		}()
	}
}

var typeNames = map[dnsmessage.Type]string{
	dnsmessage.TypeA: "A", dnsmessage.TypeAAAA: "AAAA", dnsmessage.TypeSRV: "SRV", dnsmessage.TypePTR: "PTR",
}

func (s *Server) respond(msg []byte, udp bool) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	key := typeNames[q.Type] + " " + strings.TrimSuffix(q.Name.String(), ".")

	s.mu.Lock()
	s.asked = append(s.asked, key)
	ans, ok := s.table[key]
	if !ok {
		ans = Answer{RCode: s.defaultRCode}
	}
	drop := s.dropAll
	s.mu.Unlock()
	if drop {
		return nil
	}

	truncated := (udp && ans.TruncateUDP) || (!udp && ans.TruncateTCP)
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, RecursionDesired: true, RecursionAvailable: true,
		RCode: ans.RCode, Truncated: truncated,
	})
	b.EnableCompression()
	must := func(err error) {
		if err != nil {
			s.tb.Errorf("dnstest: %v", err)
		}
	}
	must(b.StartQuestions())
	if ans.WrongQuestion {
		q.Name = dnsmessage.MustNewName("other.invalid.")
	}
	must(b.Question(q))
	must(b.StartAnswers())
	if !truncated {
		owner := q.Name
		if ans.CNAME != "" {
			target := dnsmessage.MustNewName(ans.CNAME + ".")
			must(b.CNAMEResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 30},
				dnsmessage.CNAMEResource{CNAME: target}))
			owner = target
		}
		for _, a := range ans.Addrs {
			ip := netip.MustParseAddr(a)
			if ip.Is4() {
				must(b.AResource(dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 300},
					dnsmessage.AResource{A: ip.As4()}))
			} else {
				must(b.AAAAResource(dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 300},
					dnsmessage.AAAAResource{AAAA: ip.As16()}))
			}
		}
		for _, r := range ans.SRV {
			// The root "." is already fully qualified.
			target := r.Target
			if target != "." {
				target += "."
			}
			must(b.SRVResource(dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: 60},
				dnsmessage.SRVResource{Priority: r.Priority, Weight: r.Weight, Port: r.Port, Target: dnsmessage.MustNewName(target)}))
		}
		for _, name := range ans.PTR {
			must(b.PTRResource(dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 3600},
				dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(name + ".")}))
		}
	}
	out, err := b.Finish()
	if err != nil {
		s.tb.Errorf("dnstest: %v", err)
		return nil
	}
	return out
}
