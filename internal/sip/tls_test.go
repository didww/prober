package sip

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// selfSigned makes a self-signed server certificate for 127.0.0.1.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestOptionsTLSInvalidCert(t *testing.T) {
	cert := selfSigned(t)
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("srv"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(l)
	defer ua.Close()
	ap, _ := netip.ParseAddrPort(l.Addr().String())
	time.Sleep(100 * time.Millisecond)

	spec := Spec{Target: ap.Addr(), Port: ap.Port(), Transport: TLS, Cycles: 1, Timeout: 3 * time.Second}
	log := slog.New(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelError}))
	var r *Result
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Run(ctx, spec, log, func(e Event) {
		if e.Kind == ResultDone {
			r = e.Result
		}
	}); err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("no result")
	}
	// The probe reaches the server despite the untrusted cert...
	if !r.Responded || r.StatusCode != 200 {
		t.Fatalf("expected 200 over TLS: responded=%v code=%d", r.Responded, r.StatusCode)
	}
	// ...and flags the certificate as invalid with a reason (for the warning).
	if !r.TLS || r.TLSValid || r.TLSError == "" {
		t.Fatalf("expected an invalid-cert flag: tls=%v valid=%v err=%q", r.TLS, r.TLSValid, r.TLSError)
	}
}
