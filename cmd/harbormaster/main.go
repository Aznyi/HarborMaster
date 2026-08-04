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
	"path/filepath"
	"sync"
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
		// Built from configuration so an operator can extend the patterns, and
		// applied inside the adapter so values are masked at the boundary
		// rather than somewhere further out.
		Masker: cfg.Masker(),
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := dockerClient.Close(); err != nil {
			logger.Debug("close docker client failed", slog.String("error", err.Error()))
		}
	}()

	dockerAPIVersion := probeDocker(ctx, logger, dockerClient, db)

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}

	// Exactly one component owns the periodic full sweep. When the event engine
	// runs it takes that job at its own reconciliation interval and the
	// inventory's ticker is suppressed, so two full-refresh timers can never
	// both be hammering a privileged socket.
	reconcileInterval, eventsOwnReconciliation := cfg.ReconcileInterval()
	logger.Info("periodic full refresh configured",
		slog.Duration("interval", reconcileInterval),
		slog.String("owner", refreshOwner(eventsOwnReconciliation)))

	inventory := service.NewInventoryService(service.InventoryOptions{
		Runtime:          dockerClient,
		Inventory:        db.Inventory,
		Containers:       db.Containers,
		Logger:           logger,
		Config:           cfg.Inventory,
		SuppressPeriodic: eventsOwnReconciliation,
	})

	events := service.NewEventService(service.EventOptions{
		Runtime:   dockerClient,
		Events:    db.DockerEvents,
		Inventory: inventory,
		Logger:    logger,
		Config:    cfg.Events,
	})

	health := service.NewHealthService(service.HealthOptions{
		DB:        db,
		Docker:    dockerClient,
		Events:    events,
		Logger:    logger,
		StartedAt: startedAt,
	})

	// Configuration snapshots.
	//
	// The key is resolved BEFORE anything can capture. Failing here is
	// deliberate and total: a snapshot written under the wrong key produces
	// digests that compare unequal against all history, which reads as "every
	// secret changed at once" -- a false alarm indistinguishable from a breach.
	// Refusing to start is loud, recoverable, and honest.
	snapshotKey, err := service.ResolveSnapshotKey(ctx, db.Snapshots, cfg.Snapshots,
		filepath.Join(filepath.Dir(cfg.Store.Path), "snapshot-hmac.key"))
	if err != nil {
		return fmt.Errorf("resolve snapshot signing key: %w", err)
	}
	// Key ID and source only. The key itself never reaches a log record.
	logger.Info("snapshot digest key ready",
		slog.String("keyId", snapshotKey.KeyID),
		slog.String("source", string(snapshotKey.Source)))
	if snapshotKey.PermissionsTooWide {
		logger.Warn("snapshot key file is readable beyond its owner",
			slog.String("mode", fmt.Sprintf("%#o", snapshotKey.ObservedMode)),
			slog.String("expected", "0600"))
	}
	if cfg.MaskPatternsWereOverridden() {
		logger.Warn("default secret-masking patterns were REPLACED by configuration",
			slog.Int("configuredPatterns", len(cfg.Snapshots.MaskPatternsOverride)),
			slog.String("effect", "variables matching only the defaults will no longer be masked"))
	}

	hasher := service.NewHasher(snapshotKey)

	snapshots := service.NewSnapshotService(service.SnapshotOptions{
		Containers: db.Containers,
		Snapshots:  db.Snapshots,
		Inventory:  db.Inventory,
		Hasher:     hasher,
		Versions: service.HostVersions{
			HarborMaster: build.Version,
			DockerAPI:    dockerAPIVersion,
		},
		Config: cfg.Snapshots,
		Logger: logger,
	})

	readiness := service.NewReadinessEngine(service.ReadinessOptions{
		Inventory: service.StoreReadinessInventory{
			Inventory: db.Inventory,
			Images:    db.Images,
			Networks:  db.Networks,
			Volumes:   db.Volumes,
		},
		Pinger: inventory,
		// The null provider: Phase 3 inspects no filesystem at all.
		Host:   service.NewNullHostValidation(),
		Config: cfg.Snapshots,
	})

	diffs := service.NewDiffEngine(cfg.Snapshots)
	retention := service.NewRetentionService(db.Snapshots, cfg.Snapshots, logger)

	// SHUTDOWN ORDER, and why it is what it is.
	//
	// Deferred calls unwind last-to-first, and the order below is deliberate:
	//
	//  1. serve() returns after the HTTP server has drained. In-flight requests
	//     and open SSE streams end first, so nothing is still reading.
	//  2. background.Wait() (deferred here, runs next) waits for the inventory
	//     loop and the event engine to exit. Both are cancelled by the same ctx
	//     the signal handler cancels, and both return only once every goroutine
	//     they own has stopped.
	//  3. dockerClient.Close() and db.Close() (deferred earlier in this
	//     function, so they run last) release the socket and the database.
	//
	// The database MUST close after the background services, or a final event
	// flush would write to a closed handle. That ordering is the reason this
	// wait group is deferred here rather than anywhere earlier.
	var background sync.WaitGroup
	background.Add(3)

	go func() {
		defer background.Done()
		inventory.Run(ctx)
	}()
	go func() {
		defer background.Done()
		events.Run(ctx)
	}()
	go func() {
		defer background.Done()
		retention.Run(ctx)
	}()
	defer background.Wait()

	server := api.NewServer(api.Options{
		Health:       health,
		Inventory:    inventory,
		Containers:   db.Containers,
		Warnings:     db.Inventory,
		Images:       db.Images,
		Networks:     db.Networks,
		Volumes:      db.Volumes,
		DockerEvents: db.DockerEvents,
		EventEngine:  events,

		Snapshots: db.Snapshots,
		Capture:   snapshots,
		Diffs:     diffs,
		Readiness: readiness,
		// Renders a container's CURRENT configuration for a diff against the
		// present. In memory only: a GET must not write a snapshot.
		SnapshotSpecBuilder: func(detail domain.ContainerDetail) domain.SnapshotSpec {
			return service.BuildSpec(detail, detail.Image, hasher)
		},

		Logger:         logger,
		Config:         cfg.Server,
		SnapshotConfig: cfg.Snapshots,
		Assets:         assets,
	})

	recordEvent(ctx, logger, db, domain.Event{
		Type:     domain.EventServerStarted,
		Severity: domain.SeverityInfo,
		Message:  "harbormaster started",
	})

	return serve(ctx, logger, db, server.HTTPServer(), cfg.Server.ShutdownTimeout)
}

