// Package config loads HarborMaster's runtime configuration from the
// environment.
//
// Two rules govern this package:
//
//   - Defaults are safe. The HTTP listener binds to loopback so that running
//     the bare binary never exposes the API to the network by accident. The
//     container image opts in to 0.0.0.0 explicitly.
//   - Values are never logged. Config is a plausible carrier for secrets in
//     later work (registry credentials, notification webhooks), so the
//     redacted String method is the only rendering this package offers.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

const envPrefix = "HARBORMASTER_"

// Default values, also documented in .env.example.
const (
	DefaultHTTPAddr          = "127.0.0.1:8080"
	DefaultMaxRequestBytes   = int64(1 << 20) // 1 MiB
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 15 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	DefaultShutdownTimeout   = 15 * time.Second
	DefaultDockerTimeout     = 10 * time.Second
	DefaultDBPath            = "./data/harbormaster.db"
	DefaultLogLevel          = "info"
	DefaultLogFormat         = "json"

	// Storage reliability defaults.
	//
	// DefaultDBBusyTimeout bounds cross-process lock contention. Five seconds
	// absorbs a checkpoint or a backup without turning a momentary overlap
	// into a failed write, and is short enough that a genuinely stuck writer
	// is reported rather than waited on forever.
	DefaultDBBusyTimeout      = 5 * time.Second
	DefaultDBIntegrityCheck   = IntegrityCheckQuick
	DefaultDBIntegrityTimeout = 30 * time.Second
	DefaultDBRequireWAL       = false

	// MinDBBusyTimeout rejects a value so small it would surface SQLITE_BUSY
	// on any normal overlap. Zero is not offered: "wait not at all" is a
	// configuration nobody wants and a support case waiting to happen.
	MinDBBusyTimeout = 100 * time.Millisecond
	// MaxDBBusyTimeout keeps a typo from making every write appear to hang.
	MaxDBBusyTimeout = 5 * time.Minute

	// Inventory defaults.
	//
	// DefaultRefreshInterval is a compromise: often enough that the UI is not
	// stale, rare enough that HarborMaster is not a load source on a host with
	// a thousand containers. Set the interval to 0 to disable periodic refresh
	// and drive it manually.
	DefaultInventoryEnabled = true
	DefaultRefreshOnStartup = true
	DefaultRefreshInterval  = 60 * time.Second
	DefaultInventoryWorkers = 8
	DefaultAbsentRetention  = 7 * 24 * time.Hour
	MinRefreshInterval      = 5 * time.Second
	MaxInventoryWorkers     = 64

	// Event engine defaults.
	//
	// DefaultEventReconcileInterval is deliberately much longer than
	// DefaultRefreshInterval. With live events driving targeted refreshes, a
	// full sweep is a safety net for what the stream missed rather than the
	// primary way the inventory stays current. See Config.ReconcileInterval for
	// how the two settings relate.
	DefaultEventsEnabled          = true
	DefaultEventReconnectInitial  = 1 * time.Second
	DefaultEventReconnectMax      = 60 * time.Second
	DefaultEventReconnectFactor   = 2.0
	DefaultEventBufferSize        = 1024
	DefaultEventBatchSize         = 64
	DefaultEventBatchFlush        = 500 * time.Millisecond
	DefaultEventDedupWindow       = 10 * time.Second
	DefaultEventRefreshDebounce   = 750 * time.Millisecond
	DefaultEventReconcileInterval = 15 * time.Minute
	DefaultEventRetentionAge      = 7 * 24 * time.Hour
	DefaultEventRetentionCount    = int64(50000)
	DefaultEventPruneInterval     = 1 * time.Hour
	DefaultEventStreamSubscribers = 16
	DefaultEventStreamBuffer      = 128
	DefaultEventStreamReplay      = 200
	DefaultEventStreamHeartbeat   = 20 * time.Second

	// Event engine bounds. The minimums exist for the same reason
	// MinRefreshInterval does: a tiny value would hammer a privileged socket or
	// spin a goroutine, and is far more likely to be a typo than an intention.
	MinEventReconnectDelay    = 100 * time.Millisecond
	MinEventDedupWindow       = 1 * time.Second
	MinEventReconcileInterval = 30 * time.Second
	MinEventPruneInterval     = 1 * time.Minute
	MinEventStreamHeartbeat   = 1 * time.Second
	MaxEventBufferSize        = 1 << 16
	MaxEventBatchSize         = 1024
	MaxEventStreamSubscribers = 256
	MaxEventStreamReplay      = 1000

	// Snapshot defaults.
	//
	// Snapshots are HarborMaster's own records: capture reads the inventory and
	// writes to the local database. Nothing here can change a container.
	DefaultSnapshotsEnabled = true

	// DefaultSnapshotMaxInventoryAge is how stale the inventory may be before a
	// readiness verdict degrades to at most "warning". Most readiness checks
	// answer from the inventory rather than the daemon, so a "ready" derived
	// from a six-hour-old reading states history as though it were fact.
	DefaultSnapshotMaxInventoryAge = 15 * time.Minute

	// Retention. The newest snapshot of a container is never pruned under any
	// policy: it is the restore baseline. Zero on either dimension disables it.
	DefaultSnapshotRetentionCount = 50
	DefaultSnapshotRetentionAge   = 90 * 24 * time.Hour
	DefaultSnapshotPruneInterval  = 1 * time.Hour
	DefaultSnapshotPruneBatch     = 200

	// Diff limits. A diff decodes two documents and cross-products two
	// configuration sets, and an unauthenticated caller can ask for one
	// repeatedly, so every dimension is bounded.
	DefaultSnapshotMaxConcurrentDiffs = 4
	DefaultSnapshotDiffTimeout        = 5 * time.Second
	DefaultSnapshotMaxDiffEntries     = 1000
	DefaultSnapshotMaxGroupEntries    = 5000

	// Write-endpoint limits. Per-process rather than per-client: there is no
	// authentication and therefore no trustworthy client identity to key on,
	// and a per-IP bucket would be trivially evaded while implying a precision
	// HarborMaster does not have.
	DefaultWriteRateLimit = 6.0 // requests per minute
	DefaultWriteRateBurst = 3

	// DefaultSnapshotMaxReasonBytes bounds the operator-supplied capture reason.
	DefaultSnapshotMaxReasonBytes = 500

	// Snapshot bounds.
	MaxSnapshotConcurrentDiffs = 64
	MaxSnapshotPruneBatch      = 10000
	MaxSnapshotReasonBytes     = 4096

	// Drift detection defaults.
	//
	// DefaultDriftSweepInterval is deliberately much longer than the event
	// debounce: with events driving evaluation, the sweep is a safety net for
	// what the stream missed rather than the primary mechanism. It is also the
	// most expensive periodic job HarborMaster runs, because it decodes one
	// snapshot document per container.
	DefaultDriftEnabled               = true
	DefaultDriftEvaluateOnEvents      = true
	DefaultDriftEvaluationDebounce    = 5 * time.Second
	DefaultDriftMaxPendingEvaluations = 256
	DefaultDriftSweepInterval         = 30 * time.Minute
	DefaultDriftSweepOnStartup        = true
	DefaultDriftEvaluationTimeout     = 10 * time.Second
	// DefaultDriftMaxRecordsPerContainer is generous: a container that really
	// differs in 500 fields has been rebuilt, and the operator needs to see
	// that rather than a truncated list.
	DefaultDriftMaxRecordsPerContainer = 500
	DefaultDriftRetentionAge           = 30 * 24 * time.Hour
	DefaultDriftPruneInterval          = 6 * time.Hour
	DefaultDriftMaxNoteBytes           = 500

	// Drift bounds. The minimums exist for the same reason the event engine's
	// do: a tiny value would spin a worker or hammer the database, and is far
	// more likely a typo than an intention.
	MinDriftEvaluationDebounce  = 100 * time.Millisecond
	MinDriftSweepInterval       = 1 * time.Minute
	MinDriftPruneInterval       = 1 * time.Minute
	MaxDriftPendingEvaluations  = 4096
	MaxDriftRecordsPerContainer = 5000
	MaxDriftNoteBytes           = 4096

	// Policy engine defaults.
	//
	// The sweep interval is shorter than drift's because a compliance pass is
	// much cheaper: it reads the container rows the inventory already holds and
	// applies rules in memory, where drift decodes one snapshot document per
	// container. It is also the pass an operator most expects to be current.
	DefaultPolicyEnabled               = true
	DefaultPolicyEvaluateOnEvents      = true
	DefaultPolicyEvaluationDebounce    = 5 * time.Second
	DefaultPolicyMaxPendingEvaluations = 256
	DefaultPolicySweepInterval         = 15 * time.Minute
	DefaultPolicySweepOnStartup        = true
	DefaultPolicyEvaluationTimeout     = 10 * time.Second
	// DefaultPolicyMaxPolicies bounds the active set the engine loads once per
	// sweep. Far above any real deployment; it exists so an unauthenticated
	// caller cannot make each pass arbitrarily expensive by creating policies.
	DefaultPolicyMaxPolicies = 200
	// DefaultPolicyMaxViolationsPerContainer bounds one container's failures.
	// Past it the pass is marked INCOMPLETE rather than truncated silently,
	// because an incomplete pass resolves nothing.
	DefaultPolicyMaxViolationsPerContainer = 500
	DefaultPolicyMaxRulesPerPolicy         = 32
	DefaultPolicyMaxValuesPerRule          = 32
	DefaultPolicyMaxNameBytes              = 120
	DefaultPolicyMaxDescriptionBytes       = 1000
	// The policy write endpoints get their OWN rate limit, deliberately more
	// permissive than the shared one. See config.Policy.WriteRateLimit for why.
	DefaultPolicyWriteRateLimit = 60.0 // requests per minute
	DefaultPolicyWriteRateBurst = 20

	DefaultPolicyRetentionAge  = 90 * 24 * time.Hour
	DefaultPolicyPruneInterval = 6 * time.Hour
	DefaultPolicyMaxNoteBytes  = 500

	// Policy bounds. The minimums exist for the same reason drift's do: a tiny
	// value would spin a worker or hammer the database, and is far more likely
	// a typo than an intention. The maximums bound what one evaluation can
	// cost, since a policy is administrator-supplied input to an
	// unauthenticated API.
	MinPolicyEvaluationDebounce     = 100 * time.Millisecond
	MinPolicySweepInterval          = 1 * time.Minute
	MinPolicyPruneInterval          = 1 * time.Minute
	MaxPolicyPendingEvaluations     = 4096
	MaxPolicyCount                  = 2000
	MaxPolicyViolationsPerContainer = 5000
	MaxPolicyRulesPerPolicy         = 64
	MaxPolicyValuesPerRule          = 128
	MaxPolicyNameBytes              = 512
	MaxPolicyDescriptionBytes       = 8192
	MaxPolicyNoteBytes              = 4096
	MaxPolicyWriteRateLimit         = 6000.0
	MaxPolicyWriteRateBurst         = 1000

	// DefaultHealthcheckTimeout bounds the `harbormaster healthcheck` probe.
	// It is deliberately short: a container health check that outlives the
	// orchestrator's own timeout is worse than useless, because the runtime
	// kills it and reports a failure regardless of the application's state.
	DefaultHealthcheckTimeout = 3 * time.Second

	unixDockerHost    = "unix:///var/run/docker.sock"
	windowsDockerHost = "npipe:////./pipe/docker_engine"
)

