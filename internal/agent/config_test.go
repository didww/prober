package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func caPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func base(t *testing.T) BackendConfig {
	return BackendConfig{Address: "backend:50051", Site: "fra", Token: "tok"}
}

func TestTokenSources(t *testing.T) {
	dir := t.TempDir()

	// Inline.
	if tok, err := base(t).token(); err != nil || tok != "tok" {
		t.Fatalf("inline token: %q %v", tok, err)
	}

	// File, with a trailing newline that must be trimmed.
	f := write(t, dir, "token", "s3cr3t\n")
	c := BackendConfig{Address: "b:1", Site: "fra", TokenFile: f}
	if tok, err := c.token(); err != nil || tok != "s3cr3t" {
		t.Fatalf("file token: %q %v", tok, err)
	}

	// Both set is an error.
	c = BackendConfig{Token: "a", TokenFile: f}
	if _, err := c.token(); err == nil {
		t.Fatal("token + token_file must be rejected")
	}

	// Neither set is an error.
	if _, err := (BackendConfig{}).token(); err == nil {
		t.Fatal("missing token must be rejected")
	}

	// Empty file is an error, not an empty token.
	empty := write(t, dir, "empty", "\n  \n")
	if _, err := (BackendConfig{TokenFile: empty}).token(); err == nil {
		t.Fatal("empty token_file must be rejected")
	}

	// Missing file is an error.
	if _, err := (BackendConfig{TokenFile: filepath.Join(dir, "nope")}).token(); err == nil {
		t.Fatal("missing token_file must be rejected")
	}
}

func TestTLSTrust(t *testing.T) {
	dir := t.TempDir()
	ca := caPEM(t)

	// System roots when nothing is given: RootCAs nil.
	cfg, err := base(t).tlsConfig()
	if err != nil || cfg.RootCAs != nil {
		t.Fatalf("system roots: %v %v", cfg, err)
	}

	// Inline CA.
	c := base(t)
	c.CA = ca
	cfg, err = c.tlsConfig()
	if err != nil || cfg.RootCAs == nil {
		t.Fatalf("inline ca: %v %v", cfg, err)
	}

	// CA file.
	f := write(t, dir, "ca.pem", ca)
	c = base(t)
	c.CAFile = f
	cfg, err = c.tlsConfig()
	if err != nil || cfg.RootCAs == nil {
		t.Fatalf("ca file: %v %v", cfg, err)
	}

	// ServerName is carried through.
	c = base(t)
	c.ServerName = "backend.example"
	cfg, _ = c.tlsConfig()
	if cfg.ServerName != "backend.example" {
		t.Fatalf("server_name: %q", cfg.ServerName)
	}

	// Both CA sources set is an error.
	c = base(t)
	c.CA, c.CAFile = ca, f
	if _, err := c.tlsConfig(); err == nil {
		t.Fatal("ca + ca_file must be rejected")
	}

	// Garbage PEM is an error.
	c = base(t)
	c.CA = "not a certificate"
	if _, err := c.tlsConfig(); err == nil {
		t.Fatal("invalid ca pem must be rejected")
	}

	// Missing CA file is an error.
	c = base(t)
	c.CAFile = filepath.Join(dir, "nope.pem")
	if _, err := c.tlsConfig(); err == nil {
		t.Fatal("missing ca_file must be rejected")
	}

	// InsecureSkipVerify turns verification off and needs no trust source.
	c = base(t)
	c.InsecureSkipVerify = true
	cfg, err = c.tlsConfig()
	if err != nil || !cfg.InsecureSkipVerify {
		t.Fatalf("insecure: %v %v", cfg, err)
	}

	// A CA alongside insecure is a mistake and is rejected.
	c = base(t)
	c.InsecureSkipVerify = true
	c.CA = ca
	if _, err := c.tlsConfig(); err == nil {
		t.Fatal("ca + insecure_skip_verify must be rejected")
	}
}

func TestValidate(t *testing.T) {
	if err := base(t).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := (BackendConfig{Site: "fra", Token: "t"}).Validate(); err == nil {
		t.Fatal("missing address must fail")
	}
	if err := (BackendConfig{Address: "b:1", Token: "t"}).Validate(); err == nil {
		t.Fatal("missing site must fail")
	}
	if err := (BackendConfig{Address: "b:1", Site: "fra"}).Validate(); err == nil {
		t.Fatal("missing token must fail")
	}
}

// mkLeaf makes a CA and a server cert signed by it with the given SAN.
func mkLeaf(t *testing.T, san string) (caPEMStr string, serverCert tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caPEMStr = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))

	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: san},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{san}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	caCert, _ := x509.ParseCertificate(caDER)
	srvDER, _ := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	serverCert = tls.Certificate{Certificate: [][]byte{srvDER}, PrivateKey: srvKey}
	return
}

// handshake dials a TLS server presenting serverCert, with the given client
// config, over a loopback listener. Returns the client's handshake error.
func handshake(t *testing.T, clientCfg *tls.Config, serverCert tls.Certificate) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.(*tls.Conn).Handshake()
		c.Close()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func TestSkipHostnameVerify(t *testing.T) {
	ca, serverCert := mkLeaf(t, "the-real-name") // cert is for "the-real-name"

	// Full verification against a DIFFERENT dialled name fails on the hostname.
	full := BackendConfig{Address: "svc:1", Site: "fra", Token: "t", CA: ca, ServerName: "k8s-service-name"}
	cfg, err := full.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if handshake(t, cfg, serverCert) == nil {
		t.Fatal("full verification must reject a name not in the certificate")
	}

	// skip_hostname_verify: the same mismatch is accepted, because the chain to
	// the CA is what is checked, not the name.
	skip := BackendConfig{Address: "svc:1", Site: "fra", Token: "t", CA: ca, ServerName: "k8s-service-name", SkipHostnameVerify: true}
	cfg, err = skip.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, cfg, serverCert); err != nil {
		t.Fatalf("skip_hostname_verify must accept a CA-signed cert regardless of name: %v", err)
	}

	// But a cert signed by a DIFFERENT CA is still rejected — the chain check
	// remains.
	otherCA, _ := mkLeaf(t, "the-real-name")
	skipOther := BackendConfig{Address: "svc:1", Site: "fra", Token: "t", CA: otherCA, SkipHostnameVerify: true}
	cfg, _ = skipOther.tlsConfig()
	if handshake(t, cfg, serverCert) == nil {
		t.Fatal("skip_hostname_verify must still reject a cert that does not chain to the trusted CA")
	}
}

func TestDNSConfigClient(t *testing.T) {
	if c, err := (DNSConfig{}).client(); err != nil || c != nil {
		t.Fatalf("empty: client=%v err=%v", c, err)
	}
	c, err := (DNSConfig{Nameservers: []string{"10.0.0.53", "[2001:db8::53]:5353", "192.0.2.1:53"}}).client()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.53:53", "[2001:db8::53]:5353", "192.0.2.1:53"}
	if len(c.Nameservers) != len(want) {
		t.Fatalf("got %v, want %v", c.Nameservers, want)
	}
	for i := range want {
		if c.Nameservers[i] != want[i] {
			t.Errorf("nameserver %d: got %q, want %q", i, c.Nameservers[i], want[i])
		}
	}
	for _, bad := range []string{"", "ns.example.com", "10.0.0.53:x"} {
		if _, err := (DNSConfig{Nameservers: []string{bad}}).client(); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}
