// Package agent is the PoP-side component (prober-agent): it holds the
// probing engine, dials the backend, and runs the jobs the backend sends.
//
// The agent never listens for inbound connections. It opens one long-lived
// gRPC stream to the backend, authenticated by a per-agent bearer token over
// server-side TLS.
package agent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// BackendConfig is how this agent reaches the backend.
type BackendConfig struct {
	// Address is host:port of the backend's gRPC listener.
	Address string `yaml:"address"`

	// Site is this agent's location id (fra, ams, …). The backend also
	// derives the site from the token and treats that as authoritative; this
	// is sent in Hello and cross-checked, and it labels local logs.
	Site string `yaml:"site"`

	// The bearer token identifying this agent. Exactly one of Token and
	// TokenFile must be set. A file is preferred: it keeps the secret out of
	// the config and is what systemd LoadCredential and a k8s Secret deliver.
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`

	// Trust for the backend's TLS certificate. All empty means the system CA
	// roots, which is right when the backend uses a public or already-trusted
	// CA. For an internal CA, give it here as a PEM: CAFile points at a file,
	// CA carries it inline (for a container that would rather not mount one).
	// At most one of CA and CAFile may be set.
	CA     string `yaml:"ca"`
	CAFile string `yaml:"ca_file"`

	// ServerName overrides the name verified in the backend's certificate,
	// for when Address is an IP or differs from the certificate's name.
	ServerName string `yaml:"server_name"`

	// InsecureSkipVerify turns OFF verification of the backend's certificate.
	// The connection stays encrypted but is no longer authenticated, so a
	// man-in-the-middle could impersonate the backend and capture this
	// agent's token. It exists for development and self-signed testing only;
	// never set it in production. The agent logs a warning when it is on.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`

	// DialTimeout bounds the initial connection. Zero means 10s.
	DialTimeout time.Duration `yaml:"dial_timeout"`
}

// token returns the effective bearer token, reading TokenFile if set.
//
// A file is read at startup: a missing or empty one is a refusal to start,
// not a silent fallback to an empty token. Trailing whitespace is trimmed,
// because `echo > token` leaves a newline that would otherwise be sent as
// part of the credential.
func (c BackendConfig) token() (string, error) {
	switch {
	case c.Token != "" && c.TokenFile != "":
		return "", errors.New("backend: set only one of token or token_file")
	case c.TokenFile != "":
		b, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return "", fmt.Errorf("backend: reading token_file: %w", err)
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("backend: token_file %s is empty", c.TokenFile)
		}
		return tok, nil
	case c.Token != "":
		return c.Token, nil
	default:
		return "", errors.New("backend: a token is required (token or token_file)")
	}
}

// tlsConfig builds the client TLS configuration: the trust roots and the
// verification name. An empty trust source keeps RootCAs nil, which tells
// crypto/tls to use the system store.
func (c BackendConfig) tlsConfig() (*tls.Config, error) {
	if c.CA != "" && c.CAFile != "" {
		return nil, errors.New("backend: set only one of ca or ca_file")
	}

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: c.ServerName,
	}

	if c.InsecureSkipVerify {
		// Verification is off, so trust roots are moot: whatever the backend
		// presents is accepted. A CA alongside this is almost certainly a
		// mistake, so it is rejected rather than silently ignored.
		if c.CA != "" || c.CAFile != "" {
			return nil, errors.New("backend: ca/ca_file is meaningless with insecure_skip_verify")
		}
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}

	var pem []byte
	switch {
	case c.CAFile != "":
		b, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("backend: reading ca_file: %w", err)
		}
		pem = b
	case c.CA != "":
		pem = []byte(c.CA)
	default:
		return cfg, nil // system roots
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		src := "ca"
		if c.CAFile != "" {
			src = "ca_file (" + c.CAFile + ")"
		}
		return nil, fmt.Errorf("backend: %s contained no valid PEM certificate", src)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// Validate reports whether this config could start, without dialing. Secret
// files are read here, so a missing token or an unreadable CA is caught at
// startup or by a -check-config, not on the first connection.
func (c BackendConfig) Validate() error {
	if c.Address == "" {
		return errors.New("backend: address is required")
	}
	if c.Site == "" {
		return errors.New("backend: site is required")
	}
	if _, err := c.token(); err != nil {
		return err
	}
	if _, err := c.tlsConfig(); err != nil {
		return err
	}
	return nil
}

func (c BackendConfig) dialTimeout() time.Duration {
	if c.DialTimeout <= 0 {
		return 10 * time.Second
	}
	return c.DialTimeout
}
