// Command punchd is the router half of the punch (ADR-0044): it pulls the desired list
// from the cluster, keeps one natmap child per punch alive, renders the DNAT redirect
// into fw4's auto-include and reads it back, and reports what it observes.
//
// It is a static binary (CGO_ENABLED=0) that a procd service runs on the router; it
// needs `natmap` and `nft` on PATH and nothing else. `-dry-run` renders and prints
// without touching either.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/2017fighting/punch/internal/agent"
)

// version is the release identity, baked in at link time by CI (`-X main.version=v0.1.0`).
// A plain `go build` leaves it "dev": the tag is the only thing that names a release, and
// `punchd -version` is how a router answers "which one is running".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("punchd exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "/etc/punch/punch.conf", "path to the `key = value` config file")
		dryRun     = flag.Bool("dry-run", false, "fetch the desired list, print the nft table and the status that would be posted; touch neither nft nor natmap")
		interval   = flag.Duration("interval", 0, "override the reconcile interval (default: the config's `interval`, else 15s)")
		logFormat  = flag.String("log-format", "text", "log format: text or json")
		showVer    = flag.Bool("version", false, "print the build version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch strings.ToLower(*logFormat) {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		return fmt.Errorf("unknown -log-format %q (text or json)", *logFormat)
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	log.Info("config loaded", "path", *configPath, "cluster", cfg.ClusterURL,
		"stun", cfg.STUN, "http", cfg.HTTP, "iface", cfg.Iface, "nftPath", cfg.NFTPath,
		"stateDir", cfg.StateDir, "fwmark", cfg.FWMark, "interval", cfg.Interval.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *dryRun {
		a, err := agent.New(cfg, agent.Deps{Log: log, Out: os.Stdout}, true)
		if err != nil {
			return err
		}
		return a.DryRun(ctx)
	}

	a, err := agent.New(cfg, agent.Deps{
		Natmap: agent.NewNatmap(log),
		Nft:    agent.NewNftExec(),
		Log:    log,
	}, false)
	if err != nil {
		return err
	}
	if *interval > 0 {
		cfg.Interval = *interval
	}
	return a.Run(ctx, cfg.Interval)
}