// Server holds HTTP listener settings.
type Server struct {
	Addr              string
	MaxRequestBytes   int64
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// Docker holds Docker Engine connection settings.
type Docker struct {
	// Host is the Engine endpoint, e.g. unix:///var/run/docker.sock.
	Host string
	// Timeout bounds a single Engine API call.
	Timeout time.Duration
}

// Store holds persistence settings.
type Store struct {
	// Path is the SQLite database file.
	Path string

	// BusyTimeout bounds how long a statement waits for another PROCESS's
	// write lock. Within HarborMaster the connection pool already serialises
	// writers, so this governs contention with a second instance, a backup
	// tool, or an operator's sqlite3 shell.
	BusyTimeout time.Duration

	// IntegrityCheck selects the validation performed at startup: off, quick,
	// or full. Quick is the default; see internal/store/integrity.go for why
	// the cheap check is the right one on the startup path.
	IntegrityCheck string
	// IntegrityTimeout bounds that check. Past it the result is reported as
	// incomplete and startup continues, because a slow disk must not become an
	// outage on a database that is probably fine.
	IntegrityTimeout time.Duration

	// RequireWAL turns "write-ahead logging could not be enabled" from a
	// warning into a refusal to start. Off by default: a rollback journal is
	// slower and less concurrent but still correct, and refusing to run on a
	// filesystem that cannot do WAL would be a harsh default.
	RequireWAL bool
}

// Inventory holds settings for the read-only inventory engine.
type Inventory struct {
	// Enabled turns the whole inventory engine on or off. When false no
	// refresh runs and the API serves whatever was last persisted.
	Enabled bool
	// RefreshOnStartup runs one refresh as soon as the server is up.
	RefreshOnStartup bool
	// RefreshInterval is the periodic refresh period. Zero disables periodic
	// refresh; manual refresh still works.
	RefreshInterval time.Duration
	// Workers bounds concurrent container inspections. The Docker socket is a
	// single daemon, so more is not automatically faster, and unbounded
	// concurrency against 1,000 containers would be hostile to the host.
	Workers int
	// MaskPatterns are the environment-variable name fragments treated as
	// secret-bearing.
	MaskPatterns []string
	// AbsentRetention is how long a container that has disappeared is kept in
	// the inventory before being purged. Zero keeps absent containers forever.
	AbsentRetention time.Duration
}

// Events holds settings for the read-only Docker event engine.
//
// The engine subscribes to the daemon's event stream and uses events as HINTS
// that inventory may have changed. It never writes container state from an
// event: it asks the inventory service to re-read the resource, so there stays
// exactly one normalization and persistence path.
type Events struct {
	// Enabled turns the whole event engine on or off. When false nothing
	// connects to the event stream, the SSE endpoint reports the feature
	// disabled, and periodic inventory refresh carries the inventory alone.
	Enabled bool

	// ReconnectInitial is the first backoff delay after a stream drop, and
	// ReconnectMax caps it. ReconnectFactor multiplies the delay after each
	// consecutive failure. Jitter is applied on top so a daemon restart does
	// not produce a synchronised reconnect storm across several HarborMasters.
	ReconnectInitial time.Duration
	ReconnectMax     time.Duration
	ReconnectFactor  float64

	// BufferSize bounds the in-process queue between the stream reader and the
	// event processor. The reader never blocks on it: when it is full the event
	// is dropped, counted, and a full reconciliation is requested, because a
	// blocked reader would silently stall the whole stream.
	BufferSize int
	// BatchSize and BatchFlush bound how many events are persisted in one
	// transaction and how long a partial batch waits.
	BatchSize  int
	BatchFlush time.Duration

	// DedupWindow is how long an event fingerprint is remembered in memory. A
	// genuinely repeated event with a different Docker timestamp has a
	// different fingerprint and is NOT suppressed.
	DedupWindow time.Duration

	// RefreshDebounce coalesces the burst of events one lifecycle transition
	// produces (die, stop, start, health_status) into a single refresh per
	// resource.
	RefreshDebounce time.Duration

	// ReconcileInterval is the periodic full-reconciliation period. See
	// Config.ReconcileInterval for how it interacts with the inventory's own
	// refresh interval.
	ReconcileInterval time.Duration

	// RetentionAge and RetentionCount bound stored event history. Zero on
	// either disables that dimension of pruning; both zero keeps everything,
	// which is a valid but unbounded choice.
	RetentionAge   time.Duration
	RetentionCount int64
	// PruneInterval is how often retention runs.
	PruneInterval time.Duration

	// StreamSubscribers caps concurrent SSE clients. StreamBuffer bounds each
	// subscriber's queue: a slow reader drops events rather than blocking event
	// processing for everyone else.
	StreamSubscribers int
	StreamBuffer      int
	// StreamReplay caps how many stored events a Last-Event-ID reconnect
	// replays. A client that fell far behind gets a truncation notice and
	// should reload the paginated history instead.
	StreamReplay int
	// StreamHeartbeat is the comment-frame interval that keeps an idle stream
	// alive through proxies.
	StreamHeartbeat time.Duration
}

// Snapshots holds settings for configuration capture, restore readiness,
// retention, and the diff engine.
//
// A snapshot is a read of the current configuration written to HarborMaster's
// own database. Nothing in this section grants any ability to change Docker.
type Snapshots struct {
	// Enabled turns capture and the snapshot endpoints on or off.
	Enabled bool

	// HMACKeyFile and HMACKey supply the key that derives secret digests.
	//
	// HMACKeyFile is preferred and is listed first because it is how Docker
	// Secrets and Kubernetes secret volumes present material, and it keeps the
	// key out of the process environment -- an environment variable is readable
	// through /proc/<pid>/environ and is routinely captured by process
	// inspectors and crash reporters. Empty on both means the key is generated
	// once under the data directory, which is a standalone convenience rather
	// than the recommended production path.
	HMACKeyFile string
	HMACKey     string

	// MaskMode is "default" or "all-sensitive".
	MaskMode string
	// MaskPatternsExtra is merged with the defaults. Additive, so it cannot
	// reduce protection.
	MaskPatternsExtra []string
	// MaskPatternsOverride REPLACES the defaults. The one masking setting that
	// can reduce protection, so it warns at startup.
	MaskPatternsOverride []string

	// MaxInventoryAge is how stale the inventory may be before readiness
	// degrades to at most "warning". Readiness never triggers a refresh: a read
	// endpoint that can drive privileged socket traffic is a DoS amplifier.
	MaxInventoryAge time.Duration

	// RetentionCount is the maximum snapshots kept PER CONTAINER and
	// RetentionAge the maximum age. Zero on either disables that dimension;
	// both zero keeps everything, which is valid but unbounded.
	RetentionCount int
	RetentionAge   time.Duration
	// PruneInterval is how often retention runs, PruneBatch how many rows one
	// transaction deletes. Bounded batches keep a large backlog from holding
	// the single SQLite writer for an unbounded time.
	PruneInterval time.Duration
	PruneBatch    int

	// MaxConcurrentDiffs bounds simultaneous diff computations process-wide.
	// Excess requests are refused with 429 rather than queued: a queue converts
	// a load spike into unbounded memory and latency.
	MaxConcurrentDiffs int
	// DiffTimeout bounds one diff; MaxDiffEntries caps returned changes across
	// all groups; MaxGroupEntries caps how much one group compares.
	DiffTimeout     time.Duration
	MaxDiffEntries  int
	MaxGroupEntries int

	// WriteRateLimit (requests per minute) and WriteRateBurst bound the POST
	// endpoints. Per-process, not per-client.
	WriteRateLimit float64
	WriteRateBurst int

	// MaxReasonBytes bounds the operator-supplied capture reason.
	MaxReasonBytes int
}

// Policy holds settings for the policy engine.
//
// A policy is an administrator-defined rule that a container's configuration is
// checked AGAINST. It is never applied, enforced, or pushed to Docker: the
// engine reads HarborMaster's own inventory, and nothing in this section grants
// any ability to change a container.
//
// The bounds here are security controls rather than tuning knobs. Policy
// definitions arrive from an unauthenticated API, so the number of policies,
// the rules in each, and the values in each rule all have to be capped, or a
// caller could make every compliance pass arbitrarily expensive.
type Policy struct {
	// Enabled turns the engine on or off. When false nothing is evaluated, the
	// endpoints report the feature disabled, and no violations are written.
	Enabled bool

	// EvaluateOnEvents schedules an evaluation when a targeted refresh of one
	// container commits. Off does not mean stale: a full pass still runs after
	// every successful inventory refresh and on the periodic sweep.
	EvaluateOnEvents bool
	// EvaluationDebounce coalesces the burst one lifecycle transition produces
	// into a single evaluation per container.
	EvaluationDebounce time.Duration
	// MaxPendingEvaluations caps the coalescing queue. Past it the engine
	// escalates to a full sweep, which covers every pending container and
	// costs less than tracking them individually.
	MaxPendingEvaluations int

	// SweepInterval is the periodic full pass, and the safety net for whatever
	// the refresh notifications missed. Zero disables it.
	SweepInterval time.Duration
	// SweepOnStartup evaluates the estate once at boot, so a HarborMaster that
	// was down while containers changed reports compliance without waiting for
	// the first interval.
	SweepOnStartup bool

	// EvaluationTimeout bounds one container's pass.
	EvaluationTimeout time.Duration
	// MaxPolicies bounds the active set one pass loads. Past it the remainder
	// are not evaluated and a warning is logged.
	MaxPolicies int
	// MaxViolationsPerContainer bounds how many failures one pass may record.
	// Past it the pass is marked INCOMPLETE rather than truncated silently,
	// because an incomplete pass has not established that the rules it never
	// applied now pass.
	MaxViolationsPerContainer int

	// The definition bounds. These are what the API validates a submitted
	// policy against, and what bounds the cost of evaluating one.
	MaxRulesPerPolicy   int
	MaxValuesPerRule    int
	MaxNameBytes        int
	MaxDescriptionBytes int

	// RetentionAge is how long a RESOLVED violation is kept. Open violations
	// are never pruned: an unreviewed failure does not become less true with
	// age. Zero keeps resolved history forever.
	RetentionAge time.Duration
	// PruneInterval is how often that retention runs.
	PruneInterval time.Duration
	// MaxNoteBytes bounds the operator's annotation on a status change.
	MaxNoteBytes int

	// WriteRateLimit (requests per minute) and WriteRateBurst bound the policy
	// write endpoints.
	//
	// Separate from the snapshot and refresh limiter, and more permissive,
	// because a rate limit should be proportional to what the request costs. A
	// snapshot capture or an inventory refresh drives a Docker sweep; a policy
	// write is one small transaction on HarborMaster's own table. Sharing one
	// bucket would mean either throttling the cheap operation absurdly -- an
	// operator building a five-rule policy set would hit 429 -- or
	// under-protecting the expensive one.
	//
	// POST /policy/evaluate shares this bucket: the request itself is cheap,
	// and what bounds the actual work is the coalescing queue behind it rather
	// than the rate limit.
	WriteRateLimit float64
	WriteRateBurst int
}

// Drift holds settings for configuration drift detection.
//
// Drift compares a container's CURRENT configuration against its baseline
// snapshot and records the differences. It reads HarborMaster's own inventory
// and its own snapshots; it never calls Docker, and nothing in this section
// grants any ability to change a container or to put one back.
//
// Distinct from Policy above: drift measures change from a baseline, a policy
// measures compliance with a rule.
type Drift struct {
	// Enabled turns detection on or off. When false nothing is evaluated, the
	// endpoints report the feature disabled, and no records are written.
	Enabled bool

	// EvaluateOnEvents schedules an evaluation when the event engine sees a
	// container change. Off does not mean stale: the periodic sweep still
	// runs, just at its own cadence.
	EvaluateOnEvents bool
	// EvaluationDebounce coalesces the burst one lifecycle transition produces
	// into a single evaluation per container.
	EvaluationDebounce time.Duration
	// MaxPendingEvaluations caps the coalescing queue. Past it the engine
	// escalates to a full sweep, which covers every pending container and
	// costs less than tracking them individually.
	MaxPendingEvaluations int

	// SweepInterval is the periodic full sweep, and the safety net for
	// whatever the event stream missed. Zero disables it.
	SweepInterval time.Duration
	// SweepOnStartup evaluates the estate once at boot, so a HarborMaster that
	// was down while containers changed reports drift without waiting for the
	// first interval.
	SweepOnStartup bool

	// EvaluationTimeout bounds one container's comparison.
	EvaluationTimeout time.Duration
	// MaxRecordsPerContainer bounds how many differences one evaluation may
	// record. Past it the comparison is marked INCOMPLETE rather than
	// truncated silently, because a truncated comparison has not established
	// that the fields it never reached still match.
	MaxRecordsPerContainer int

	// RetentionAge is how long a RESOLVED record is kept. Open records are
	// never pruned: an unreviewed difference does not become less true with
	// age. Zero keeps resolved history forever.
	RetentionAge time.Duration
	// PruneInterval is how often that retention runs.
	PruneInterval time.Duration
	// MaxNoteBytes bounds the operator's annotation on a status change.
	MaxNoteBytes int
}

// Healthcheck holds settings for the `harbormaster healthcheck` command.
type Healthcheck struct {
	// Timeout bounds the whole probe, connection included.
	Timeout time.Duration
}

// Log holds structured-logging settings.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is one of json, text.
	Format string
}

