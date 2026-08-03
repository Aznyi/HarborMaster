// Command harbormaster runs the HarborMaster API server and serves the
// embedded web interface.
//
// HarborMaster is read-only today: it inventories the Docker Engine's
// reachability and establishes the snapshot store. It never creates, updates,
// removes, or restarts a container, and it never executes a command.
//
// Usage:
//
//	harbormaster [serve]     Run the API server (the default)
//	harbormaster healthcheck Probe the local health endpoint and exit
//	harbormaster version     Print build metadata
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Aznyi/HarborMaster/internal/api"
	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/healthcheck"
	"github.com/Aznyi/HarborMaster/internal/logging"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
	"github.com/Aznyi/HarborMaster/internal/version"
	"github.com/Aznyi/HarborMaster/web"
)

// Exit codes for the top-level command. `healthcheck` returns its own codes,
// documented in internal/healthcheck.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// dispatch routes a command line to a subcommand.
//
// Bare `harbormaster` still serves, so the container ENTRYPOINT needs no
// argument. An unrecognised argument is a usage error rather than a silent
// fallback to serving: starting a server when the operator asked for something
// else is the wrong kind of surprise for a tool that fronts the Docker socket.
func dispatch(args []string) int {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}

	switch command {
	case "serve":
		if err := run(); err != nil {
			// The logger may not exist yet if config loading failed, so this
			// last resort goes straight to stderr.
			fmt.Fprintf(os.Stderr, "harbormaster: %v\n", err)
			return exitFailure
		}
		return exitOK

	case "healthcheck":
		return healthcheckCommand()

	case "version":
		build := version.Get()
		fmt.Printf("harbormaster %s (commit %s, built %s, %s, %s)\n",
			build.Version, build.Commit, build.BuildDate, build.GoVersion, build.Platform)
		return exitOK

	case "help", "-h", "--help":
		usage(os.Stdout)
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "harbormaster: unknown command %q\n\n", command)
		usage(os.Stderr)
		return exitUsage
	}
}

// healthcheckCommand probes this host's own health endpoint.
//
// It is what the image's HEALTHCHECK runs. The runtime image is distroless and
// has no shell, curl, or wget by design, so the binary has to be able to check
// itself. It opens no database and no Docker connection: a probe that depends
// on the things it is meant to report on would deadlock or lie.
func healthcheckCommand() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid configuration")
		return exitUsage
	}

	return healthcheck.Run(context.Background(), os.Stdout, healthcheck.Options{
		URL:     cfg.HealthcheckURL(),
		Timeout: cfg.Healthcheck.Timeout,
	})
}

func usage(w io.Writer) {
	fmt.Fprint(w, `HarborMaster - safety-first container lifecycle manager.

Usage:
  harbormaster [serve]     Run the API server and serve the web interface (default)
  harbormaster healthcheck Probe the local health endpoint; exit 0 when healthy or degraded
  harbormaster version     Print build metadata
  harbormaster help        Show this message

Configuration is read from the environment. See .env.example for the full list.
`)
}

func run() error {
	startedAt := time.Now().UTC()

	cfg, err := config.Load()
	if err != nil {
		// The error names offending variables but never their values.
		return fmt.Errorf("invalid configuration: %w", err)
	}

	logger := logging.New(os.Stdout, cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(logger)

	build := version.Get()
	logger.Info("starting harbormaster",
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("goVersion", build.GoVersion),
		slog.String("platform", build.Platform))

	if !cfg.IsLoopback() {
		logger.Warn("http listener is not bound to loopback; HarborMaster fronts a privileged Docker socket, so ensure the port is not reachable from an untrusted network")
	}

	// SIGINT/SIGTERM cancel this context, which unwinds the whole process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Store.Path)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close database failed", slog.String("error", err.Error()))
		}
	}()
	logger.Info("database ready")

	dockerClient, err := docker.New(docker.Options{
		Host:    cfg.Docker.Host,
		Timeout: cfg.Docker.Timeout,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := dockerClient.Close(); err != nil {
			logger.Debug("close docker client failed", slog.String("error", err.Error()))
		}
	}()

	probeDocker(ctx, logger, dockerClient, db)

	health := service.NewHealthService(service.HealthOptions{
		DB:        db,
		Docker:    dockerClient,
		Logger:    logger,
		StartedAt: startedAt,
	})

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}

	server := api.NewServer(api.Options{
		Health: health,
		Logger: logger,
		Config: cfg.Server,
		Assets: assets,
	})

	recordEvent(ctx, logger, db, domain.Event{
		Type:     domain.EventServerStarted,
		Severity: domain.SeverityInfo,
		Message:  "harbormaster started",
	})

	return serve(ctx, logger, db, server.HTTPServer(), cfg.Server.ShutdownTimeout)
}

// serve runs the HTTP server until ctx is cancelled, then drains in-flight
// requests within shutdownTimeout.
func serve(ctx context.Context, logger *slog.Logger, db *store.DB, httpServer *http.Server, shutdownTimeout time.Duration) error {
	errCh := make(chan error, 1)

	go func() {
		logger.Info("http server listening", slog.String("addr", httpServer.Addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received", slog.Duration("gracePeriod", shutdownTimeout))
	}

	// A fresh context: the signal already cancelled ctx, so reusing it would
	// abort the drain immediately.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	recordEvent(shutdownCtx, logger, db, domain.Event{
		Type:     domain.EventServerStopped,
		Severity: domain.SeverityInfo,
		Message:  "harbormaster stopping",
	})

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed; closing connections",
			slog.String("error", err.Error()))
		if closeErr := httpServer.Close(); closeErr != nil {
			return fmt.Errorf("force close http server: %w", closeErr)
		}
	}

	// Drain the goroutine so a late ListenAndServe error is not lost.
	if err := <-errCh; err != nil {
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

// probeDocker performs the startup reachability check.
//
// An unreachable socket is logged and recorded, not fatal: HarborMaster is
// useful in a degraded state, and the UI renders the disconnected condition.
func probeDocker(ctx context.Context, logger *slog.Logger, client docker.Pinger, db *store.DB) {
	info, err := client.Ping(ctx)
	if err != nil {
		logger.Warn("docker engine unreachable at startup; continuing in degraded mode",
			slog.String("error", err.Error()))
		recordEvent(ctx, logger, db, domain.Event{
			Type:     domain.EventDockerDisconnected,
			Severity: domain.SeverityWarning,
			Message:  docker.SanitizeError(err),
		})
		return
	}

	logger.Info("docker engine reachable",
		slog.String("apiVersion", info.APIVersion),
		slog.String("osType", info.OSType))
	recordEvent(ctx, logger, db, domain.Event{
		Type:     domain.EventDockerConnected,
		Severity: domain.SeverityInfo,
		Message:  "docker engine reachable",
	})
}

// recordEvent appends to the audit log. A failure to record must not take the
// server down, so it is logged and swallowed.
func recordEvent(ctx context.Context, logger *slog.Logger, db *store.DB, event domain.Event) {
	if _, err := db.Events.Append(ctx, event); err != nil {
		logger.Error("record event failed",
			slog.String("type", string(event.Type)),
			slog.String("error", err.Error()))
	}
}
