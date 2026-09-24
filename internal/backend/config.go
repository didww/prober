// Package backend is the central component (prober-backend). It has three
// listeners: gRPC for agents, HTTP for the browser (SPA, REST, SSE), and
// Prometheus metrics. It is the only client of the agents and the only
// ingress for the browser.
package backend

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/didww/prober/internal/auth"
)

type Config struct {
	Listen ListenConfig `yaml:"listen"`

	// BasePath mounts the whole app under a sub-path, e.g. "/prober". Empty is
	// the root. The SPA and the auth cookie/redirect are scoped to it.
	BasePath string `yaml:"base_path"`

	// Auth is OIDC login for the browser UI. Off means every request is
	// anonymous — acceptable only on a trusted network. It does not touch the
	// agent gRPC listener, which is authenticated by its own per-agent token.
	Auth auth.Config `yaml:"auth"`

	// AgentToken is a single shared enrollment token. Any agent presenting it
	// registers under the site it declares in its Hello (autoregistration), so
	// a new PoP needs no backend config change — only the token. It is weaker
	// than per-site tokens: whoever holds it can register as any site, so use
	// it on a trusted network, and constrain it with AllowedSites if you can
	// enumerate the sites up front.
	AgentToken string `yaml:"agent_token"`

	// AllowedSites, when set, limits which sites the shared token may register.
	// It does not affect pinned per-site tokens in Agents.
	AllowedSites []string `yaml:"allowed_sites"`

	// Agents pins a bearer token to a specific site. A token here is
	// authoritative for its site over whatever the agent claims in Hello, and
	// takes precedence over the shared token. Use it for sites that need a
	// stronger identity than the shared token gives.
	Agents []AgentAuth `yaml:"agents"`

	// RunTTL is how long a finished run's events stay replayable for a late
	// or reconnecting browser. Zero means 5m.
	RunTTL time.Duration `yaml:"run_ttl"`

	// Monitors are the predefined targets agents probe on their own schedule
	// (stage-2 monitoring). The backend pushes this list to each agent as a
	// versioned Assignment on connect and on SIGHUP reload; results come back as
	// ordinary job events and are turned into Prometheus metrics and, for
	// trace/mtr, VictoriaLogs records.
	Monitors []MonitorConfig `yaml:"monitors"`

	// VictoriaLogs is where trace/mtr monitor results are shipped. Empty URL
	// disables log shipping (metrics still work).
	VictoriaLogs VictoriaLogsConfig `yaml:"victorialogs"`
}

// MonitorConfig is one predefined target and how often to probe it. Kind selects
// the probe: "trace" (mtr), "ping" (single-hop RTT), or "sip" (SIP OPTIONS).
// The kind-specific block (trace or sip) carries the probe parameters; a "ping"
// monitor uses the trace block with mode forced to PING.
type MonitorConfig struct {
	ID        string `yaml:"id"`
	Kind      string `yaml:"kind"`
	Target    string `yaml:"target"`
	IntervalS uint32 `yaml:"interval_s"`
	// Sites restricts the monitor to those sites; empty means every agent runs it.
	Sites  []string          `yaml:"sites"`
	Labels map[string]string `yaml:"labels"`

	Trace *TraceParams `yaml:"trace"`
	SIP   *SipParams   `yaml:"sip"`
}

// TraceParams are the trace/ping probe parameters for a monitor.
type TraceParams struct {
	Protocol     string `yaml:"protocol" json:"protocol"`
	Family       string `yaml:"family" json:"family"`
	Port         uint32 `yaml:"port" json:"port"`
	Cycles       uint32 `yaml:"cycles" json:"cycles"`
	IntervalMS   uint32 `yaml:"interval_ms" json:"interval_ms"`
	FirstTTL     uint32 `yaml:"first_ttl" json:"first_ttl"`
	MaxTTL       uint32 `yaml:"max_ttl" json:"max_ttl"`
	ResolveNames bool   `yaml:"resolve_names" json:"resolve_names"`
}

// SipParams are the SIP OPTIONS probe parameters for a monitor.
type SipParams struct {
	Transport  string `yaml:"transport" json:"transport"`
	Family     string `yaml:"family" json:"family"`
	Port       uint32 `yaml:"port" json:"port"`
	Cycles     uint32 `yaml:"cycles" json:"cycles"`
	IntervalMS uint32 `yaml:"interval_ms" json:"interval_ms"`
	TimeoutMS  uint32 `yaml:"timeout_ms" json:"timeout_ms"`
}

// VictoriaLogsConfig points the trace/mtr log shipper at a VictoriaLogs
// instance. Records are POSTed to {URL}/insert/jsonline as NDJSON.
type VictoriaLogsConfig struct {
	URL          string   `yaml:"url"`
	Username     string   `yaml:"username"`
	Password     string   `yaml:"password"`
	AccountID    int      `yaml:"account_id"`
	ProjectID    int      `yaml:"project_id"`
	StreamFields []string `yaml:"stream_fields"`
	BatchMax     int      `yaml:"batch_max"`
	FlushMS      int      `yaml:"flush_interval_ms"`
}