// Config is the fully resolved application configuration.
type Config struct {
	Server      Server
	Docker      Docker
	Store       Store
	Log         Log
	Healthcheck Healthcheck
	Inventory   Inventory
	Events      Events
	Snapshots   Snapshots
	Drift       Drift
	Policy      Policy
}

var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"json", "text"}
)

// The integrity-check vocabulary.
//
// Restated here rather than imported from internal/store so that configuration
// stays a leaf package: config is loaded before anything is opened, and making
// it depend on the persistence adapter would invert the layering for the sake
// of three string constants. A test asserts the two vocabularies agree.
const (
	IntegrityCheckOff   = "off"
	IntegrityCheckQuick = "quick"
	IntegrityCheckFull  = "full"
)

var validIntegrityChecks = []string{IntegrityCheckOff, IntegrityCheckQuick, IntegrityCheckFull}

// defaultMaskMode is the classification policy when none is configured.
const defaultMaskMode = domain.MaskModeDefault

// Load reads configuration from the process environment, applying defaults for
// anything unset and validating the result.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

// lookupFunc mirrors os.LookupEnv so tests can supply a fake environment
// without mutating the real one.
type lookupFunc func(string) (string, bool)

func load(lookup lookupFunc) (Config, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := Config{
		Server: Server{
			Addr: stringVar(lookup, "HTTP_ADDR", DefaultHTTPAddr),
		},
		Docker: Docker{
			Host: stringVar(lookup, "DOCKER_HOST", defaultDockerHost()),
		},
		Store: Store{
			Path:           stringVar(lookup, "DB_PATH", DefaultDBPath),
			IntegrityCheck: strings.ToLower(stringVar(lookup, "DB_INTEGRITY_CHECK", DefaultDBIntegrityCheck)),
		},
		Log: Log{
			Level:  strings.ToLower(stringVar(lookup, "LOG_LEVEL", DefaultLogLevel)),
			Format: strings.ToLower(stringVar(lookup, "LOG_FORMAT", DefaultLogFormat)),
		},
		Inventory: Inventory{
			MaskPatterns: listVar(lookup, "INVENTORY_MASK_PATTERNS", domain.DefaultMaskPatterns),
		},
		Snapshots: Snapshots{
			HMACKeyFile: stringVar(lookup, "SNAPSHOT_HMAC_KEY_FILE", ""),
			HMACKey:     stringVar(lookup, "SNAPSHOT_HMAC_KEY", ""),
			MaskMode:    strings.ToLower(stringVar(lookup, "MASK_MODE", string(defaultMaskMode))),
			// nil rather than the defaults: "unset" and "set to the defaults"
			// must stay distinguishable so Masker can tell an additive merge
			// from an override.
			MaskPatternsExtra:    listVar(lookup, "MASK_PATTERNS_EXTRA", nil),
			MaskPatternsOverride: listVar(lookup, "MASK_PATTERNS_OVERRIDE", nil),
		},
	}

	var err error

	cfg.Server.MaxRequestBytes, err = int64Var(lookup, "MAX_REQUEST_BYTES", DefaultMaxRequestBytes)
	collect(err)
	cfg.Server.ReadHeaderTimeout, err = durationVar(lookup, "READ_HEADER_TIMEOUT", DefaultReadHeaderTimeout)
	collect(err)
	cfg.Server.ReadTimeout, err = durationVar(lookup, "READ_TIMEOUT", DefaultReadTimeout)
	collect(err)
	cfg.Server.WriteTimeout, err = durationVar(lookup, "WRITE_TIMEOUT", DefaultWriteTimeout)
	collect(err)
	cfg.Server.IdleTimeout, err = durationVar(lookup, "IDLE_TIMEOUT", DefaultIdleTimeout)
	collect(err)
	cfg.Server.ShutdownTimeout, err = durationVar(lookup, "SHUTDOWN_TIMEOUT", DefaultShutdownTimeout)
	collect(err)
	cfg.Docker.Timeout, err = durationVar(lookup, "DOCKER_TIMEOUT", DefaultDockerTimeout)
	collect(err)
	cfg.Healthcheck.Timeout, err = durationVar(lookup, "HEALTHCHECK_TIMEOUT", DefaultHealthcheckTimeout)
	collect(err)

	cfg.Store.BusyTimeout, err = durationVar(lookup, "DB_BUSY_TIMEOUT", DefaultDBBusyTimeout)
	collect(err)
	cfg.Store.IntegrityTimeout, err = durationVar(lookup, "DB_INTEGRITY_TIMEOUT", DefaultDBIntegrityTimeout)
	collect(err)
	cfg.Store.RequireWAL, err = boolVar(lookup, "DB_REQUIRE_WAL", DefaultDBRequireWAL)
	collect(err)

	cfg.Inventory.Enabled, err = boolVar(lookup, "INVENTORY_ENABLED", DefaultInventoryEnabled)
	collect(err)
	cfg.Inventory.RefreshOnStartup, err = boolVar(lookup, "INVENTORY_REFRESH_ON_STARTUP", DefaultRefreshOnStartup)
	collect(err)
	cfg.Inventory.RefreshInterval, err = durationVar(lookup, "INVENTORY_REFRESH_INTERVAL", DefaultRefreshInterval)
	collect(err)
	cfg.Inventory.AbsentRetention, err = durationVar(lookup, "INVENTORY_ABSENT_RETENTION", DefaultAbsentRetention)
	collect(err)

	cfg.Inventory.Workers, err = intVar(lookup, "INVENTORY_WORKERS", DefaultInventoryWorkers)
	collect(err)

	cfg.Events.Enabled, err = boolVar(lookup, "EVENTS_ENABLED", DefaultEventsEnabled)
	collect(err)
	cfg.Events.ReconnectInitial, err = durationVar(lookup, "EVENTS_RECONNECT_INITIAL_DELAY", DefaultEventReconnectInitial)
	collect(err)
	cfg.Events.ReconnectMax, err = durationVar(lookup, "EVENTS_RECONNECT_MAX_DELAY", DefaultEventReconnectMax)
	collect(err)
	cfg.Events.ReconnectFactor, err = floatVar(lookup, "EVENTS_RECONNECT_MULTIPLIER", DefaultEventReconnectFactor)
	collect(err)
	cfg.Events.BatchFlush, err = durationVar(lookup, "EVENTS_BATCH_FLUSH_INTERVAL", DefaultEventBatchFlush)
	collect(err)
	cfg.Events.DedupWindow, err = durationVar(lookup, "EVENTS_DEDUP_WINDOW", DefaultEventDedupWindow)
	collect(err)
	cfg.Events.RefreshDebounce, err = durationVar(lookup, "EVENTS_REFRESH_DEBOUNCE", DefaultEventRefreshDebounce)
	collect(err)
	cfg.Events.ReconcileInterval, err = durationVar(lookup, "EVENTS_RECONCILE_INTERVAL", DefaultEventReconcileInterval)
	collect(err)
	cfg.Events.RetentionAge, err = durationVar(lookup, "EVENTS_RETENTION_AGE", DefaultEventRetentionAge)
	collect(err)
	cfg.Events.PruneInterval, err = durationVar(lookup, "EVENTS_PRUNE_INTERVAL", DefaultEventPruneInterval)
	collect(err)
	cfg.Events.StreamHeartbeat, err = durationVar(lookup, "EVENTS_STREAM_HEARTBEAT", DefaultEventStreamHeartbeat)
	collect(err)

	cfg.Events.RetentionCount, err = int64Var(lookup, "EVENTS_RETENTION_COUNT", DefaultEventRetentionCount)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"EVENTS_BUFFER_SIZE", DefaultEventBufferSize, &cfg.Events.BufferSize},
		{"EVENTS_BATCH_SIZE", DefaultEventBatchSize, &cfg.Events.BatchSize},
		{"EVENTS_STREAM_MAX_SUBSCRIBERS", DefaultEventStreamSubscribers, &cfg.Events.StreamSubscribers},
		{"EVENTS_STREAM_BUFFER_SIZE", DefaultEventStreamBuffer, &cfg.Events.StreamBuffer},
		{"EVENTS_STREAM_REPLAY_LIMIT", DefaultEventStreamReplay, &cfg.Events.StreamReplay},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	cfg.Snapshots.Enabled, err = boolVar(lookup, "SNAPSHOTS_ENABLED", DefaultSnapshotsEnabled)
	collect(err)
	cfg.Snapshots.MaxInventoryAge, err = durationVar(lookup,
		"SNAPSHOT_READINESS_MAX_INVENTORY_AGE", DefaultSnapshotMaxInventoryAge)
	collect(err)
	cfg.Snapshots.RetentionAge, err = durationVar(lookup, "SNAPSHOT_RETENTION_AGE", DefaultSnapshotRetentionAge)
	collect(err)
	cfg.Snapshots.PruneInterval, err = durationVar(lookup, "SNAPSHOT_PRUNE_INTERVAL", DefaultSnapshotPruneInterval)
	collect(err)
	cfg.Snapshots.DiffTimeout, err = durationVar(lookup, "SNAPSHOT_DIFF_TIMEOUT", DefaultSnapshotDiffTimeout)
	collect(err)
	cfg.Snapshots.WriteRateLimit, err = floatVar(lookup, "WRITE_RATE_LIMIT", DefaultWriteRateLimit)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"SNAPSHOT_RETENTION_COUNT", DefaultSnapshotRetentionCount, &cfg.Snapshots.RetentionCount},
		{"SNAPSHOT_PRUNE_BATCH", DefaultSnapshotPruneBatch, &cfg.Snapshots.PruneBatch},
		{"SNAPSHOT_MAX_CONCURRENT_DIFFS", DefaultSnapshotMaxConcurrentDiffs, &cfg.Snapshots.MaxConcurrentDiffs},
		{"SNAPSHOT_MAX_DIFF_ENTRIES", DefaultSnapshotMaxDiffEntries, &cfg.Snapshots.MaxDiffEntries},
		{"SNAPSHOT_MAX_GROUP_ENTRIES", DefaultSnapshotMaxGroupEntries, &cfg.Snapshots.MaxGroupEntries},
		{"SNAPSHOT_MAX_REASON_BYTES", DefaultSnapshotMaxReasonBytes, &cfg.Snapshots.MaxReasonBytes},
		{"WRITE_RATE_BURST", DefaultWriteRateBurst, &cfg.Snapshots.WriteRateBurst},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	cfg.Drift.Enabled, err = boolVar(lookup, "DRIFT_ENABLED", DefaultDriftEnabled)
	collect(err)
	cfg.Drift.EvaluateOnEvents, err = boolVar(lookup, "DRIFT_EVALUATE_ON_EVENTS", DefaultDriftEvaluateOnEvents)
	collect(err)
	cfg.Drift.SweepOnStartup, err = boolVar(lookup, "DRIFT_SWEEP_ON_STARTUP", DefaultDriftSweepOnStartup)
	collect(err)
	cfg.Drift.EvaluationDebounce, err = durationVar(lookup, "DRIFT_EVALUATION_DEBOUNCE", DefaultDriftEvaluationDebounce)
	collect(err)
	cfg.Drift.SweepInterval, err = durationVar(lookup, "DRIFT_SWEEP_INTERVAL", DefaultDriftSweepInterval)
	collect(err)
	cfg.Drift.EvaluationTimeout, err = durationVar(lookup, "DRIFT_EVALUATION_TIMEOUT", DefaultDriftEvaluationTimeout)
	collect(err)
	cfg.Drift.RetentionAge, err = durationVar(lookup, "DRIFT_RETENTION_AGE", DefaultDriftRetentionAge)
	collect(err)
	cfg.Drift.PruneInterval, err = durationVar(lookup, "DRIFT_PRUNE_INTERVAL", DefaultDriftPruneInterval)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"DRIFT_MAX_PENDING_EVALUATIONS", DefaultDriftMaxPendingEvaluations, &cfg.Drift.MaxPendingEvaluations},
		{"DRIFT_MAX_RECORDS_PER_CONTAINER", DefaultDriftMaxRecordsPerContainer, &cfg.Drift.MaxRecordsPerContainer},
		{"DRIFT_MAX_NOTE_BYTES", DefaultDriftMaxNoteBytes, &cfg.Drift.MaxNoteBytes},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	cfg.Policy.Enabled, err = boolVar(lookup, "POLICY_ENABLED", DefaultPolicyEnabled)
	collect(err)
	cfg.Policy.EvaluateOnEvents, err = boolVar(lookup, "POLICY_EVALUATE_ON_EVENTS", DefaultPolicyEvaluateOnEvents)
	collect(err)
	cfg.Policy.SweepOnStartup, err = boolVar(lookup, "POLICY_SWEEP_ON_STARTUP", DefaultPolicySweepOnStartup)
	collect(err)
	cfg.Policy.EvaluationDebounce, err = durationVar(lookup, "POLICY_EVALUATION_DEBOUNCE", DefaultPolicyEvaluationDebounce)
	collect(err)
	cfg.Policy.SweepInterval, err = durationVar(lookup, "POLICY_SWEEP_INTERVAL", DefaultPolicySweepInterval)
	collect(err)
	cfg.Policy.EvaluationTimeout, err = durationVar(lookup, "POLICY_EVALUATION_TIMEOUT", DefaultPolicyEvaluationTimeout)
	collect(err)
	cfg.Policy.RetentionAge, err = durationVar(lookup, "POLICY_RETENTION_AGE", DefaultPolicyRetentionAge)
	collect(err)
	cfg.Policy.PruneInterval, err = durationVar(lookup, "POLICY_PRUNE_INTERVAL", DefaultPolicyPruneInterval)
	collect(err)
	cfg.Policy.WriteRateLimit, err = floatVar(lookup, "POLICY_WRITE_RATE_LIMIT", DefaultPolicyWriteRateLimit)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"POLICY_MAX_PENDING_EVALUATIONS", DefaultPolicyMaxPendingEvaluations, &cfg.Policy.MaxPendingEvaluations},
		{"POLICY_MAX_POLICIES", DefaultPolicyMaxPolicies, &cfg.Policy.MaxPolicies},
		{"POLICY_MAX_VIOLATIONS_PER_CONTAINER", DefaultPolicyMaxViolationsPerContainer, &cfg.Policy.MaxViolationsPerContainer},
		{"POLICY_MAX_RULES_PER_POLICY", DefaultPolicyMaxRulesPerPolicy, &cfg.Policy.MaxRulesPerPolicy},
		{"POLICY_MAX_VALUES_PER_RULE", DefaultPolicyMaxValuesPerRule, &cfg.Policy.MaxValuesPerRule},
		{"POLICY_MAX_NAME_BYTES", DefaultPolicyMaxNameBytes, &cfg.Policy.MaxNameBytes},
		{"POLICY_MAX_DESCRIPTION_BYTES", DefaultPolicyMaxDescriptionBytes, &cfg.Policy.MaxDescriptionBytes},
		{"POLICY_MAX_NOTE_BYTES", DefaultPolicyMaxNoteBytes, &cfg.Policy.MaxNoteBytes},
		{"POLICY_WRITE_RATE_BURST", DefaultPolicyWriteRateBurst, &cfg.Policy.WriteRateBurst},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports whether the configuration is internally usable.
