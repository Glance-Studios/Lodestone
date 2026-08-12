package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

	if cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "warning: LODESTONE_TOKEN not set; protected endpoints will reject every request")
	}

	st, err := store.New(filepath.Join(cfg.DataDir, "artifacts"))
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}

	led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger.json"))
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}

	opts := server.Options{
		Version: version,
		Token:   cfg.Token,
		Store:   st,
		Ledger:  led,
	}

	// Deploying is opt-in: without a configured target the agent is still a
	// useful upload-and-ledger service, and POST /deploy answers 501.
	if cfg.DeployEnabled() {
		packager, deployer, err := deployPipeline(cfg)
		if err != nil {
			return err
		}
		opts.Packager = packager
		opts.Deployer = deployer

		fmt.Printf("deploy target: %s/%s container %s\n", cfg.Namespace, cfg.Deployment, cfg.Container)
		fmt.Printf("packaging:     %s -> %s at %s\n", cfg.BaseImage, cfg.Repo, cfg.DestPath)
	} else {
		fmt.Println("deploy target: not configured (POST /deploy disabled)")
	}

	srv := server.New(opts)
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)

	fmt.Printf("lodestoned %s listening on %s (data %s)\n", version, addr, cfg.DataDir)

	return serve(addr, srv.Handler())
}

// shutdownGrace bounds how long a shutdown waits for in-flight requests. A
// deploy holds its request open for the whole rollout, so this has to exceed a
// realistic rollout: killing the process mid-rollout is how you end up with a
// half-deployed cluster and nothing to roll it back.
const shutdownGrace = 5 * time.Minute

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

// deployPipeline builds the packager and the rollout driver from config.
func deployPipeline(cfg config.Config) (server.Packager, server.Deployer, error) {
	clientset, err := k8s.NewClientset(cfg.Kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes client: %w", err)
	}

	target := k8s.NewDeployment(clientset, cfg.Namespace, cfg.Deployment, cfg.Container)

	packager := &image.Packager{
		Base:     cfg.BaseImage,
		Repo:     cfg.Repo,
		DestPath: cfg.DestPath,
	}

	rolloutOpts := rollout.Options{Checks: healthChecks(cfg)}

	// A closure adapts rollout.Deploy to the narrow Deployer signature the
	// server wants, binding the target and options here rather than leaking
	// them into the HTTP layer.
	deployer := func(ctx context.Context, imageRef string) <-chan rollout.Event {
		return rollout.Deploy(ctx, target, imageRef, rolloutOpts)
	}

	return packager, deployer, nil
}

// healthChecks builds the gate from config. Returns nil when nothing is
// configured, which rollout treats as "settled is good enough".
func healthChecks(cfg config.Config) []health.Check {
	var checks []health.Check

	if cfg.HealthURL != "" {
		checks = append(checks, health.HTTPCheck{URL: cfg.HealthURL})
	}
	if cfg.HealthAddr != "" {
		checks = append(checks, health.TCPCheck{Addr: cfg.HealthAddr})
	}
	return checks
}
