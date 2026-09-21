// Command prober-backend is the central service: it accepts agent gRPC
// streams, serves the browser API and SSE over HTTP, and exposes Prometheus
// metrics, each on its own listener.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/didww/prober/internal/backend"
)

var (
	version = "dev"
	commit  = ""
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "prober-backend:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to the YAML configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	checkConfig := flag.Bool("check-config", false, "validate the configuration and exit")
	debug := flag.Bool("debug", false, "log at debug level")
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

	var cfg backend.Config
	if err := loadYAML(*configPath, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if *checkConfig {
		// config.Load does not look inside the auth block; validate it here so a
		// cookie_secret too short to start is caught before a restart.
		if cfg.Auth.Enabled {
			if err := cfg.Auth.Validate(); err != nil {
				return err
			}
		}
		fmt.Printf("config OK: grpc %s, http %s, %d agent(s), auth %v\n",
			cfg.Listen.GRPC.Addr, cfg.Listen.HTTP, len(cfg.Agents), cfg.Auth.Enabled)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("prober-backend starting", "version", version, "commit", commit)
	srv, err := backend.New(ctx, cfg, log, version, commit)
	if err != nil {
		return err
	}
	go watchReload(ctx, *configPath, srv, log)
	return srv.Run(ctx)
}

// watchReload re-reads the config file on SIGHUP and applies the monitor set
// (only that section is hot-reloaded). A bad reload is logged and the running
// monitor set is kept.
func watchReload(ctx context.Context, path string, srv *backend.Server, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			var cfg backend.Config
			if err := loadYAML(path, &cfg); err != nil {
				log.Error("reload: config read failed", "err", err)
				continue
			}
			if err := cfg.Validate(); err != nil {
				log.Error("reload: config invalid", "err", err)
				continue
			}
			log.Info("SIGHUP: reloading monitors")
			srv.ReloadMonitors(cfg.Monitors)
		}
	}
}

func loadYAML(path string, v any) error {
	if path == "" {
		return fmt.Errorf("a -config file is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytesReader(b))
	dec.KnownFields(true)
	return dec.Decode(v)
}