func (c Config) Validate() error {
	var errs []error

	if _, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		errs = append(errs, fmt.Errorf("%sHTTP_ADDR must be host:port", envPrefix))
	}
	if c.Server.MaxRequestBytes <= 0 {
		errs = append(errs, fmt.Errorf("%sMAX_REQUEST_BYTES must be positive", envPrefix))
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"READ_HEADER_TIMEOUT", c.Server.ReadHeaderTimeout},
		{"READ_TIMEOUT", c.Server.ReadTimeout},
		{"WRITE_TIMEOUT", c.Server.WriteTimeout},
		{"IDLE_TIMEOUT", c.Server.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", c.Server.ShutdownTimeout},
		{"DOCKER_TIMEOUT", c.Docker.Timeout},
		{"HEALTHCHECK_TIMEOUT", c.Healthcheck.Timeout},
	} {
		if d.value <= 0 {
			errs = append(errs, fmt.Errorf("%s%s must be positive", envPrefix, d.name))
		}
	}
	if strings.TrimSpace(c.Docker.Host) == "" {
		errs = append(errs, fmt.Errorf("%sDOCKER_HOST must not be empty", envPrefix))
	}
	if strings.TrimSpace(c.Store.Path) == "" {
		errs = append(errs, fmt.Errorf("%sDB_PATH must not be empty", envPrefix))
	}
	// An unrecognised mode is rejected rather than treated as "off". Silently
	// skipping the integrity check because of a typo is the failure this
	// setting exists to prevent.
	if !slices.Contains(validIntegrityChecks, c.Store.IntegrityCheck) {
		errs = append(errs, fmt.Errorf("%sDB_INTEGRITY_CHECK must be one of %s",
			envPrefix, strings.Join(validIntegrityChecks, ", ")))
	}
	if c.Store.BusyTimeout < MinDBBusyTimeout || c.Store.BusyTimeout > MaxDBBusyTimeout {
		errs = append(errs, fmt.Errorf("%sDB_BUSY_TIMEOUT must be between %s and %s",
			envPrefix, MinDBBusyTimeout, MaxDBBusyTimeout))
	}
	// Zero would mean an unbounded check on the startup path, which is exactly
	// the hang this timeout exists to prevent.
	if c.Store.IntegrityTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%sDB_INTEGRITY_TIMEOUT must be positive", envPrefix))
	}
	if !slices.Contains(validLogLevels, c.Log.Level) {
		errs = append(errs, fmt.Errorf("%sLOG_LEVEL must be one of %s", envPrefix, strings.Join(validLogLevels, ", ")))
	}
	if !slices.Contains(validLogFormats, c.Log.Format) {
		errs = append(errs, fmt.Errorf("%sLOG_FORMAT must be one of %s", envPrefix, strings.Join(validLogFormats, ", ")))
	}

	// A zero interval is a valid, documented way to disable periodic refresh.
	// A tiny non-zero one is almost always a mistake, and it would hammer a
	// privileged socket, so it is rejected rather than silently clamped.
	if c.Inventory.RefreshInterval != 0 && c.Inventory.RefreshInterval < MinRefreshInterval {
		errs = append(errs, fmt.Errorf("%sINVENTORY_REFRESH_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinRefreshInterval))
	}
	if c.Inventory.Workers < 1 || c.Inventory.Workers > MaxInventoryWorkers {
		errs = append(errs, fmt.Errorf("%sINVENTORY_WORKERS must be between 1 and %d",
			envPrefix, MaxInventoryWorkers))
	}
	if c.Inventory.AbsentRetention < 0 {
		errs = append(errs, fmt.Errorf("%sINVENTORY_ABSENT_RETENTION must not be negative", envPrefix))
	}

	errs = append(errs, c.Events.validate()...)
	errs = append(errs, c.Snapshots.validate()...)
	errs = append(errs, c.Drift.validate()...)
	errs = append(errs, c.Policy.validate()...)

	return errors.Join(errs...)
}

// validate checks the policy settings.
//
// Validated even when the engine is disabled, for the same reason the drift
// settings are: a configuration error that only surfaces the day someone flips
// the feature on is a worse failure than one caught at startup.
func (p Policy) validate() []error {
	var errs []error

	if p.EvaluationDebounce < MinPolicyEvaluationDebounce {
		errs = append(errs, fmt.Errorf("%sPOLICY_EVALUATION_DEBOUNCE must be at least %s",
			envPrefix, MinPolicyEvaluationDebounce))
	}
	// Zero is the documented way to disable the periodic sweep. A tiny
	// non-zero one would re-evaluate the estate continuously.
	if p.SweepInterval != 0 && p.SweepInterval < MinPolicySweepInterval {
		errs = append(errs, fmt.Errorf("%sPOLICY_SWEEP_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinPolicySweepInterval))
	}
	if p.EvaluationTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%sPOLICY_EVALUATION_TIMEOUT must be positive", envPrefix))
	}
	if p.PruneInterval < MinPolicyPruneInterval {
		errs = append(errs, fmt.Errorf("%sPOLICY_PRUNE_INTERVAL must be at least %s",
			envPrefix, MinPolicyPruneInterval))
	}
	// Zero disables resolved-violation pruning, which is valid but unbounded.
	// Negative is not a way to do anything.
	if p.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sPOLICY_RETENTION_AGE must not be negative", envPrefix))
	}
	// A non-positive rate would mean "no writes at all", which is a way to
	// disable the feature by accident rather than a way to configure it.
	if p.WriteRateLimit <= 0 || p.WriteRateLimit > MaxPolicyWriteRateLimit {
		errs = append(errs, fmt.Errorf("%sPOLICY_WRITE_RATE_LIMIT must be between 0 (exclusive) and %.0f",
			envPrefix, MaxPolicyWriteRateLimit))
	}

	for _, b := range []struct {
		name            string
		value, min, max int
	}{
		{"POLICY_MAX_PENDING_EVALUATIONS", p.MaxPendingEvaluations, 1, MaxPolicyPendingEvaluations},
		{"POLICY_MAX_POLICIES", p.MaxPolicies, 1, MaxPolicyCount},
		{"POLICY_MAX_VIOLATIONS_PER_CONTAINER", p.MaxViolationsPerContainer, 1, MaxPolicyViolationsPerContainer},
		{"POLICY_MAX_RULES_PER_POLICY", p.MaxRulesPerPolicy, 1, MaxPolicyRulesPerPolicy},
		{"POLICY_MAX_VALUES_PER_RULE", p.MaxValuesPerRule, 1, MaxPolicyValuesPerRule},
		{"POLICY_MAX_NAME_BYTES", p.MaxNameBytes, 1, MaxPolicyNameBytes},
		{"POLICY_MAX_DESCRIPTION_BYTES", p.MaxDescriptionBytes, 1, MaxPolicyDescriptionBytes},
		{"POLICY_MAX_NOTE_BYTES", p.MaxNoteBytes, 1, MaxPolicyNoteBytes},
		{"POLICY_WRITE_RATE_BURST", p.WriteRateBurst, 1, MaxPolicyWriteRateBurst},
	} {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	return errs
}

// validate checks the drift settings.
//
// Validated even when drift is disabled, for the same reason the event and
// snapshot settings are: a configuration error that only surfaces the day
// someone flips the feature on is a worse failure than one caught at startup.
func (d Drift) validate() []error {
	var errs []error

	if d.EvaluationDebounce < MinDriftEvaluationDebounce {
		errs = append(errs, fmt.Errorf("%sDRIFT_EVALUATION_DEBOUNCE must be at least %s",
			envPrefix, MinDriftEvaluationDebounce))
	}
	// Zero is the documented way to disable the periodic sweep. A tiny
	// non-zero one would re-read every container continuously.
	if d.SweepInterval != 0 && d.SweepInterval < MinDriftSweepInterval {
		errs = append(errs, fmt.Errorf("%sDRIFT_SWEEP_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinDriftSweepInterval))
	}
	if d.EvaluationTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%sDRIFT_EVALUATION_TIMEOUT must be positive", envPrefix))
	}
	if d.PruneInterval < MinDriftPruneInterval {
		errs = append(errs, fmt.Errorf("%sDRIFT_PRUNE_INTERVAL must be at least %s",
			envPrefix, MinDriftPruneInterval))
	}
	// Zero disables resolved-record pruning, which is valid but unbounded.
	// Negative is not a way to do anything.
	if d.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sDRIFT_RETENTION_AGE must not be negative", envPrefix))
	}

	for _, b := range []struct {
		name            string
		value, min, max int
	}{
		{"DRIFT_MAX_PENDING_EVALUATIONS", d.MaxPendingEvaluations, 1, MaxDriftPendingEvaluations},
		{"DRIFT_MAX_RECORDS_PER_CONTAINER", d.MaxRecordsPerContainer, 1, MaxDriftRecordsPerContainer},
		{"DRIFT_MAX_NOTE_BYTES", d.MaxNoteBytes, 1, MaxDriftNoteBytes},
	} {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	return errs
}

// validate checks the snapshot settings.
//
// Validated even when snapshots are disabled, for the same reason the event
// settings are: a configuration error that only surfaces the day someone flips
// the feature on is a worse failure than one caught at startup.
func (s Snapshots) validate() []error {
	var errs []error

	if !domain.ValidMaskMode(s.MaskMode) {
		errs = append(errs, fmt.Errorf("%sMASK_MODE must be one of default, all-sensitive", envPrefix))
	}

	// Zero is a documented "disabled" on both retention dimensions; negative is
	// never meaningful and is far more likely a typo than an intention.
	for _, v := range []struct {
		name  string
		value int
	}{
		{"SNAPSHOT_RETENTION_COUNT", s.RetentionCount},
	} {
		if v.value < 0 {
			errs = append(errs, fmt.Errorf("%s%s must not be negative", envPrefix, v.name))
		}
	}
	if s.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_RETENTION_AGE must not be negative", envPrefix))
	}

	// These bound real work and must be positive: a zero batch would never make
	// progress, and a zero concurrency ceiling would refuse every diff.
	if s.PruneBatch < 1 || s.PruneBatch > MaxSnapshotPruneBatch {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_PRUNE_BATCH must be between 1 and %d",
			envPrefix, MaxSnapshotPruneBatch))
	}
	if s.PruneInterval <= 0 {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_PRUNE_INTERVAL must be positive", envPrefix))
	}
	if s.MaxConcurrentDiffs < 1 || s.MaxConcurrentDiffs > MaxSnapshotConcurrentDiffs {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_MAX_CONCURRENT_DIFFS must be between 1 and %d",
			envPrefix, MaxSnapshotConcurrentDiffs))
	}
	// An unbounded diff is a denial-of-service surface on an unauthenticated
	// endpoint, so there is no "0 means unlimited" here.
	if s.DiffTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_DIFF_TIMEOUT must be positive", envPrefix))
	}
	if s.MaxDiffEntries < 1 {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_MAX_DIFF_ENTRIES must be positive", envPrefix))
	}
	if s.MaxGroupEntries < 1 {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_MAX_GROUP_ENTRIES must be positive", envPrefix))
	}
	if s.MaxReasonBytes < 1 || s.MaxReasonBytes > MaxSnapshotReasonBytes {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_MAX_REASON_BYTES must be between 1 and %d",
			envPrefix, MaxSnapshotReasonBytes))
	}
	if s.MaxInventoryAge <= 0 {
		errs = append(errs, fmt.Errorf("%sSNAPSHOT_READINESS_MAX_INVENTORY_AGE must be positive", envPrefix))
	}
	if s.WriteRateLimit <= 0 {
		errs = append(errs, fmt.Errorf("%sWRITE_RATE_LIMIT must be positive", envPrefix))
	}
	if s.WriteRateBurst < 1 {
		errs = append(errs, fmt.Errorf("%sWRITE_RATE_BURST must be at least 1", envPrefix))
	}

	// Both forms set at once is ambiguous: it is not obvious whether the
	// operator wanted the union or the replacement, and guessing either way
	// could silently reduce protection.
	if len(s.MaskPatternsExtra) > 0 && len(s.MaskPatternsOverride) > 0 {
		errs = append(errs, fmt.Errorf(
			"%sMASK_PATTERNS_EXTRA and %sMASK_PATTERNS_OVERRIDE are mutually exclusive",
			envPrefix, envPrefix))
	}

	return errs
}

