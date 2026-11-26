// Command punch-controller is the cluster half of the punch (ADR-0044): it serves the
// desired list to the router agent, turns the agent's reports into `Punch` status and
// into the `<name>-punch` ConfigMap, and exports the metrics home-ops' alerts read.
//
// It talks to the apiserver through client-go's dynamic client, as the ServiceAccount
// from home-ops/infrastructure/punch/rbac.yaml (get/list/watch punches, get/patch/update
// punches/status, full configmaps — and nothing else).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/2017fighting/punch/internal/controller"
)

func main() {
	if err := run(); err != nil {
		slog.Error("punch-controller exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen     = flag.String("listen", ":8080", "address to serve /punches/desired, /punches/status and /metrics on")
		tokenEnv   = flag.String("token-env", "PUNCH_BEARER_TOKEN", "name of the environment variable holding the bearer token the agent must present")
		resync     = flag.Duration("resync", 15*time.Second, "how often the reconcile loop applies the deadline and bindPort rules")
		deadline   = flag.Duration("deadline", 60*time.Second, "how long a punch may go without a report before it is marked down (= four missed 15s ticks)")
		kubeconfig = flag.String("kubeconfig", "", "path to a kubeconfig (local runs only; in-cluster config is used when empty)")
		logFormat  = flag.String("log-format", "text", "log format: text or json")
	)
	flag.Parse()

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

	token := os.Getenv(*tokenEnv)
	if token == "" {
		// Not a warning: an empty token would make the route an unauthenticated write
		// path into every app's ConfigMap, and the route has no edge auth by design.
		return fmt.Errorf("refusing to start: %s is empty, so the API endpoints would be unauthenticated", *tokenEnv)
	}
	for _, v := range []string{*tokenEnv, "POD_NAMESPACE", "HOSTNAME"} {
		log.Info("env", "name", v, "set", os.Getenv(v) != "")
	}

	cfg, err := kubeRESTConfig(*kubeconfig)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}

	ctrl, err := controller.New(controller.Options{Dynamic: dyn, Deadline: *deadline, Log: log})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go ctrl.Run(ctx, *resync)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           ctrl.Handler(token),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *listen, "resync", resync.String(), "deadline", deadline.String(),
			"endpoints", "/punches/desired (GET), /punches/status (POST), /metrics (GET)")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func kubeRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %q: %w", kubeconfig, err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config (run inside the cluster, or pass -kubeconfig): %w", err)
	}
	return cfg, nil
}
