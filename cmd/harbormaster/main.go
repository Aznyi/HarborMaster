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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aznyi/HarborMaster/internal/api"
	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/diagnostics"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/healthcheck"
	"github.com/Aznyi/HarborMaster/internal/logging"
	"github.com/Aznyi/HarborMaster/internal/registry"
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

	case "diagnose":
		return diagnoseCommand(os.Stdout)

	case "backup":
		return backupCommand(os.Stdout, args[1:])

	case "admin":
		return adminCommand(os.Stdout, args[1:])

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

// diagnoseCommand inspects HarborMaster's storage and prints a report.
//
// It opens the database READ-ONLY and never contacts Docker, so it is safe to
// run against a live server, a stopped one, or a copy. It is a command rather
// than an endpoint on purpose: see the package comment in internal/diagnostics.
func diagnoseCommand(out io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		// The error names offending variables, never their values.
		fmt.Fprintf(os.Stderr, "diagnose: invalid configuration: %v\n", err)
		return diagnostics.ExitFailed
	}

	report := diagnostics.Diagnose(context.Background(), cfg)
	diagnostics.Render(out, report)
	return report.ExitCode()
}

// backupCommand writes a consistent, verified copy of the database.
//
// Exactly one argument, the destination. No flags: every additional knob on a
// backup command is another way to produce a backup that is not what the
// operator thought it was.
func backupCommand(out io.Writer, args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(os.Stderr, "backup: usage: harbormaster backup <destination>")
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: invalid configuration: %v\n", err)
		return diagnostics.ExitFailed
	}

	return diagnostics.RunBackup(context.Background(), out, cfg, args[0])
}

