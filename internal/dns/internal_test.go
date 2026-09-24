package dns

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientFromResolvConf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	conf := "# generated\nsearch example.com\nnameserver 10.0.0.53\noptions ndots:1 timeout:3 attempts:4\nnameserver fe80::1%eth0 # v6 with a zone\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	c := clientFrom(path)
	if len(c.Nameservers) != 2 || c.Nameservers[0] != "10.0.0.53:53" || c.Nameservers[1] != "[fe80::1%eth0]:53" {
		t.Errorf("nameservers: %v", c.Nameservers)
	}
	if c.Timeout != 3*time.Second || c.Attempts != 4 {
		t.Errorf("options: timeout=%v attempts=%d", c.Timeout, c.Attempts)
	}

	// No file: the stub resolver's loopback defaults, and its default options.
	n := clientFrom(filepath.Join(t.TempDir(), "missing")).normalized()
	if len(n.Nameservers) != 2 || n.Nameservers[0] != "127.0.0.1:53" || n.Timeout != 5*time.Second || n.Attempts != 2 {
		t.Errorf("defaults: %+v", n)
	}
}

func TestWorseOutcome(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "NODATA", "NODATA"},
		{"NODATA", "NXDOMAIN", "NXDOMAIN"},
		{"NXDOMAIN", "NODATA", "NXDOMAIN"},
		{"NODATA", "SERVFAIL", "SERVFAIL"},
		{"REFUSED", "NXDOMAIN", "REFUSED"},
		{"SERVFAIL", "TIMEOUT", "TIMEOUT"},
		{"TIMEOUT", "NODATA", "TIMEOUT"},
	}
	for _, c := range cases {
		if got := worse(c.a, c.b); got != c.want {
			t.Errorf("worse(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
	if got := outcome(Result{Err: context.DeadlineExceeded}); got != "TIMEOUT" {
		t.Errorf("outcome(deadline) = %q", got)
	}
	if got := outcome(Result{Status: "REFUSED"}); got != "REFUSED" {
		t.Errorf("outcome(refused) = %q", got)
	}
}
