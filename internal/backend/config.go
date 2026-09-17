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
	return nil
}

func (c Config) runTTL() time.Duration {
	if c.RunTTL <= 0 {
		return 5 * time.Minute
	}
	return c.RunTTL
}