func usage(w io.Writer) {
	// Usage text to stdout. A write failure here means the stream is gone,
	// which is not something a help message can or should recover from.
	_, _ = fmt.Fprint(w, `HarborMaster - safety-first container lifecycle manager.

Usage:
  harbormaster [serve]      Run the API server and serve the web interface (default)
  harbormaster healthcheck  Probe the local health endpoint; exit 0 when healthy or degraded
  harbormaster diagnose     Inspect the database and report reliability findings
  harbormaster backup PATH  Write a consistent, verified copy of the database to PATH
  harbormaster admin ...    Claim the installation or recover an account (see admin help)
  harbormaster version      Print build metadata
  harbormaster help         Show this message

diagnose opens the database read-only and contacts no Docker daemon. backup
uses SQLite's VACUUM INTO, so it is consistent while the server is running, and
it verifies the copy before reporting success. admin writes directly to the
database and takes filesystem access as its authority. None of the three is
reachable over HTTP: they report host detail, or perform recovery, that an API
must never expose.

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

	db, err := openStore(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close database failed", slog.String("error", err.Error()))
		}
	}()
	logger.Info("database ready",
		slog.String("journalMode", db.OpenReport().JournalMode),
		slog.String("integrity", db.OpenReport().Integrity.Summary()))

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

	// One grace budget for every detached background task, taken from the
	// server's shutdown timeout. The orchestrator counts down a single
	// deadline for the whole process, so the HTTP drain and the background
	// drain must not each help themselves to their own.
	shutdownGrace := cfg.Server.ShutdownTimeout

	inventory := service.NewInventoryService(service.InventoryOptions{
		Runtime:          dockerClient,
		Inventory:        db.Inventory,
		Containers:       db.Containers,
		Logger:           logger,
		Config:           cfg.Inventory,
		SuppressPeriodic: eventsOwnReconciliation,
		ShutdownGrace:    shutdownGrace,
	})

	events := service.NewEventService(service.EventOptions{
		Runtime:       dockerClient,
		Events:        db.DockerEvents,
		Inventory:     inventory,
		Logger:        logger,
		Config:        cfg.Events,
		ShutdownGrace: shutdownGrace,
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

	// Configuration drift.
	//
	// The engine reuses the Phase 3 diff engine to compare each container
	// against its baseline snapshot; it reads the inventory HarborMaster has
	// already persisted and never calls Docker. It has no remediation path,
	// and the only mutation it exposes is an operator moving a record's status
	// on HarborMaster's own row.
	//
	// The spec builder is the same one the snapshot diff endpoint uses:
	// in-memory only, so evaluating drift cannot create a snapshot as a side
	// effect -- which would make every evaluation the new baseline and leave
	// drift permanently empty.
	drift := service.NewDriftService(service.DriftOptions{
		Snapshots:  db.Snapshots,
		Containers: db.Containers,
		Records:    db.Drift,
		Pruner:     db.Drift,
		Inventory:  db.Inventory,
		SpecBuilder: func(detail domain.ContainerDetail) domain.SnapshotSpec {
			return service.BuildSpec(detail, detail.Image, hasher)
		},
		Config: cfg.Drift,
		Logger: logger,
	})

	// The policy engine.
	//
	// A policy is an administrator-defined rule that a container's
	// configuration is CHECKED AGAINST. It is never applied, enforced, or
	// pushed to the daemon: the engine reads the inventory HarborMaster has
	// already persisted, applies rules in memory, and writes the failures to
	// its own tables.
	//
	// Independent of drift above, not layered on it. Drift measures change
	// from a baseline; a policy measures compliance with a rule. A container
	// can drift into a still-compliant configuration, and one that has never
	// moved can be non-compliant from the day it was created because the rule
	// arrived later.
	policies := service.NewPolicyService(service.PolicyOptions{
		Definitions: db.Policies,
		Containers:  db.Containers,
		Violations:  db.Policies,
		Pruner:      db.Policies,
		Inventory:   db.Inventory,
		Config:      cfg.Policy,
		Logger:      logger,
	})

	// The event engine schedules an evaluation once a TARGETED refresh has
	// COMMITTED, never on the raw event: evaluating before the inventory is
	// written would read data one generation stale. Drift and policy are
	// separate consumers of the same signal.
	events.AddEvaluationScheduler(drift)
	events.AddEvaluationScheduler(policies)

	// Image intelligence.
	//
	// The only component in HarborMaster that opens an outbound connection. It
	// resolves manifests and lists tags over HTTPS, anonymously, and records
	// whether a newer image exists. It cannot pull, push, delete, or prune
	// anything, and it holds no registry credentials.
	//
	// Registry destinations come ONLY from image references the inventory
	// already holds; there is no configuration setting and no API parameter that
	// supplies a host. See internal/registry for the layered SSRF defences.
	imageIntel := service.NewImageIntelService(service.ImageIntelOptions{
		Store: db.ImageIntel,
		Registry: registry.New(registry.Options{
			Version:        build.Version,
			RequestTimeout: cfg.ImageIntel.RequestTimeout,
			MaxAttempts:    cfg.ImageIntel.MaxAttempts,
			RetryBackoff:   cfg.ImageIntel.RetryBackoff,
		}),
		Config: cfg.ImageIntel,
		Logger: logger,
	})

	// Change planning.
	//
	// The synthesis layer: it combines the inventory, snapshots, restore
	// readiness, drift, policy compliance, and image intelligence into one
	// assessment per proposed image change.
	//
	// It reads six tables and writes one. There is no Docker call behind it, no
	// network request, and nothing that executes a plan -- a plan is analysis an
	// operator acts on with their own tooling.
	planner := service.NewPlannerService(service.PlannerOptions{
		Store:  db.Plans,
		Config: cfg.Planner,
		Logger: logger,
	})

	// Identity, authorization, and the security audit log.
	//
	// # Always on
	//
	// There is no setting that disables authentication. HarborMaster fronts a
	// root-equivalent socket and can now replace a running container; an
	// "auth off for convenience" switch is a switch that ends up on in
	// production. The only unauthenticated routes are the four listed in the
	// API's route table.
	//
	// # The key is the installation's, not a second one
	//
	// Session tokens and the bootstrap token are stored as keyed digests under
	// the SAME installation key the snapshots use, with the purpose strings in
	// internal/service providing the domain separation. A second key file would
	// be a second thing to back up, a second thing to lose, and a second way for
	// a restore to half-work.
	//
	// The consequence is worth stating plainly: replacing the key logs everyone
	// out. That is the correct behaviour -- a session digest computed under a
	// key that no longer exists cannot be verified, and honouring it anyway
	// would mean not verifying it.
	hasherArgon, err := service.NewPasswordHasher(service.ArgonParamsFrom(cfg.Auth))
	if err != nil {
		return fmt.Errorf("password hashing parameters: %w", err)
	}
	logger.Info("password hashing configured",
		slog.Int("memoryKiB", cfg.Auth.ArgonMemoryKiB),
		slog.Int("iterations", cfg.Auth.ArgonIterations),
		slog.Int("parallelism", cfg.Auth.ArgonParallelism))

	auditRecorder := service.NewAuditRecorder(db.Audit, cfg.Auth, logger, nil)

	auth := service.NewAuthService(service.AuthOptions{
		Store:  service.NewAuthStore(db.Users, db.Sessions, db.Audit),
		Audit:  auditRecorder,
		Key:    snapshotKey,
		Hasher: hasherArgon,
		Config: cfg.Auth,
		Logger: logger,
	})

	users := service.NewUserService(
		service.NewUserAdminStore(db.Users, db.Sessions),
		auditRecorder, hasherArgon, logger, nil)

	// An unclaimed installation prints a one-time token, and every restart
	// invalidates the previous one. Claiming therefore requires the ability to
	// read this process's log or its data directory, rather than merely being
	// the first to reach the port.
	announceBootstrap(ctx, logger, auth)

	// Safe image acquisition.
	//
	// HARBORMASTER'S ONLY DOCKER MUTATION, and the only place in this function
	// where a write capability is handed to anything. Every other service above
	// receives dockerClient through the read-only docker.Runtime interface;
	// this one receives it a second time through docker.ImageAcquirer, which
	// has exactly one method: pull a digest-pinned image.
	//
	// It does not update containers. A container keeps running the image it was
	// created from; an acquired image is another entry in the local store.
	//
	// Off unless the deployment asked for it. When disabled the acquirer stays
	// nil and the service refuses everything -- so the capability is absent
	// rather than merely unused.
	var acquirer docker.ImageAcquirer
	if cfg.Acquisition.Enabled {
		acquirer = dockerClient
	}
	acquisitions := service.NewAcquisitionService(service.AcquisitionOptions{
		Store:    db.Acquisitions,
		Evidence: service.NewPlanEvidence(db.Plans, db.ImageIntel, db.Containers),
		Runtime:  dockerClient,
		Acquirer: acquirer,
		// The OUTCOME of a download reaches the security audit log attributed
		// to the account that asked for it, not just the request.
		Audit:  auditRecorder,
		Config: cfg.Acquisition,
		Logger: logger,
	})

	// Manual container recreation.
	//
	// HARBORMASTER'S LARGEST PRIVILEGE. Acquisition above writes to the image
	// store, which affects nothing that is serving. This can stop a running
	// container and replace it.
	//
	// Two capabilities are handed over here and nowhere else in this function:
	// docker.ConfigCapturer, which reads a container's live configuration into
	// an opaque value, and docker.ContainerMutator, whose five methods are the
	// whole of HarborMaster's ability to change a container. Every other
	// service above holds dockerClient only through the read-only
	// docker.Runtime interface and therefore cannot reach either.
	//
	// Off unless the deployment asked for it, and configuration validation
	// refuses to start with recreation on and acquisition off. When disabled
	// both stay nil and the service refuses everything -- so the capability is
	// ABSENT rather than merely unused.
	var (
		capturer docker.ConfigCapturer
		mutator  docker.ContainerMutator
	)
	if cfg.Execution.Enabled {
		capturer = dockerClient
		mutator = dockerClient
	}
	executions := service.NewExecutionService(service.ExecutionOptions{
		Store: db.Executions,
		Evidence: service.NewExecutionEvidence(
			db.Acquisitions, db.Plans, db.Containers,
			db.Snapshots, db.Policies, db.Inventory, db.ImageIntel),
		Runtime:  dockerClient,
		Capturer: capturer,
		Mutator:  mutator,
		// The same installation key the snapshots use. Configuration
		// preservation compares sensitive values as keyed digests, and digests
		// produced under a different key are not comparable -- so sharing the
		// key is what makes the comparison mean anything.
		Hasher: hasher,
		// The OUTCOME of a recreation -- the most consequential thing
		// HarborMaster does -- reaches the security audit log attributed to the
		// account that asked for it.
		Audit:  auditRecorder,
		Config: cfg.Execution,
		Logger: logger,
	})

	// A plan rests on the inventory, so a committed refresh is the moment its
	// inputs may have moved. Cheap to trigger: every assessment is
	// fingerprinted, so a pass over an unchanged estate writes nothing.
	inventory.AddRefreshObserver(planner)

	// The reference set is re-projected after every successful refresh. That is
	// a query and a transaction, not a burst of registry traffic: collection
	// itself is rationed by the engine's own schedule.
	inventory.AddRefreshObserver(imageIntel)

	// A full compliance pass runs after every SUCCESSFUL INVENTORY REFRESH.
	// The observer fires once the refresh has committed, so a pass always
	// reads committed data -- and because every refresh path (startup,
	// periodic, manual, and the event engine's reconciliation) funnels through
	// that commit, one hook covers all four.
	inventory.AddRefreshObserver(policies)

	// SHUTDOWN ORDER, and why it is what it is.
	//
	// Deferred calls unwind last-to-first, and the order below is deliberate:
	//
	//  1. serve() returns after the HTTP server has drained. In-flight requests
	//     and open SSE streams end first, so nothing is still reading.
	//  2. awaitBackgroundServices (deferred here, runs next) waits for the
	//     inventory loop and the event engine to exit. Both are cancelled by
	//     the same ctx the signal handler cancels, and both return only once
	//     every goroutine they own has stopped.
	//  3. dockerClient.Close() and db.Close() (deferred earlier in this
	//     function, so they run last) release the socket and the database.
	//
	// The database MUST close after the background services, or a final event
	// flush would write to a closed handle. That ordering is the reason this
	// wait is deferred here rather than anywhere earlier.
	//
	// The wait is BOUNDED. An unbounded one makes the whole shutdown only as
	// fast as the slowest background task, and those tasks are allowed to run
	// for minutes: a reconciliation of a thousand containers against a daemon
	// that has stopped answering would hold the process open long past the
	// point the runtime gives up and sends SIGKILL -- which is a worse ending
	// than an orderly abandonment, because it happens at an arbitrary instant.
	var background sync.WaitGroup
	background.Add(11)

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
	go func() {
		defer background.Done()
		drift.Run(ctx)
	}()
	go func() {
		defer background.Done()
		policies.Run(ctx)
	}()
	go func() {
		defer background.Done()
		imageIntel.Run(ctx)
	}()
	go func() {
		defer background.Done()
		planner.Run(ctx)
	}()
	go func() {
		defer background.Done()
		acquisitions.Run(ctx)
	}()
	go func() {
		defer background.Done()
		executions.Run(ctx)
	}()
	go func() {
		defer background.Done()
		auth.Run(ctx)
	}()
	go func() {
		defer background.Done()
		auditRecorder.Run(ctx)
	}()
	defer awaitBackgroundServices(logger, &background, shutdownGrace)

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

		Drift:       db.Drift,
		DriftConfig: cfg.Drift,

		Policies:     db.Policies,
		PolicyEngine: policies,
		PolicyConfig: cfg.Policy,

		ImageIntel:     db.ImageIntel,
		ImageCollector: imageIntel,

		Plans:   db.Plans,
		Planner: planner,

		Acquisitions: acquisitions,
		Executions:   executions,

		Auth:       auth,
		Users:      users,
		Audit:      auditRecorder,
		AuthConfig: cfg.Auth,

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

// announceBootstrap mints and prints the one-time claim token when this
// installation has no administrator yet.
//
// # Why the token is printed rather than defaulted
//
// The alternative is a built-in default account, which is how appliances end up
// on the internet with admin/admin. There is no default account here: until
// somebody claims the installation, HarborMaster holds no credentials at all
// and every route except the four public ones answers 401.
//
// # Why it goes to the log
//
// The log is the one channel an operator already has for every deployment shape
// -- `docker logs`, a systemd journal, a compose console. Writing it to a file
// would require a writable path that may not exist; printing it to stdout only
// would lose it under a log collector.
//
// It is INFO, not WARN. An unclaimed installation is the expected state of a
// first start, and a warning that fires on every correct first run teaches
// operators to ignore warnings.
//
// A failure here is not fatal. The CLI (`harbormaster admin bootstrap`) can
// claim the installation from the host filesystem without a token, so a server
// that could not mint one is inconvenient rather than unusable.
func announceBootstrap(ctx context.Context, logger *slog.Logger, auth *service.AuthService) {
	status, err := auth.BootstrapStatus(ctx)
	if err != nil {
		logger.Error("could not determine whether this installation has been claimed",
			slog.String("error", err.Error()))
		return
	}
	if status.Completed {
		return
	}

	token, expiresAt, err := auth.IssueBootstrapToken(ctx)
	if err != nil {
		if errors.Is(err, service.ErrBootstrapClosed) {
			return
		}
		logger.Error("could not issue a bootstrap token; claim this installation with `harbormaster admin bootstrap`",
			slog.String("error", err.Error()))
		return
	}

	// The token is a credential, and this is the only place it is ever
	// disclosed: the database holds a keyed digest, and no API response
	// contains it. It is printed as its own block rather than as a structured
	// field so a JSON log line cannot bury it in a collector's detail pane.
	logger.Info("this installation has not been claimed; a one-time bootstrap token was issued",
		slog.Time("expiresAt", expiresAt),
		slog.String("nextStep", "open the web interface and create the first administrator"))
	fmt.Printf("\n"+
		"  ==========================================================\n"+
		"   HarborMaster bootstrap token (valid until %s)\n"+
		"\n"+
		"     %s\n"+
		"\n"+
		"   Use it once to create the first administrator. Restarting\n"+
		"   HarborMaster issues a new token and invalidates this one.\n"+
		"  ==========================================================\n\n",
		expiresAt.Format(time.RFC3339), token)
}

// openStore opens the database with the configured reliability settings and
// translates a classified failure into an operator-facing message.
//
// The remedy line is what makes the difference between "migrate database:
// database disk image is malformed" and an operator who knows, in the first
// sentence, that the answer is a restore rather than a restart loop.
func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (*store.DB, error) {
	db, err := store.OpenWithOptions(ctx, store.Options{
		Path:             cfg.Store.Path,
		BusyTimeout:      cfg.Store.BusyTimeout,
		Integrity:        store.IntegrityMode(cfg.Store.IntegrityCheck),
		IntegrityTimeout: cfg.Store.IntegrityTimeout,
		RequireWAL:       cfg.Store.RequireWAL,
		Logger:           logger,
	})
	if err == nil {
		return db, nil
	}

	// A schema this build does not understand is its own kind of failure, and
	// the wrong remedy (delete the database) is catastrophic, so it is named
	// explicitly rather than folded into the storage classification.
	switch {
	case errors.Is(err, store.ErrSchemaAhead),
		errors.Is(err, store.ErrMigrationChanged),
		errors.Is(err, store.ErrMigrationGap):
		return nil, fmt.Errorf("%w\n\tthe database was NOT modified; do not delete it", err)
	}

	// The fatal/non-fatal distinction is the difference between "wait for the
	// disk to free up and it will come back" and "this will never come back".
	// Saying so is what stops an operator putting a corrupt database into a
	// restart loop while their backup window closes.
	if kind := store.Classify(err); kind != store.FailureNone {
		return nil, fmt.Errorf("%w\n\t%s%s", err, store.Remedy(kind), restartAdvice(kind))
	}
	if errors.Is(err, store.ErrCorrupt) {
		return nil, fmt.Errorf("%w\n\t%s%s", err,
			store.Remedy(store.FailureCorrupt), restartAdvice(store.FailureCorrupt))
	}
	return nil, err
}

// restartAdvice tells the operator whether starting again could possibly help.
func restartAdvice(kind store.FailureKind) string {
	if store.IsFatal(kind) {
		return "\n\trestarting will not help; this condition does not clear on its own"
	}
	return "\n\tHarborMaster will start normally once this condition clears"
}

// refreshOwner names the component that owns the periodic full sweep, for the
// startup log line.
func refreshOwner(eventEngine bool) string {
	if eventEngine {
		return "event engine reconciliation"
	}
	return "inventory refresh interval"
}

// awaitBackgroundServices waits for the background services, within a bound.
//
// Each service already bounds its own internal work, so reaching the bound
// here means one of them is genuinely wedged -- most plausibly a database
// write against a volume that has stopped answering. Abandoning it is safe:
// the deferred db.Close that runs immediately after this rolls back any
// uncommitted transaction, which is the same outcome a SIGKILL would produce,
// minus the arbitrary timing.
//
// It is logged at error level rather than swallowed. A shutdown that silently
// abandons work teaches an operator nothing the next time it happens.
func awaitBackgroundServices(logger *slog.Logger, background *sync.WaitGroup, grace time.Duration) {
	if service.WaitGroupTimeout(background, grace) {
		logger.Info("background services stopped")
		return
	}
	logger.Error("background services did not stop within the shutdown grace period; abandoning them",
		slog.Duration("grace", grace),
		slog.String("effect", "any uncommitted database transaction is rolled back when the handle closes"))
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
