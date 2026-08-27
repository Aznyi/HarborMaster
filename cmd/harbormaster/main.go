// Command harbormaster runs the HarborMaster API server and serves the
// embedded web interface.
//
// HarborMaster observes a Docker host and, when a deployment explicitly opts
// in, changes it: it inventories containers, snapshots their configuration,
// tracks images against their registries, plans changes, and can acquire an
// image, recreate a container onto it, verify the result, and roll it back.
//
// Every one of those mutations is off by default and reaches Docker through a
// separate capability interface held by exactly one service. The read-only
// runtime interface every other service holds cannot change anything, and it
// never executes a command inside a container.
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

	// The IANA timezone database, embedded in the binary.
	//
	// A maintenance window carries an IANA zone and every comparison is made in
	// it. The runtime image is distroless and carries no system zoneinfo, so
	// without this import every named zone would fail to load, every window
	// would correctly-but-uselessly fail closed, and automation would never act
	// on any deployment that named one.
	//
	// Roughly 450 KB of binary, paid once, for a subsystem whose safety
	// property depends on getting "02:00 in Europe/London" right twice a year.
	_ "time/tzdata"

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

	announceListenerExposure(logger, cfg, runningInContainer())

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

	// Configuration snapshots.
	//
	// The key is resolved BEFORE anything can capture. Failing here is
	// deliberate and total: a snapshot written under the wrong key produces
	// digests that compare unequal against all history, which reads as "every
	// secret changed at once" -- a false alarm indistinguishable from a breach.
	// Refusing to start is loud, recoverable, and honest.
	//
	// It is resolved before the DOCKER CLIENT because the client's masker needs
	// the digester built from it. A sensitive value can only be digested while
	// it is in memory, which is the moment the masker classifies it; a client
	// constructed before the key existed would classify every value with no
	// evidence attached and there is no later point to add it.
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

	dockerClient, err := docker.New(docker.Options{
		Host:    cfg.Docker.Host,
		Timeout: cfg.Docker.Timeout,
		// Empty in every normal deployment: the client negotiates. A pin
		// exists for a daemon whose negotiation misbehaves, and for the
		// compatibility matrix in CI.
		APIVersion: cfg.Docker.APIVersion,
		// Built from configuration so an operator can extend the patterns, and
		// applied inside the adapter so values are masked at the boundary
		// rather than somewhere further out.
		//
		// The digester travels with it. Masking and digesting are the same
		// decision made at the same instant -- "this is sensitive, so hide the
		// value and record evidence about it" -- and splitting them is what let
		// the evidence be computed after the value was gone. The adapter
		// receives a function, never the key.
		Masker: cfg.Masker().WithDigester(hasher.DigestValue),
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
		DB:     db,
		Docker: dockerClient,
		Events: events,
		// Which capabilities this deployment has, served only to an
		// authenticated caller.
		//
		// Booleans, never values. An operator looking at an empty page needs to
		// tell "switched off" from "not working", and nothing else here needs
		// to leave the process.
		Features: domain.Features{
			Inventory:                 cfg.Inventory.Enabled,
			Events:                    cfg.Events.Enabled,
			Snapshots:                 cfg.Snapshots.Enabled,
			Drift:                     cfg.Drift.Enabled,
			Policy:                    cfg.Policy.Enabled,
			Planner:                   cfg.Planner.Enabled,
			ImageIntel:                cfg.ImageIntel.Enabled,
			Acquisition:               cfg.Acquisition.Enabled,
			Execution:                 cfg.Execution.Enabled,
			Rollback:                  cfg.Rollback.Enabled,
			Automation:                cfg.Automation.Enabled,
			Notifications:             cfg.Notifications.Enabled,
			NotificationsAllowPrivate: cfg.Notifications.AllowPrivateDestinations,
		},
		Logger:    logger,
		StartedAt: startedAt,
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

	// One recorder, used by both the capture path and the readiness endpoint,
	// so a verdict is persisted whichever produced it.
	readinessRecorder := service.NewReadinessRecorder(readiness,
		service.StoreReadinessSections{Snapshots: db.Snapshots}, logger)

	snapshots := service.NewSnapshotService(service.SnapshotOptions{
		Containers: db.Containers,
		Snapshots:  db.Snapshots,
		Inventory:  db.Inventory,
		Hasher:     hasher,
		Versions: service.HostVersions{
			HarborMaster: build.Version,
			DockerAPI:    dockerAPIVersion,
		},
		Config:    cfg.Snapshots,
		Logger:    logger,
		Readiness: readinessRecorder,
	})

	diffs := service.NewDiffEngine(cfg.Snapshots)
	retention := service.NewRetentionService(db.Snapshots, cfg.Snapshots, logger)

	// Which container HarborMaster is running in.
	//
	// # This is the thing that stops HarborMaster updating itself
	//
	// A self-update is uniquely broken: the process performing the recreation
	// is inside the container being recreated, so it is killed partway through
	// and the record of what it was doing is never written. HarborMaster
	// refuses at four independent layers, and every one of them consults this.
	//
	// Resolved from independent signals -- a configured id, /proc, the
	// hostname, its own label -- none of which is required and any one of which
	// suffices. It reads no Docker socket: the identity comes from this
	// process's own view of itself and is resolved against the inventory
	// HarborMaster already holds.
	//
	// A failure to identify is a RESULT, not an error. The zero identity
	// matches nothing, which means a deployment where every probe fails
	// excludes nothing -- so the four refusals are what make this safe, not
	// this alone.
	self := service.NewSelfIdentifier(service.SelfIdentifierOptions{
		Containers:   db.Containers,
		ConfiguredID: cfg.Self.ContainerID,
		Logger:       logger,
	})

	// Operator notifications.
	//
	// HarborMaster's SECOND outbound egress, after image intelligence. Wired
	// before every service that raises one, and off unless the deployment
	// asked: with notifications disabled the Notifier below is nil, every
	// `raise` in the service layer becomes a nil check, and nothing leaves this
	// host.
	//
	// The engine is constructed either way, because the delivery history and
	// the destination list stay READABLE when sending is off -- an operator
	// configures and reviews before switching it on, which is the order those
	// should happen in.
	notifications, err := service.WireNotifications(db.Notifications, cfg.Notifications,
		build.Version, logger)
	if err != nil {
		return err
	}
	if cfg.Notifications.Enabled && cfg.Notifications.AllowPrivateDestinations {
		logger.Warn("notification destinations may resolve to private addresses; " +
			"an administrator who can create a destination can make HarborMaster " +
			"issue an HTTPS request to anything this container can route to")
	}

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
		Notify: notifications.Notifier,
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
		Notify:      notifications.Notifier,
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
		// The tracking references of managed containers. Without this a
		// container HarborMaster has updated declares only an immutable digest
		// and is never checked against a registry again.
		Lineage:    db.Lineage,
		Reconciler: service.NewLineageReconciler(db.Lineage, logger, nil),
		Store:      db.ImageIntel,
		Registry: registry.New(registry.Options{
			Version:        build.Version,
			RequestTimeout: cfg.ImageIntel.RequestTimeout,
			MaxAttempts:    cfg.ImageIntel.MaxAttempts,
			RetryBackoff:   cfg.ImageIntel.RetryBackoff,
		}),
		Config: cfg.ImageIntel,
		Notify: notifications.Notifier,
		Logger: logger,
	})

	// Snapshot assurance.
	//
	// # Why this is constructed here, above the planner
	//
	// A change plan records the configuration snapshot it was assessed against,
	// and a plan is immutable. So the only moment a baseline can enter a plan is
	// BEFORE the plan is written -- which makes the start of a planner pass the
	// one correct place to capture the ones that are missing.
	//
	// Capturing later, at acquisition or execution time, would produce evidence
	// the plan does not contain, and reading it into that plan would mean the
	// assessment an operator reviewed was not the assessment that authorised the
	// change. That is the load-bearing invariant of Phase 17.2.
	//
	// # It holds no Docker capability
	//
	// Assurance reaches one method on the snapshot service, which reads
	// HarborMaster's own container repository and never a socket. There is
	// nowhere on either options struct to wire a Docker interface, and two
	// architecture tests hold that.
	assurance := service.NewSnapshotAssurance(service.SnapshotAssuranceOptions{
		Capturer: snapshots,
		Logger:   logger,
	})

	// The bulk half: baselines for containers an update policy governs but that
	// have never been snapshotted. Bounded per pass, four queries, and free on a
	// deployment with no policies -- it returns before reading the estate.
	snapshotPreparer := service.NewSnapshotPreparer(service.SnapshotPreparerOptions{
		Assurance: assurance,
		Policies:  db.UpdatePolicies,
		Targets:   db.Containers,
		Baselines: db.Snapshots,
		Self:      self,
		Logger:    logger,
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
		Lineage: db.Lineage,
		Store:   db.Plans,
		// Baselines first, so a plan written moments later CONTAINS the
		// snapshot evidence rather than needing a second pass to notice it.
		Prepare: snapshotPreparer,
		Config:  cfg.Planner,
		Notify:  notifications.Notifier,
		Logger:  logger,
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
		Self:     self,
		// The OUTCOME of a download reaches the security audit log attributed
		// to the account that asked for it, not just the request.
		Audit:  auditRecorder,
		Notify: notifications.Notifier,
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
	// Workload dependencies: what must be stable before something else changes.
	//
	// A READER. It holds no Docker capability -- DependencyOptions has nowhere
	// to put one -- and it submits nothing. Its whole output is evidence, handed
	// to components that already own their capability and re-run their own
	// preflight.
	//
	// Wired UNCONDITIONALLY, and that is load-bearing rather than convenient.
	// The execution service fails closed without it: "HarborMaster cannot
	// establish what shares this container's namespace" and "nothing shares this
	// container's namespace" are opposite facts, and only the second permits a
	// stop. TestCompositionRootSuppliesDependencyEvidence fails the build if this
	// stops being passed to the execution service below.
	dependencies := service.NewDependencyService(service.DependencyOptions{
		Store:   db.Dependencies,
		Lineage: service.NewDependencyLineage(db.Containers),
		Self:    self,
		Audit:   auditRecorder,
		Logger:  logger,
		// The coordinated-update record, the execution records it derives member
		// state from, and the change plans it produces for reattachments.
		//
		// All three are HarborMaster's own tables. Writing a plan causes nothing
		// to happen: acting on one goes through the existing acquisition and
		// execution services, which own their capabilities and re-run their own
		// preflights.
		Operations: db.DependencyOperations,
		Executions: service.NewDependencyExecutions(db.Executions),
		Plans:      db.Plans,
		// Somewhere to say one sentence, not a capability. Nil when
		// notifications are off, which is the default and changes nothing.
		Notifier: notifications.Notifier,
	})

	// Plan approval.
	//
	// # Constructed here, above the executor that consults it
	//
	// A plan whose recommendation is `manualReview` asks for a person. Until
	// Phase 17.7 nothing could record that the person had looked, so the
	// execution preflight refused it forever -- "the review has not happened",
	// with no way for it to happen.
	//
	// This records the second fact next to the planner's, and changes nothing
	// about the first: the plan keeps its score, its factors and its
	// recommendation exactly as written. It reaches a plan READER and an
	// approval table, holds no Docker capability, and cannot acquire or execute
	// anything -- architecture tests hold all three.
	planApprovals := service.NewPlanApprovalService(service.PlanApprovalOptions{
		Store:  db.PlanApprovals,
		Plans:  db.Plans,
		Audit:  auditRecorder,
		Logger: logger,
	})

	executions := service.NewExecutionService(service.ExecutionOptions{
		Lineage: db.Lineage,
		Store:   db.Executions,
		Evidence: service.NewExecutionEvidence(
			db.Acquisitions, db.Plans, db.Containers,
			db.Snapshots, db.Policies, db.Inventory, db.ImageIntel),
		Runtime:  dockerClient,
		Capturer: capturer,
		Mutator:  mutator,
		// The last safe point. Assurance runs here as a MEASUREMENT: if the
		// snapshot describing this container is not the one the plan was
		// assessed against, the plan is stale and the recreation is refused.
		// The new baseline is kept for the next planner pass; it does not
		// authorise the plan already in flight.
		Assurance: assurance,
		// Whether a person reviewed a plan that asks for one. Consulted ONLY
		// for a manualReview recommendation, and only to replace one refusal
		// with one permission: every other preflight below it still runs.
		Approvals: planApprovals,
		// The LAST line of the self-update defence, and the one that matters
		// most: this is the layer that actually stops a container.
		Self: self,
		// Invariant A. The live experiment established that STOPPING a
		// namespace provider is the moment its dependents break -- silently,
		// with no network and nothing logged -- so this is the layer that has to
		// establish they can be reattached before anything stops.
		Dependencies: dependencies,
		// The same installation key the snapshots use. Configuration
		// preservation compares sensitive values as keyed digests, and digests
		// produced under a different key are not comparable -- so sharing the
		// key is what makes the comparison mean anything.
		Hasher: hasher,
		// The OUTCOME of a recreation -- the most consequential thing
		// HarborMaster does -- reaches the security audit log attributed to the
		// account that asked for it.
		Audit:  auditRecorder,
		Notify: notifications.Notifier,
		Config: cfg.Execution,
		Logger: logger,
	})

	// Manual rollback.
	//
	// HARBORMASTER'S THIRD AND MOST NARROWLY SCOPED DOCKER CAPABILITY. It can
	// stop the container a recreation put in place and start the one it
	// replaced -- and nothing else.
	//
	// docker.ContainerRollbacker is handed over here and nowhere else. Its four
	// methods can stop, rename, and start a container by EXACT ID. It cannot
	// create and it cannot remove: a rollback that could delete would destroy
	// the failed replacement, which is the evidence of why the rollback was
	// needed.
	//
	// Off unless the deployment asked for it, and configuration validation
	// refuses to start with rollback on and recreation off. When disabled the
	// capability stays nil and the service refuses everything -- ABSENT rather
	// than merely unused.
	var rollbacker docker.ContainerRollbacker
	if cfg.Rollback.Enabled {
		rollbacker = dockerClient
	}
	rollbacks := service.NewRollbackService(service.RollbackOptions{
		Lineage:    db.Lineage,
		Store:      db.Rollbacks,
		Evidence:   service.NewRollbackEvidence(db.Executions, db.Inventory),
		Runtime:    dockerClient,
		Rollbacker: rollbacker,
		// The same installation key the snapshots and recreations use. The
		// rollback's preservation check compares the original against itself
		// across the operation, and digests produced under a different key are
		// not comparable.
		Hasher: hasher,
		Audit:  auditRecorder,
		Notify: notifications.Notifier,
		Config: cfg.Rollback,
		Logger: logger,
	})

	// Update policies.
	//
	// Administration only. Creating a rule records an intention; whether that
	// intention is ever acted on is the engine's business, and the engine
	// re-reads the policies from the database on every pass.
	//
	// Wired even when the ENGINE is off, deliberately: an operator must be able
	// to write and review their rules before switching automation on, which is
	// the order those two things should be done in.
	updatePolicies := service.NewUpdatePolicyService(service.UpdatePolicyOptions{
		Store:  db.UpdatePolicies,
		Audit:  auditRecorder,
		Limits: domain.DefaultUpdatePolicyLimits(),
		Logger: logger,
	})

	// Notification administration.
	//
	// Separate from the engine that delivers, and given the audit recorder
	// because every write here is an administrator pointing HarborMaster's
	// second outbound egress somewhere -- including the test send, which
	// produces a real request without changing a row.
	notificationAdmin := service.NewNotificationAdminService(service.NotificationAdminOptions{
		Store:  db.Notifications,
		Engine: notifications.Service,
		Audit:  auditRecorder,
		SMTP: domain.SMTPSettings{
			Host:     cfg.Notifications.SMTPHost,
			Port:     cfg.Notifications.SMTPPort,
			StartTLS: cfg.Notifications.SMTPStartTLS,
		},
		Limits: domain.DefaultNotificationLimits(),
	})

	// THE AUTOMATION ENGINE.
	//
	// The first subsystem that can cause this host to change with nobody
	// watching -- and note what is NOT handed to it here. There is no
	// dockerClient, no capability interface, and nowhere on AutomationOptions
	// to put one. Its whole ability to affect the host is the pipeline adapter
	// below, which forwards three request types to the three services that
	// already hold their own capabilities and run their own preflights.
	//
	// So automation is a CALLER. It submits exactly the requests an operator's
	// HTTP request submits, and every one of them is re-validated against the
	// live host at the moment it acts. Four architecture tests pin this.
	//
	// Off unless the deployment asked for it, and configuration validation
	// refuses to start with automation on and either recreation or rollback
	// off: automation submits recreations, and an unattended update that fails
	// verification must be able to put the container back.
	automation := service.NewAutomationService(service.AutomationOptions{
		Store:    db.Automation,
		Policies: db.UpdatePolicies,
		// The last two answer one question: which containers has HarborMaster
		// positively established as needing no update. Without them a container
		// with no change plan is indistinguishable from one that was never
		// assessed, and its dependents are held for ever -- see
		// domain.AssessNoUpdate.
		Evidence: service.NewAutomationEvidence(
			db.Containers, db.Plans, db.Acquisitions, db.Executions,
			db.Lineage, db.ImageIntel),
		Pipeline: service.NewAutomationPipeline(acquisitions, executions, rollbacks),
		// The ordering evidence a pass reads, and the coordinator the follower
		// advances. NOT a capability: it holds no Docker interface, and the
		// engine's whole ability to affect the host remains the pipeline above.
		Dependencies: dependencies,
		// The passes, the pauses, and the approvals reach the security audit
		// log. The MUTATIONS are audited by the services that perform them --
		// auditing them twice would make the host-change counter over-report
		// the one number an administrator most needs to trust.
		Audit:  auditRecorder,
		Notify: notifications.Notifier,
		// So a decision pass never proposes HarborMaster's own container. The
		// acquisition and execution preflights refuse independently; this is
		// what stops the request being made in the first place.
		Self:   self,
		Config: cfg.Automation,
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

	// The self identity is re-resolved after every committed refresh. The
	// container id does not change while the process runs, but the NAME and the
	// IMAGE can -- a `docker rename`, or an operator recreating HarborMaster
	// from their own compose file -- and a stale name would exclude the wrong
	// container.
	inventory.AddRefreshObserver(self)

	// Resolved ONCE at startup, before anything can decide anything.
	//
	// The observer above covers every later refresh, but the first inventory
	// refresh is asynchronous and an operator can run a manual automation pass
	// the moment the port opens. An identity that arrived a second too late
	// would be an identity that did not exclude anything on the one pass
	// somebody was watching.
	//
	// Bounded, and its failure is a result rather than an error: a deployment
	// where every probe fails carries the zero identity, which matches nothing.
	// The four refusals are what make that safe, not this.
	self.Resolve(ctx)

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

	// start runs one background service and accounts for it.
	//
	// The count used to be written by hand -- `background.Add(13)` above a list
	// of goroutines -- and by Stage 17.9 the list had fourteen entries. The
	// fourteenth Done() drove the counter negative and the process died with
	// `panic: sync: negative WaitGroup counter` on EVERY shutdown, which is how
	// live acceptance found it.
	//
	// The panic was the visible half. The dangerous half is that Wait() returns
	// as soon as the counter reaches zero, so awaitBackgroundServices could
	// return while a service was still running -- and the deferred db.Close()
	// below it would then close the handle underneath a final event flush,
	// which is exactly the ordering the comment above exists to guarantee.
	//
	// Adding as each goroutine starts makes the number unmaintained, so it
	// cannot be wrong.
	start := func(run func(context.Context)) {
		background.Add(1)
		go func() {
			defer background.Done()
			run(ctx)
		}()
	}

	start(inventory.Run)
	start(events.Run)
	start(retention.Run)
	start(drift.Run)
	start(policies.Run)
	start(imageIntel.Run)
	start(planner.Run)
	start(acquisitions.Run)
	start(executions.Run)
	start(rollbacks.Run)
	start(automation.Run)
	start(auth.Run)
	start(auditRecorder.Run)
	start(notifications.Service.Run)
	defer awaitBackgroundServices(logger, &background, shutdownGrace)

	// The startup integrity verdict, raised once the engine that can deliver it
	// is running.
	//
	// Only for a check that RAN and found damage. An incomplete check
	// establishes nothing -- invariant 5's converse -- and reporting one as a
	// failure would page an operator because a timeout expired.
	if report := db.OpenReport().Integrity; !report.OK && !report.Incomplete {
		service.NotifyIntegrityFailed(notifications.Notifier, report.Summary())
	}

	server := api.NewServer(api.Options{
		Health:       health,
		Inventory:    inventory,
		Containers:   db.Containers,
		Warnings:     db.Inventory,
		Lineage:      db.Lineage,
		Images:       db.Images,
		Networks:     db.Networks,
		Volumes:      db.Volumes,
		DockerEvents: db.DockerEvents,
		EventEngine:  events,

		Snapshots: db.Snapshots,
		Capture:   snapshots,
		Diffs:     diffs,
		Readiness: readinessRecorder,
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
		Rollbacks:    rollbacks,

		Automation:     automation,
		UpdatePolicies: updatePolicies,
		PlanApprovals:  planApprovals,
		// The same reader the execution service consults for invariant A. One
		// instance, so what the API shows and what the recreation path enforces
		// are derived from the same evidence.
		Dependencies: dependencies,
		// Both are non-nil even when sending is off: an administrator
		// configures destinations and reviews past deliveries before switching
		// delivery on, which is the order those should happen in.
		Notifications:     notifications.Service,
		NotificationAdmin: notificationAdmin,

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

// containerMarkers are files a container runtime leaves in a container's own
// filesystem, and that nothing outside one creates.
//
// Docker writes /.dockerenv; Podman and CRI-O write /run/.containerenv. Both
// are presence checks rather than content parsing: there is nothing to bound,
// nothing to misread, and no path that any caller supplies.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// runningInContainer reports whether this process is inside a container, as far
// as that can be established from within its own namespace.
//
// It answers false on any doubt, and the two failure directions are not worth
// the same. A false negative produces the blunt warning below, which is the
// behaviour this replaced and is safe. A false positive would soften a warning
// that was real, so the probes are limited to markers nothing outside a
// container creates -- no cgroup parsing, no hostname heuristics.
func runningInContainer() bool {
	for _, marker := range containerMarkers {
		// A fixed path from a package-level list. There is no caller-supplied
		// path here and nowhere to introduce one.
		if _, err := os.Stat(marker); err == nil { //nolint:gosec // fixed paths
			return true
		}
	}
	return false
}

// announceListenerExposure states what HarborMaster can establish about the
// reachability of its own HTTP port, and nothing beyond it.
//
// # Why a container is a different statement
//
// The listen address inside a container does not determine exposure. The image
// sets HARBORMASTER_HTTP_ADDR=0.0.0.0:8080 because it has to: a process bound
// to 127.0.0.1 inside a container is unreachable through a published port, so
// binding loopback there would mean nothing could reach it at all. What decides
// exposure is the publish specification on the HOST -- `-p 127.0.0.1:8080:8080`
// against `-p 8080:8080` -- or the network the container is attached to, and
// neither is visible from inside the namespace.
//
// Warning unconditionally therefore reported a finding that had not been made,
// on every containerised deployment including the supported Compose one, which
// sets that address explicitly. A warning that fires on every correct start is
// one operators learn to scroll past -- the same reasoning that keeps the
// bootstrap announcement at INFO.
//
// It is also the general rule stated in the architecture notes: a check that
// could not be PERFORMED establishes nothing, and nothing must not be reported
// as a failure. So the containerised case says which fact is missing and where
// the operator can read it, and the uncontainerised case -- where the bind
// address does settle the question -- still warns.
// The containerised fact is a parameter rather than a call inside, so the
// decision can be tested on every host without one of these tests depending on
// whether the machine running it happens to be in a container.
func announceListenerExposure(logger *slog.Logger, cfg config.Config, containerised bool) {
	if cfg.IsLoopback() {
		return
	}
	if containerised {
		logger.Info("http listener is bound to every interface inside this container, which is what makes a published port reachable at all; whether that port is reachable from an untrusted network is decided on the host and cannot be established from here",
			slog.String("addr", cfg.Server.Addr),
			slog.String("nextStep", "check the host's publish specification with `docker port`, and keep it on loopback or behind a TLS-terminating reverse proxy"))
		return
	}
	logger.Warn("http listener is not bound to loopback; HarborMaster fronts a privileged Docker socket, so ensure the port is not reachable from an untrusted network",
		slog.String("addr", cfg.Server.Addr))
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