// refreshOwner names the component that owns the periodic full sweep, for the
// startup log line.
func refreshOwner(eventEngine bool) string {
	if eventEngine {
		return "event engine reconciliation"
	}
	return "inventory refresh interval"
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
// probeDocker reports Engine reachability at startup and returns the observed
// API version, which snapshots record as provenance. An empty string means the
// daemon was unreachable; a snapshot taken later simply carries no version
// rather than blocking capture on a transient outage.
func probeDocker(ctx context.Context, logger *slog.Logger, client docker.Pinger, db *store.DB) string {
	info, err := client.Ping(ctx)
	if err != nil {
		logger.Warn("docker engine unreachable at startup; continuing in degraded mode",
			slog.String("error", err.Error()))
		recordEvent(ctx, logger, db, domain.Event{
			Type:     domain.EventDockerDisconnected,
			Severity: domain.SeverityWarning,
			Message:  docker.SanitizeError(err),
		})
		return ""
	}

	logger.Info("docker engine reachable",
		slog.String("apiVersion", info.APIVersion),
		slog.String("osType", info.OSType))
	recordEvent(ctx, logger, db, domain.Event{
		Type:     domain.EventDockerConnected,
		Severity: domain.SeverityInfo,
		Message:  "docker engine reachable",
	})
	return info.APIVersion
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