// validate checks the event-engine settings.
//
// The settings are validated even when the engine is disabled. A configuration
// error that only surfaces the day someone flips EVENTS_ENABLED to true is a
// worse failure than one caught at startup.
func (e Events) validate() []error {
	var errs []error

	positiveDurations := []struct {
		name  string
		value time.Duration
		min   time.Duration
	}{
		{"EVENTS_RECONNECT_INITIAL_DELAY", e.ReconnectInitial, MinEventReconnectDelay},
		{"EVENTS_RECONNECT_MAX_DELAY", e.ReconnectMax, MinEventReconnectDelay},
		{"EVENTS_BATCH_FLUSH_INTERVAL", e.BatchFlush, time.Millisecond},
		{"EVENTS_DEDUP_WINDOW", e.DedupWindow, MinEventDedupWindow},
		{"EVENTS_REFRESH_DEBOUNCE", e.RefreshDebounce, time.Millisecond},
		{"EVENTS_RECONCILE_INTERVAL", e.ReconcileInterval, MinEventReconcileInterval},
		{"EVENTS_PRUNE_INTERVAL", e.PruneInterval, MinEventPruneInterval},
		{"EVENTS_STREAM_HEARTBEAT", e.StreamHeartbeat, MinEventStreamHeartbeat},
	}
	for _, d := range positiveDurations {
		if d.value < d.min {
			errs = append(errs, fmt.Errorf("%s%s must be at least %s", envPrefix, d.name, d.min))
		}
	}

	// A maximum below the initial delay would make the backoff shrink on the
	// first failure, which is the opposite of what backoff is for.
	if e.ReconnectMax > 0 && e.ReconnectInitial > 0 && e.ReconnectMax < e.ReconnectInitial {
		errs = append(errs, fmt.Errorf(
			"%sEVENTS_RECONNECT_MAX_DELAY must not be smaller than %sEVENTS_RECONNECT_INITIAL_DELAY",
			envPrefix, envPrefix))
	}
	// A multiplier of exactly 1 is a fixed-interval retry, which is allowed;
	// below 1 the delay would decay towards zero.
	if e.ReconnectFactor < 1 {
		errs = append(errs, fmt.Errorf("%sEVENTS_RECONNECT_MULTIPLIER must be at least 1", envPrefix))
	}

	bounded := []struct {
		name            string
		value, min, max int
	}{
		{"EVENTS_BUFFER_SIZE", e.BufferSize, 1, MaxEventBufferSize},
		{"EVENTS_BATCH_SIZE", e.BatchSize, 1, MaxEventBatchSize},
		{"EVENTS_STREAM_MAX_SUBSCRIBERS", e.StreamSubscribers, 0, MaxEventStreamSubscribers},
		{"EVENTS_STREAM_BUFFER_SIZE", e.StreamBuffer, 1, MaxEventBufferSize},
		{"EVENTS_STREAM_REPLAY_LIMIT", e.StreamReplay, 0, MaxEventStreamReplay},
	}
	for _, b := range bounded {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	// Zero is the documented way to disable a retention dimension. Negative is
	// not a way to do anything.
	if e.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sEVENTS_RETENTION_AGE must not be negative", envPrefix))
	}
	if e.RetentionCount < 0 {
		errs = append(errs, fmt.Errorf("%sEVENTS_RETENTION_COUNT must not be negative", envPrefix))
	}
	// Batching more than the queue holds cannot help and reads as a mistake.
	if e.BatchSize > e.BufferSize && e.BufferSize > 0 {
		errs = append(errs, fmt.Errorf("%sEVENTS_BATCH_SIZE must not exceed %sEVENTS_BUFFER_SIZE",
			envPrefix, envPrefix))
	}

	return errs
}

