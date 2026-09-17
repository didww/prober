// Command prober-agent runs at a PoP: it holds the probing engine, dials the
// backend, and runs the jobs the backend sends. It needs CAP_NET_RAW.
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

	log.Info("prober-agent starting", "version", version, "commit", commit, "site", cfg.Backend.Site)
	a, err := agent.New(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}
