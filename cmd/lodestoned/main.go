package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/config"
	"github.com/Glance-Studios/Lodestone/internal/health"
	"github.com/Glance-Studios/Lodestone/internal/image"
	"github.com/Glance-Studios/Lodestone/internal/k8s"
	"github.com/Glance-Studios/Lodestone/internal/ledger"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
	"github.com/Glance-Studios/Lodestone/internal/server"
	"github.com/Glance-Studios/Lodestone/internal/store"
	"github.com/Glance-Studios/Lodestone/internal/target"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "lodestoned: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("lodestoned", version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	st, err := store.New(filepath.Join(cfg.DataDir, "artifacts"))
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}

	opts := server.Options{Version: version, Store: st}

	// Targets are optional: with none, the agent serves /status and nothing else,
	// which is a useful state for a fresh install.
	if cfg.TargetsFile == "" {
		fmt.Println("targets: none configured (LODESTONE_TARGETS unset)")
	} else {
		targets, err := target.Load(cfg.TargetsFile)
		if err != nil {
			return fmt.Errorf("load targets: %w", err)
		}

		opts.Targets, err = buildTargets(cfg, targets)
		if err != nil {
			return err
		}

		for _, name := range sortedNames(targets) {
			t := targets[name]
			fmt.Printf("target %-16s %s\n", name, t.Describe())
			fmt.Printf("       %-16s %s -> %s at %s\n", "", t.BaseImage, t.Repo, t.DestPath)
			fmt.Printf("       %-16s settle %s, max %d replicas, retain %d\n", "",
				time.Duration(t.SettleTimeout), t.MaxReplicas, t.Retain)
		}
	}

	srv := server.New(opts)
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)

	fmt.Printf("lodestoned %s listening on %s (data %s)\n", version, addr, cfg.DataDir)

	return serve(addr, srv.Handler())
}

// buildTargets turns validated config into a deploy pipeline per target.
//
// One Kubernetes client is shared: it is safe for concurrent use and holds
// connection pools worth reusing. Everything else is per target, including the
// ledger, so a token that reaches one target cannot read another's history.
func buildTargets(cfg config.Config, targets map[string]target.Target) (map[string]server.TargetSpec, error) {
	clientset, err := k8s.NewClientset(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	out := make(map[string]server.TargetSpec, len(targets))

	for name, t := range targets {
		led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger", name+".json"))
		if err != nil {
			return nil, fmt.Errorf("open ledger for %s: %w", name, err)
		}

		dep := k8s.NewDeployment(clientset, t.Namespace, t.Deployment, t.Container)

		rolloutOpts := rollout.Options{
			Checks:        healthChecks(t),
			SettleTimeout: time.Duration(t.SettleTimeout),
		}

		// Bind the target and its options here rather than leaking them into the
		// HTTP layer. The loop variable is captured per iteration, which is safe
		// since Go 1.22.
		deployer := func(ctx context.Context, imageRef string, replicas *int32) <-chan rollout.Event {
			o := rolloutOpts
			o.Replicas = replicas
			return rollout.Deploy(ctx, dep, imageRef, o)
		}

		// One Packager serves both roles: it pushes images and prunes the
		// manifests it pushed, both against the same repository path.
		packager := &image.Packager{
			Base:     t.BaseImage,
			Repo:     t.Repo,
			DestPath: t.DestPath,
		}

		out[name] = server.TargetSpec{
			Config:   t,
			Packager: packager,
			Deployer: deployer,
			Ledger:   led,
			Deleter:  packager,
		}
	}

	return out, nil
}

// healthChecks builds the gate for one target. Nil when nothing is configured,
// which rollout treats as "settled is good enough".
func healthChecks(t target.Target) []health.Check {
	var checks []health.Check

	if t.HealthURL != "" {
		checks = append(checks, health.HTTPCheck{URL: t.HealthURL})
	}
	if t.HealthAddr != "" {
		checks = append(checks, health.TCPCheck{Addr: t.HealthAddr})
	}
	return checks
}

func sortedNames(targets map[string]target.Target) []string {
	return slices.Sorted(maps.Keys(targets))
}

// shutdownGrace bounds how long a shutdown waits for in-flight requests. A
// deploy holds its request open for the whole rollout, so this has to exceed a
// realistic rollout: killing the process mid-rollout is how you end up with a
// half-deployed cluster and nothing to roll it back.
const shutdownGrace = 15 * time.Minute

// serve runs the HTTP server until a termination signal arrives, then stops
// accepting connections and waits for in-flight requests to finish.
func serve(addr string, handler http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: handler}

	// NotifyContext cancels ctx on SIGINT or SIGTERM. Kubernetes sends SIGTERM
	// before SIGKILL, so this is the window we get to finish cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ListenAndServe blocks, so it runs in a goroutine and reports back on a
	// channel. Buffered, so the goroutine never blocks if we return first.
	errs := make(chan error, 1)
	go func() {
		// A clean Shutdown makes ListenAndServe return ErrServerClosed, which is
		// success rather than failure.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server on %s: %w", addr, err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		// The server stopped on its own - a bound port already in use, say.
		return err

	case <-ctx.Done():
		fmt.Printf("shutting down, waiting up to %s for in-flight work\n", shutdownGrace)

		// Stop listening, then wait for handlers to return. Note this uses a
		// fresh context: ctx is already cancelled by the signal, and passing it
		// would abort the shutdown instantly - the opposite of graceful.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		fmt.Println("stopped cleanly")
		return nil
	}
}
