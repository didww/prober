// Package backend is the central component (prober-backend). It has three
// listeners: gRPC for agents, HTTP for the browser (SPA, REST, SSE), and
// Prometheus metrics. It is the only client of the agents and the only
// ingress for the browser.
package backend

import (
	"errors"
	"fmt"
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

	// Agents maps a bearer token to the site it authenticates. The agent
	// sends the token in the stream metadata; the site derived here is
	// authoritative over whatever the agent claims in Hello.
	Agents []AgentAuth `yaml:"agents"`

	// RunTTL is how long a finished run's events stay replayable for a late
	// or reconnecting browser. Zero means 5m.
	RunTTL time.Duration `yaml:"run_ttl"`
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
	Addr     string `yaml:"addr"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type AgentAuth struct {
	Site  string `yaml:"site"`
	Token string `yaml:"token"`
}

func (c Config) Validate() error {
	if c.Listen.GRPC.Addr == "" {
		return errors.New("listen.grpc.addr is required")
	}
	if c.Listen.GRPC.CertFile == "" || c.Listen.GRPC.KeyFile == "" {
		return errors.New("listen.grpc.cert_file and key_file are required")
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
	return nil
}

func (c Config) runTTL() time.Duration {
	if c.RunTTL <= 0 {
		return 5 * time.Minute
	}
	return c.RunTTL
}