// ReconcileInterval reports the period of the ONE periodic full-inventory
// sweep, together with which component owns it.
//
// Exactly one full-refresh timer may run. Two would double the load on a
// privileged socket and make "when was the last sweep" ambiguous. So:
//
//   - Event engine enabled: the engine owns reconciliation at
//     EVENTS_RECONCILE_INTERVAL, and the inventory service's own ticker is
//     suppressed. Targeted, event-driven refreshes carry the inventory between
//     sweeps, so the sweep is a safety net rather than the main mechanism.
//   - Event engine disabled: nothing changes from Phase 2. The inventory
//     service keeps its own INVENTORY_REFRESH_INTERVAL ticker, including the
//     documented "0 disables it" behaviour.
//
// INVENTORY_REFRESH_INTERVAL is therefore retained, not removed: an existing
// configuration keeps working, and it still governs the no-events case.
func (c Config) ReconcileInterval() (interval time.Duration, ownedByEventEngine bool) {
	if c.Events.Enabled && c.Inventory.Enabled {
		return c.Events.ReconcileInterval, true
	}
	return c.Inventory.RefreshInterval, false
}

// String renders the configuration for logs WITHOUT any environment-variable
// values. It reports only which knobs exist, so an operator can confirm the
// process is configured at all without the log becoming a disclosure channel.
//
// Do not add value interpolation here.
func (c Config) String() string {
	return "config{redacted: server, docker, store, log, healthcheck, inventory, events, snapshots}"
}

