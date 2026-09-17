package trace

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Multi-hop tests. They build a chain of network namespaces:
//
//	test process ── r1 ── r2 ── target
//	10.9.0.1/24  .2  .1/24 .2  .1/24 .2   (10.9.0, 10.9.1, 10.9.2)
//	fd09:0::1    ::2 fd09:1::1 ::2  fd09:2::1 ::2
//
// so that a TTL-1 probe is answered by r1, TTL 2 by r2 and TTL 3 by the
// target, over IPv4 and IPv6, for every protocol. Real time-exceeded
// messages from a real forwarding path are what the engine is for; loopback
// cannot produce them.
//
// They need the test process to be root in a user namespace with its own
// network namespace, which `unshare -Urn` gives any user on a stock kernel,
// plus iproute2. Anything else skips.

type chain struct {
	t    *testing.T
	pids []int // one sleeping process per extra namespace
}

func (c *chain) sh(format string, args ...any) string {
	c.t.Helper()
	cmd := fmt.Sprintf(format, args...)
	out, err := exec.Command("sh", "-ec", cmd).CombinedOutput()
	if err != nil {
		c.t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

// in runs a command inside namespace i (1-based) of the chain.
func (c *chain) in(i int, format string, args ...any) string {
	c.t.Helper()
	return c.sh("nsenter -t %d -n sh -ec %s", c.pids[i-1], shellQuote(fmt.Sprintf(format, args...)))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newNS starts a process in a fresh network namespace and returns its pid,
// which is the handle nsenter needs.
func (c *chain) newNS() int {
	c.t.Helper()
	cmd := exec.Command("unshare", "-n", "sh", "-c", "echo $$; exec sleep 300")
	out, err := cmd.StdoutPipe()
	if err != nil {
		c.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	var line string
	if _, err := fmt.Fscanln(out, &line); err != nil {
		c.t.Fatal(err)
	}
	pid, err := strconv.Atoi(line)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return pid
}

func buildChain(t *testing.T) *chain {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("multi-hop tests need to be root in a user namespace: unshare -Urn")
	}
	for _, bin := range []string{"ip", "nsenter", "unshare"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	c := &chain{t: t}
	// Every chain test shares the test process's namespace. Whatever the
	// previous one left there, a veth and static routes, goes first; the
	// other namespaces die with their processes.
	const reset = "ip link del v1a 2>/dev/null; ip route flush proto boot 2>/dev/null; ip -6 route flush proto boot 2>/dev/null; true"
	_ = exec.Command("sh", "-c", reset).Run()
	t.Cleanup(func() { _ = exec.Command("sh", "-c", reset).Run() })
	c.sh("ip link set lo up")

	// Three more namespaces: r1, r2, target.
	for range 3 {
		c.pids = append(c.pids, c.newNS())
	}
	for _, pid := range c.pids {
		c.sh("nsenter -t %d -n ip link set lo up", pid)
	}

	// Link i joins namespace i-1 (0 = the test process) to namespace i.
	for i := 1; i <= 3; i++ {
		a, b := fmt.Sprintf("v%da", i), fmt.Sprintf("v%db", i)
		c.sh("ip link add %s type veth peer name %s", a, b)
		c.sh("ip link set %s netns %d", b, c.pids[i-1])
		if i > 1 {
			c.sh("ip link set %s netns %d", a, c.pids[i-2])
		}
		// The near end: .1 in the test process for link 1, else in ns i-1.
		up := func(ns int, dev, v4, v6 string) {
			if ns == 0 {
				c.sh("ip addr add %s dev %s && ip -6 addr add %s dev %s && ip link set %s up", v4, dev, v6, dev, dev)
			} else {
				c.in(ns, "ip addr add %s dev %s && ip -6 addr add %s dev %s && ip link set %s up", v4, dev, v6, dev, dev)
			}
		}
		up(i-1, a, fmt.Sprintf("10.9.%d.1/24", i-1), fmt.Sprintf("fd09:%x::1/64", i-1))
		up(i, b, fmt.Sprintf("10.9.%d.2/24", i-1), fmt.Sprintf("fd09:%x::2/64", i-1))
	}

	// Routes: everything forward goes to the next hop; everything back to
	// the previous. Routers forward.
	c.sh("ip route add 10.9.0.0/16 via 10.9.0.2 && ip -6 route add fd09::/16 via fd09:0::2")
	c.in(1, "echo 1 > /proc/sys/net/ipv4/ip_forward && echo 1 > /proc/sys/net/ipv6/conf/all/forwarding")
	c.in(1, "echo 0 > /proc/sys/net/ipv4/icmp_ratelimit && echo 0 > /proc/sys/net/ipv6/icmp/ratelimit")
	c.in(1, "ip route add 10.9.0.0/16 via 10.9.1.2 && ip -6 route add fd09::/16 via fd09:1::2")
	c.in(2, "echo 1 > /proc/sys/net/ipv4/ip_forward && echo 1 > /proc/sys/net/ipv6/conf/all/forwarding")
	c.in(2, "echo 0 > /proc/sys/net/ipv4/icmp_ratelimit && echo 0 > /proc/sys/net/ipv6/icmp/ratelimit")
	c.in(2, "ip route add 10.9.0.0/24 via 10.9.1.1 && ip -6 route add fd09:0::/64 via fd09:1::1")
	c.in(3, "ip route add 10.9.0.0/16 via 10.9.2.1 && ip -6 route add fd09::/16 via fd09:2::1")

	// Duplicate address detection would leave the v6 addresses tentative
	// for a second; wait until every one is usable.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out := c.sh("ip -6 addr show tentative")
		out += c.in(1, "ip -6 addr show tentative") + c.in(2, "ip -6 addr show tentative") + c.in(3, "ip -6 addr show tentative")
		if strings.TrimSpace(out) == "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return c
}

// expectChain checks that hop n is answered by the address expected for it
// and that the target is reached at TTL 3.
func expectChain(t *testing.T, cs []*Cycle, v6 bool) {
	t.Helper()
	want := []string{"10.9.0.2", "10.9.1.2", "10.9.2.2"}
	if v6 {
		want = []string{"fd09::2", "fd09:1::2", "fd09:2::2"}
	}
	last := cs[len(cs)-1]
	if last.ReachedAt != 3 {
		t.Fatalf("reached at %d, want 3: %+v", last.ReachedAt, last)
	}
	if len(last.Hops) != 3 {
		t.Fatalf("%d hops, want 3: %+v", len(last.Hops), last.Hops)
	}
	for i, h := range last.Hops {
		if h.TTL != i+1 {
			t.Fatalf("hop %d has ttl %d", i, h.TTL)
		}
		if len(h.Addresses) != 1 || h.Addresses[0].Addr.String() != want[i] {
			t.Fatalf("hop %d answered by %+v, want %s", h.TTL, h.Addresses, want[i])
		}
		if h.Received != len(cs) || h.LossPct != 0 || h.Sample == nil {
			t.Fatalf("hop %d stats: %+v", h.TTL, h)
		}
		if h.Best <= 0 || h.Best > h.Worst || h.Avg < h.Best || h.Avg > h.Worst {
			t.Fatalf("hop %d timing: best %v avg %v worst %v", h.TTL, h.Best, h.Avg, h.Worst)
		}
	}
}

func chainSpec(v6 bool, proto Protocol) Spec {
	target := "10.9.2.2"
	if v6 {
		target = "fd09:2::2"
	}
	return Spec{
		Target:       netip.MustParseAddr(target),
		Protocol:     proto,
		Cycles:       3,
		Interval:     200 * time.Millisecond,
		ProbeTimeout: time.Second,
		MaxTTL:       10,
	}
}

func TestChain(t *testing.T) {
	buildChain(t)
	e := testEngine(t)

	for _, v6 := range []bool{false, true} {
		for _, proto := range []Protocol{ICMP, UDP, TCP} {
			name := fmt.Sprintf("%s/v%d", proto, 4)
			if v6 {
				name = fmt.Sprintf("%s/v6", proto)
			}
			t.Run(name, func(t *testing.T) {
				cs := cycles(t, runTrace(t, e, chainSpec(v6, proto)), 3)
				expectChain(t, cs, v6)
				// After the first cycle the engine knows the target is at
				// TTL 3 and must not probe beyond it.
				for _, c := range cs[1:] {
					if len(c.Hops) != 3 {
						t.Fatalf("cycle %d probed %d hops after the target was found", c.Number, len(c.Hops))
					}
				}
			})
		}
	}
}

// TestChainTCPOpenPort: a listener on the target, so the last hop resolves
// through a completed handshake rather than a refusal. The listener is this
// test binary re-executed inside the target namespace (see TestMain).
func TestChainTCPOpenPort(t *testing.T) {
	c := buildChain(t)
	e := testEngine(t)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv := exec.Command("nsenter", "-t", strconv.Itoa(c.pids[2]), "-n", exe)
	srv.Env = append(os.Environ(), "PROBER_TEST_HELPER=tcplisten")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	time.Sleep(300 * time.Millisecond)

	spec := chainSpec(false, TCP)
	spec.Port = helperPort
	cs := cycles(t, runTrace(t, e, spec), 3)
	expectChain(t, cs, false)
}

const helperPort = 8080

// TestMain doubles as the in-namespace helper process for the chain tests.
func TestMain(m *testing.M) {
	if os.Getenv("PROBER_TEST_HELPER") == "tcplisten" {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", helperPort))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for {
			c, err := l.Accept()
			if err != nil {
				os.Exit(0)
			}
			c.Close()
		}
	}
	os.Exit(m.Run())
}
