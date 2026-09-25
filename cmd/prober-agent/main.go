// Command prober-agent runs at a PoP: it holds the probing engine, dials the
// backend, and runs the jobs the backend sends. It needs CAP_NET_RAW.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/didww/prober/internal/agent"
)

var (
	version = "dev"
	commit  = ""
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "prober-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to the YAML configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	checkConfig := flag.Bool("check-config", false, "validate the configuration and exit")
	debug := flag.Bool("debug", false, "log at debug level")
	pprofAddr := flag.String("pprof", "", "serve Go profiling (net/http/pprof) on this address, e.g. 127.0.0.1:6060; off when empty")
	flag.Parse()

	if *showVersion {
		fmt.Println(version, commit)
		return nil
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var cfg agent.Config
	if configPath != nil && *configPath != "" {
		b, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytesReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return err
		}
	}
	if err := cfg.Backend.Validate(); err != nil {
		return err
	}
	if *checkConfig {
		fmt.Printf("config OK: backend %s, site %s\n", cfg.Backend.Address, cfg.Backend.Site)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *pprofAddr != "" {
		ln, err := listenProfiling(*pprofAddr)
		if err != nil {
			return err
		}
		log.Info("profiling enabled", "addr", ln.Addr().String())
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			if err := srv.Serve(ln); err != nil {
				log.Warn("profiling server stopped", "err", err)
			}
		}()
	}

	log.Info("prober-agent starting", "version", version, "commit", commit, "site", cfg.Backend.Site)
	a, err := agent.New(cfg, log, agent.BuildInfo{Version: version, Commit: commit})
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}

// listenProfiling opens the profiling listener, on a loopback address only:
// the handlers expose heap and goroutine dumps of a process holding the
// backend token, so they must not face a network. Binding here rather than
// in the serving goroutine makes a taken port a startup error, not a warning
// after "enabled".
func listenProfiling(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("-pprof: %w", err)
	}
	if host != "localhost" {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("-pprof: %q is not a loopback address; profiling must not face a network", addr)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("-pprof: %w", err)
	}
	return ln, nil
}