// Masker builds the environment masker from the configured patterns.
//
// Precedence: an explicit override replaces the defaults, extras merge with
// them, and the legacy INVENTORY_MASK_PATTERNS setting is honoured when it was
// customised. The mode applies in every case.
//
// MaskPatternsOverride is the only path that can reduce protection, which is
// why it is the only one the loader warns about.
func (c Config) Masker() *domain.Masker {
	mode := domain.MaskMode(c.Snapshots.MaskMode)

	switch {
	case len(c.Snapshots.MaskPatternsOverride) > 0:
		return domain.NewMaskerWithMode(c.Snapshots.MaskPatternsOverride, mode)

	case len(c.Snapshots.MaskPatternsExtra) > 0:
		merged := make([]string, 0,
			len(c.Inventory.MaskPatterns)+len(c.Snapshots.MaskPatternsExtra))
		merged = append(merged, c.Inventory.MaskPatterns...)
		merged = append(merged, c.Snapshots.MaskPatternsExtra...)
		return domain.NewMaskerWithMode(merged, mode)

	default:
		return domain.NewMaskerWithMode(c.Inventory.MaskPatterns, mode)
	}
}

// MaskPatternsWereOverridden reports whether the operator replaced the default
// pattern list, so startup can warn. Reporting the fact, not the patterns.
func (c Config) MaskPatternsWereOverridden() bool {
	return len(c.Snapshots.MaskPatternsOverride) > 0
}