type ListenConfig struct {
	// GRPC serves the agent gateway. TLS is required.
	GRPC GRPCListen `yaml:"grpc"`
	// HTTP serves the SPA, REST and SSE. TLS optional (nginx may terminate).
	HTTP string `yaml:"http"`
	// Metrics serves /metrics, /healthz, /readyz. Bind to an internal
	// interface; no auth.
	Metrics string `yaml:"metrics"`
}

type GRPCListen struct {
	Addr string `yaml:"addr"`

	// The TLS keypair the gateway presents to agents. Give each either inline
	// (cert / key, a full PEM) or as a path (cert_file / key_file), not both.
	Cert     string `yaml:"cert"`
	CertFile string `yaml:"cert_file"`
	Key      string `yaml:"key"`
	KeyFile  string `yaml:"key_file"`
}

// certificate loads the gateway's TLS keypair from whichever source is set.
func (g GRPCListen) certificate() (tls.Certificate, error) {
	if g.Cert != "" && g.CertFile != "" {
		return tls.Certificate{}, errors.New("listen.grpc: set only one of cert or cert_file")
	}
	if g.Key != "" && g.KeyFile != "" {
		return tls.Certificate{}, errors.New("listen.grpc: set only one of key or key_file")
	}
	certPEM, err := pemBytes(g.Cert, g.CertFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("listen.grpc cert: %w", err)
	}
	keyPEM, err := pemBytes(g.Key, g.KeyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("listen.grpc key: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// pemBytes returns the inline value, or the file's contents, or an error when
// neither is set.
func pemBytes(inline, file string) ([]byte, error) {
	switch {
	case inline != "":
		return []byte(inline), nil
	case file != "":
		return os.ReadFile(file)
	default:
		return nil, errors.New("required (give it inline or as a *_file path)")
	}
}

type AgentAuth struct {
	Site  string `yaml:"site"`
	Token string `yaml:"token"`
}

func (c Config) Validate() error {
	if c.Listen.GRPC.Addr == "" {
		return errors.New("listen.grpc.addr is required")
	}
	if c.Listen.GRPC.Cert == "" && c.Listen.GRPC.CertFile == "" {
		return errors.New("listen.grpc: a certificate is required (cert or cert_file)")
	}
	if c.Listen.GRPC.Key == "" && c.Listen.GRPC.KeyFile == "" {
		return errors.New("listen.grpc: a key is required (key or key_file)")
	}
	if c.Listen.GRPC.Cert != "" && c.Listen.GRPC.CertFile != "" {
		return errors.New("listen.grpc: set only one of cert or cert_file")
	}
	if c.Listen.GRPC.Key != "" && c.Listen.GRPC.KeyFile != "" {
		return errors.New("listen.grpc: set only one of key or key_file")
	}
	if c.Listen.HTTP == "" {
		return errors.New("listen.http is required")
	}
	seen := map[string]bool{}
	tokens := map[string]bool{}
	for i, a := range c.Agents {
		if a.Site == "" || a.Token == "" {
			return fmt.Errorf("agents[%d]: site and token are required", i)
		}
		if seen[a.Site] {
			return fmt.Errorf("agents[%d]: duplicate site %q", i, a.Site)
		}
		if tokens[a.Token] {
			return fmt.Errorf("agents[%d]: duplicate token", i)
		}
		seen[a.Site] = true
		tokens[a.Token] = true
	}
	if err := validateMonitors(c.Monitors); err != nil {
		return err
	}
	return nil
}

// validateMonitors checks the monitor list: unique non-empty ids, a target, a
// positive interval, a known kind, and the matching kind-specific block.
func validateMonitors(mons []MonitorConfig) error {
	ids := map[string]bool{}
	for i, m := range mons {
		if m.ID == "" {
			return fmt.Errorf("monitors[%d]: id is required", i)
		}
		if ids[m.ID] {
			return fmt.Errorf("monitors[%d]: duplicate id %q", i, m.ID)
		}
		ids[m.ID] = true
		if m.Target == "" {
			return fmt.Errorf("monitors[%d] (%s): target is required", i, m.ID)
		}
		if m.IntervalS == 0 {
			return fmt.Errorf("monitors[%d] (%s): interval_s must be > 0", i, m.ID)
		}
		switch m.Kind {
		case "trace", "ping":
			// trace block is optional; sensible defaults are applied.
		case "sip":
			// sip block is optional; sensible defaults are applied.
		case "":
			return fmt.Errorf("monitors[%d] (%s): kind is required (trace, ping or sip)", i, m.ID)
		default:
			return fmt.Errorf("monitors[%d] (%s): unknown kind %q", i, m.ID, m.Kind)
		}
	}
	return nil
}

func (c Config) runTTL() time.Duration {
	if c.RunTTL <= 0 {
		return 5 * time.Minute
	}
	return c.RunTTL
}
