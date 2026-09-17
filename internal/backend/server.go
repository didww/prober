package backend

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/didww/prober/internal/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// Server wires the three listeners together and runs them under one context.
type Server struct {
	cfg     Config
	log     *slog.Logger
	gw      *Gateway
	mgr     *Manager
	api     *API
	auth    *auth.Auth
	version string
	commit  string

	ready readyFlag
}

// New builds the server. It sets up OIDC without contacting the IdP (discovery
// runs in the background), so an unreachable provider delays signing in rather
// than starting.
func New(ctx context.Context, cfg Config, log *slog.Logger, version, commit string) (*Server, error) {
	gw := NewGateway(cfg, log)
	mgr := NewManager(gw, cfg.runTTL())

	if cfg.AgentToken == "" && len(cfg.Agents) == 0 {
		log.Warn("no agent auth configured: no agent can connect until you set agent_token (shared) or agents (per-site)")
	} else if cfg.AgentToken != "" {
		log.Info("shared agent token enabled: agents autoregister under the site they declare",
			"pinned_sites", len(cfg.Agents), "allowed_sites", len(cfg.AllowedSites))
	}

	var a *auth.Auth
	if cfg.Auth.Enabled {
		var err error
		a, err = auth.New(ctx, cfg.Auth, cfg.BasePath, log)
		if err != nil {
			return nil, err
		}
	} else {
		log.Warn("authentication is disabled: every browser request is anonymous")
	}

	return &Server{
		cfg:     cfg,
		log:     log,
		gw:      gw,
		mgr:     mgr,
		api:     NewAPI(gw, mgr, a, log, version, commit),
		auth:    a,
		version: version,
		commit:  commit,
	}, nil
}

// Run starts all three listeners and blocks until ctx is cancelled, then
// shuts them down in order: stop taking browser traffic, stop the gateway so
// agents reconnect elsewhere, then the metrics endpoint last.
func (s *Server) Run(ctx context.Context) error {
	grpcSrv, err := s.grpcServer()
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: s.cfg.Listen.HTTP, Handler: s.httpHandler(), ReadHeaderTimeout: 10 * time.Second}
	var metricsSrv *http.Server
	if s.cfg.Listen.Metrics != "" {
		metricsSrv = &http.Server{Addr: s.cfg.Listen.Metrics, Handler: s.metricsHandler(), ReadHeaderTimeout: 10 * time.Second}
	}

	grpcLn, err := net.Listen("tcp", s.cfg.Listen.GRPC.Addr)
	if err != nil {
		return err
	}
	httpLn, err := net.Listen("tcp", s.cfg.Listen.HTTP)
	if err != nil {
		grpcLn.Close()
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ignoreClosed(grpcSrv.Serve(grpcLn)) })
	g.Go(func() error { return ignoreClosed(httpSrv.Serve(httpLn)) })
	if metricsSrv != nil {
		g.Go(func() error { return ignoreClosed(metricsSrv.ListenAndServe()) })
	}
	g.Go(func() error { s.mgr.Reap(ctx); return nil })

	s.ready.set(true)
	s.log.Info("prober-backend listening",
		"grpc", s.cfg.Listen.GRPC.Addr, "http", s.cfg.Listen.HTTP, "metrics", s.cfg.Listen.Metrics,
		"agents", len(s.cfg.Agents))

	<-ctx.Done()
	s.ready.set(false)
	s.log.Info("shutting down")

	sh, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sh)
	grpcSrv.GracefulStop()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(sh)
	}
	return g.Wait()
}

func (s *Server) grpcServer() (*grpc.Server, error) {
	cert, err := tls.LoadX509KeyPair(s.cfg.Listen.GRPC.CertFile, s.cfg.Listen.GRPC.KeyFile)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 20 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	pb.RegisterAgentGatewayServer(srv, s.gw)
	return srv, nil
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}