// IsLoopback reports whether the HTTP listener is bound to a loopback address.
// The server logs a warning when it is not, since HarborMaster fronts a
// privileged Docker socket.
func (c Config) IsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Server.Addr)
	if err != nil {
		return false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// HealthcheckURL returns the URL of this process's own health endpoint.
//
// It is derived from the listen address rather than configured separately, so
// the container health check cannot drift out of sync with the port the server
// actually binds. A wildcard bind is rewritten to loopback: the probe runs
// inside the same network namespace, and dialling 0.0.0.0 is not portable.
func (c Config) HealthcheckURL() string {
	host, port, err := net.SplitHostPort(c.Server.Addr)
	if err != nil || port == "" {
		return "http://127.0.0.1:8080" + healthPath
	}

	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	// net.JoinHostPort adds the brackets an IPv6 literal needs in a URL.
	return "http://" + net.JoinHostPort(host, port) + healthPath
}

// healthPath must match the route registered in internal/api.
const healthPath = "/api/v1/health"

func defaultDockerHost() string {
	if runtime.GOOS == "windows" {
		return windowsDockerHost
	}
	return unixDockerHost
}

func stringVar(lookup lookupFunc, name, fallback string) string {
	if v, ok := lookup(envPrefix + name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func int64Var(lookup lookupFunc, name string, fallback int64) (int64, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		// The offending value is deliberately omitted from the error.
		return 0, fmt.Errorf("%s%s must be an integer", envPrefix, name)
	}
	return v, nil
}

// intVar reads a value into a platform-width int.
//
// It parses straight to int instead of parsing an int64 and narrowing. On a
// 32-bit build the narrowing conversion would silently wrap an oversized value
// into a small, plausible one — 4294967304 becoming 8 workers — which then
// passes Validate and is never reported. Out of range is an error here.
func intVar(lookup lookupFunc, name string, fallback int) (int, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		// The offending value is deliberately omitted from the error.
		if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("%s%s is out of range for an integer", envPrefix, name)
		}
		return 0, fmt.Errorf("%s%s must be an integer", envPrefix, name)
	}
	return v, nil
}

func floatVar(lookup lookupFunc, name string, fallback float64) (float64, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("%s%s must be a number", envPrefix, name)
	}
	return v, nil
}

func boolVar(lookup lookupFunc, name string, fallback bool) (bool, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s%s must be a boolean such as true or false", envPrefix, name)
	}
	return v, nil
}

// listVar reads a comma-separated list, falling back to a default. Entries are
// trimmed and empties dropped, so "A,,B," yields [A B].
func listVar(lookup lookupFunc, name string, fallback []string) []string {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return append([]string(nil), fallback...)
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func durationVar(lookup lookupFunc, name string, fallback time.Duration) (time.Duration, error) {
	raw, ok := lookup(envPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s%s must be a duration such as 15s or 1m", envPrefix, name)
	}
	return v, nil
}
