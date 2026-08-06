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
	"net/netip"
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

	// Image intelligence defaults.
	//
	// These are the numbers that decide how HarborMaster behaves as a CLIENT of
	// somebody else's registry, which is the one resource it does not own. They
	// are deliberately conservative: an update that arrives an hour late costs
	// nothing, and a client that hammers Docker Hub gets rate-limited for
	// everyone sharing the egress address.
	DefaultImageIntelEnabled          = true
	DefaultImageIntelCollectOnStartup = true
	// DefaultImageIntelRefreshInterval is how long a successful answer stays
	// fresh. Publishers do not ship several times an hour, and a 6-hour cadence
	// over a thousand images is a handful of requests a minute.
	DefaultImageIntelRefreshInterval = 6 * time.Hour
	// DefaultImageIntelCollectInterval is how often the due set is drained. Much
	// shorter than the refresh interval, because it processes a bounded BATCH:
	// the pair is what spreads a large estate over time instead of bursting.
	DefaultImageIntelCollectInterval = 5 * time.Minute
	// DefaultImageIntelMaxConcurrentRequests bounds simultaneous registry
	// requests across the whole process.
	DefaultImageIntelMaxConcurrentRequests = 4
	// DefaultImageIntelMaxReferencesPerPass bounds one batch.
	DefaultImageIntelMaxReferencesPerPass = 50
	// DefaultImageIntelMaxTrackedReferences bounds how many distinct references
	// are tracked at all.
	DefaultImageIntelMaxTrackedReferences = 10000
	// DefaultImageIntelMaxTagPages bounds a tag listing. Past it the result is
	// reported as INCOMPLETE rather than as "no update", because a listing that
	// stopped early has established nothing.
	DefaultImageIntelMaxTagPages    = 5
	DefaultImageIntelRequestTimeout = 15 * time.Second
	DefaultImageIntelMaxAttempts    = 3
	DefaultImageIntelRetryBackoff   = 500 * time.Millisecond
	// DefaultImageIntelFailureBackoff and its cap bound how quickly a failing
	// reference or host is retried.
	DefaultImageIntelFailureBackoff    = 15 * time.Minute
	DefaultImageIntelMaxFailureBackoff = 24 * time.Hour
	// DefaultImageIntelUnsupportedInterval is how often a reference that cannot
	// be looked up is reconsidered. Long, because the answer almost never
	// changes -- but not never, since a private repository can become public.
	DefaultImageIntelUnsupportedInterval = 24 * time.Hour
	DefaultImageIntelHistoryRetention    = 90 * 24 * time.Hour
	DefaultImageIntelPruneInterval       = 6 * time.Hour

	// Image intelligence bounds. The maximums on concurrency and batch size are
	// the ones that matter: they are what stops a misconfiguration from turning
	// HarborMaster into something a registry blocks.
	MinImageIntelRefreshInterval    = 5 * time.Minute
	MinImageIntelCollectInterval    = 30 * time.Second
	MinImageIntelPruneInterval      = 1 * time.Minute
	MaxImageIntelConcurrentRequests = 16
	MaxImageIntelReferencesPerPass  = 500
	MaxImageIntelTrackedReferences  = 100000
	MaxImageIntelTagPages           = 50
	MaxImageIntelAttempts           = 5

	// Change planner defaults.
	//
	// Planning is cheap and local: it reads six tables and writes one, makes no
	// network request, and touches no Docker socket. The bounds below exist to
	// keep a pass proportional to the estate rather than to protect anyone from
	// it.
	DefaultPlannerEnabled           = true
	DefaultPlannerGenerateOnStartup = true
	// DefaultPlannerInterval is the periodic pass. Short, because a pass over an
	// unchanged estate writes nothing -- the fingerprint means the common case
	// costs a handful of grouped queries and no rows.
	DefaultPlannerInterval = 15 * time.Minute
	// DefaultPlannerBatchSize is how many containers one batch assesses. Each
	// batch costs five grouped queries whatever its size, so this trades peak
	// memory against query count.
	DefaultPlannerBatchSize = 500
	// DefaultPlannerMaxContainers caps a whole pass, so a pathologically large
	// inventory produces bounded work.
	DefaultPlannerMaxContainers     = 20000
	DefaultPlannerGenerationTimeout = 5 * time.Minute
	// DefaultPlannerRetentionAge is how long a SUPERSEDED plan is kept. The
	// current plan for a container is never pruned.
	DefaultPlannerRetentionAge  = 90 * 24 * time.Hour
	DefaultPlannerPruneInterval = 6 * time.Hour

	// Image acquisition defaults.
	//
	// This is the one feature that can change the Docker host, so its default
	// is the conservative one: OFF. A deployment opts in deliberately, and the
	// bounds below exist to keep an opted-in deployment from being able to
	// saturate its own disk or a public registry.
	//
	// DefaultAcquisitionEnabled is false. Every other feature in HarborMaster
	// defaults to on because every other feature only reads.
	DefaultAcquisitionEnabled = false
	// DefaultAcquisitionRequireSnapshot keeps the restore-readiness gate on. An
	// operator acquiring an image is usually about to act on it with their own
	// tooling, and having a recorded configuration to refer back to is the
	// point of the gate.
	DefaultAcquisitionRequireSnapshot = true

	// DefaultAcquisitionMaxConcurrent bounds simultaneous transfers. Low,
	// because each one is a sustained download competing for the same disk and
	// the same uplink.
	DefaultAcquisitionMaxConcurrent = 2
	// DefaultAcquisitionMaxPerRegistry bounds simultaneous transfers against
	// ONE registry. Anonymous rate limits are shared by egress address, so a
	// host that hammers a public registry gets everything behind that address
	// throttled.
	DefaultAcquisitionMaxPerRegistry = 1

	// DefaultAcquisitionPullTimeout bounds one transfer. Generous, because a
	// multi-gigabyte image over a slow link is not a fault -- but bounded,
	// because a transfer that never finishes holds a slot forever.
	DefaultAcquisitionPullTimeout = 30 * time.Minute
	// DefaultAcquisitionRequestTTL is how long a queued request stays valid.
	// Past it the request is abandoned rather than started: the evidence behind
	// the approval has aged, and acting on it would be acting on a stale check.
	DefaultAcquisitionRequestTTL = 1 * time.Hour
	// DefaultAcquisitionRegistryFreshness is how recent a successful registry
	// lookup must be for its digest to be acted on.
	DefaultAcquisitionRegistryFreshness = 24 * time.Hour

	// DefaultAcquisitionMaxEvents bounds the audit trail per acquisition, which
	// is what stops a chatty pull turning one operator action into an unbounded
	// number of writes.
	DefaultAcquisitionMaxEvents     = 200
	DefaultAcquisitionSweepInterval = 1 * time.Minute
	DefaultAcquisitionPruneInterval = 6 * time.Hour
	// DefaultAcquisitionRetentionAge is how long a COMPLETED audit record is
	// kept. Long, because it is the evidence that an image was downloaded.
	DefaultAcquisitionRetentionAge = 180 * 24 * time.Hour

	// Acquisition bounds.
	MinAcquisitionPullTimeout   = 1 * time.Minute
	MinAcquisitionSweepInterval = 10 * time.Second
	MinAcquisitionRequestTTL    = 1 * time.Minute
	MaxAcquisitionConcurrent    = 8
	MaxAcquisitionEvents        = 2000

	// DefaultExecutionEnabled is false, and it is the most consequential
	// default in HarborMaster.
	//
	// Turning this on gives HarborMaster the ability to STOP AND REPLACE a
	// running container. Acquisition, the only other write, adds an entry to
	// the image store and changes nothing that is running; this changes
	// something that is serving. It is off until a deployment asks for it.
	DefaultExecutionEnabled = false

	// DefaultExecutionStartupTimeout bounds the wait for a replacement to
	// become healthy. Generous, because a startup probe with a start period is
	// normal and an application that takes two minutes to warm up is not
	// unhealthy.
	DefaultExecutionStartupTimeout = 5 * time.Minute
	// DefaultExecutionStabilityPeriod is how long a container with NO health
	// check must stay running to count as stable.
	//
	// Short by necessity and honest about what it proves: staying up for thirty
	// seconds establishes that the container did not crash on startup, and
	// nothing more. A container that declares a health check gets a real
	// verdict; this is the weaker evidence available when it does not.
	DefaultExecutionStabilityPeriod = 30 * time.Second
	// DefaultExecutionHealthPollInterval is how often the replacement is
	// re-inspected while waiting.
	DefaultExecutionHealthPollInterval = 2 * time.Second
	// DefaultExecutionStopTimeout is how long the ORIGINAL is given to exit on
	// its own before the daemon terminates it. Matches Docker's own default, so
	// a container tuned for `docker stop` behaves the same here.
	DefaultExecutionStopTimeout = 30 * time.Second

	// DefaultExecutionMaxConcurrent bounds simultaneous recreations.
	//
	// ONE. Deliberately not tunable upward far: a recreation stops something
	// that is serving, and doing several at once turns a contained failure into
	// a multi-container outage nobody chose.
	DefaultExecutionMaxConcurrent = 1

	// DefaultExecutionRequestTTL is how long a queued request stays valid.
	// Short, because the preflight evidence behind it ages: a request that has
	// waited an hour was approved against a host that may no longer look the
	// same.
	DefaultExecutionRequestTTL = 15 * time.Minute
	// DefaultExecutionAcquisitionFreshness is how recent the acquisition must
	// be. An old download does not establish that the image is still present.
	DefaultExecutionAcquisitionFreshness = 24 * time.Hour
	// DefaultExecutionInventoryFreshness is how recent HarborMaster's view of
	// the host must be before it will change one.
	DefaultExecutionInventoryFreshness = 15 * time.Minute
	// DefaultExecutionPolicyFreshness is how recent the policy evaluation must
	// be. Compliance established last week is not compliance established now.
	DefaultExecutionPolicyFreshness = 24 * time.Hour

	DefaultExecutionMaxEvents     = 200
	DefaultExecutionSweepInterval = 1 * time.Minute
	DefaultExecutionPruneInterval = 6 * time.Hour
	// DefaultExecutionRetentionAge is how long a COMPLETED record is kept. A
	// year: this is the record of a container having been replaced, which is
	// the most consequential thing HarborMaster does and the thing an audit is
	// most likely to ask about. A failure that left containers behind is never
	// pruned at all, whatever this says.
	DefaultExecutionRetentionAge = 365 * 24 * time.Hour

	// Execution bounds.
	MinExecutionStartupTimeout     = 10 * time.Second
	MaxExecutionStartupTimeout     = 30 * time.Minute
	MinExecutionStabilityPeriod    = 1 * time.Second
	MaxExecutionStabilityPeriod    = 10 * time.Minute
	MinExecutionHealthPollInterval = 500 * time.Millisecond
	MinExecutionStopTimeout        = 1 * time.Second
	MaxExecutionStopTimeout        = 5 * time.Minute
	MinExecutionRequestTTL         = 1 * time.Minute
	MinExecutionSweepInterval      = 10 * time.Second
	// MaxExecutionConcurrent is the hard ceiling on simultaneous recreations.
	//
	// Four, and the number is a judgement rather than a technical limit. A
	// deployment that wants to replace a dozen containers at once wants fleet
	// updates, which HarborMaster does not have and which would need its own
	// design, its own blast-radius controls, and its own review.
	MaxExecutionConcurrent = 4
	MaxExecutionEvents     = 2000

	// ---- authentication ----------------------------------------------------
	//
	// Unlike every other feature in HarborMaster, authentication has no
	// "enabled" flag. It is ALWAYS on. A switch that could turn it off would be
	// a switch that could be flipped by accident, by a copied compose file, or
	// by an environment variable an attacker who already had a foothold could
	// set -- and the whole point of this phase is that no anonymous request
	// reaches a Docker mutation.

	// DefaultSessionIdleTTL is how long a session survives without use.
	//
	// Eight hours: a working day, so an operator is not signed out over lunch,
	// and short enough that an abandoned browser on a shared machine does not
	// stay usable overnight.
	DefaultSessionIdleTTL = 8 * time.Hour
	// DefaultSessionAbsoluteTTL is the hard ceiling regardless of use.
	//
	// Seven days. It bounds a STOLEN session that is being deliberately kept
	// warm, which idle expiry alone cannot: an attacker with a token can make a
	// request every hour forever.
	DefaultSessionAbsoluteTTL = 7 * 24 * time.Hour
	// DefaultSessionTouchInterval is how often idle expiry is written forward.
	//
	// Five minutes. Writing on every request would make every read a write, and
	// on SQLite's single writer that is the difference between a page load and
	// a queue.
	DefaultSessionTouchInterval = 5 * time.Minute
	// DefaultMaxSessionsPerUser bounds concurrent sessions for one account.
	// Past it the oldest is superseded.
	DefaultMaxSessionsPerUser = 10
	// DefaultSessionRetention is how long a revoked session row is kept, so
	// "why was I signed out" has an answer for a while.
	DefaultSessionRetention = 30 * 24 * time.Hour
	// DefaultSessionSweepInterval is how often expiry and pruning run.
	DefaultSessionSweepInterval = 15 * time.Minute

	// DefaultMaxLoginBackoff caps the per-account exponential backoff.
	//
	// Fifteen minutes. Long enough that online guessing is hopeless, short
	// enough that an operator who fat-fingered their password four times is not
	// locked out of their own estate for the afternoon.
	DefaultMaxLoginBackoff = 15 * time.Minute
	// DefaultMaxAddressFailures is how many failures one source address may
	// accumulate inside the window before it is throttled.
	DefaultMaxAddressFailures = 20
	// DefaultAddressFailureWindow is that window.
	DefaultAddressFailureWindow = 15 * time.Minute

	// DefaultBootstrapTokenTTL is how long the one-time claim token lasts.
	//
	// One hour, and re-minted at every startup. Long enough to copy from a log
	// and paste into a browser, short enough that a token in an old log file is
	// not a way to claim an installation later.
	DefaultBootstrapTokenTTL = 1 * time.Hour

	// DefaultAuditRetention is how long an OPERATIONAL audit event is kept.
	DefaultAuditRetention = 180 * 24 * time.Hour
	// DefaultSecurityAuditRetention is how long an AUTHENTICATION,
	// authorization, or user-administration event is kept.
	//
	// Two years, deliberately much longer. An inventory refresh from six months
	// ago is noise; a failed login from six months ago is the first entry in a
	// story.
	DefaultSecurityAuditRetention = 2 * 365 * 24 * time.Hour
	// DefaultAuditSummaryWindow is how far back the audit page's counters look.
	DefaultAuditSummaryWindow = 24 * time.Hour
	// DefaultAuditPruneInterval is how often retention runs.
	DefaultAuditPruneInterval = 6 * time.Hour

	// Argon2id default cost.
	//
	// 64 MiB, 3 passes, 4 lanes: comfortably above OWASP.s current minimum and
	// chosen so a login costs roughly 100ms on modest hardware. The FLOORS and
	// CEILINGS that stop these being set to something meaningless live in
	// internal/service, beside the code that uses them.
	DefaultArgonMemoryKiB   = 64 * 1024
	DefaultArgonIterations  = 3
	DefaultArgonParallelism = 4

	// Authentication bounds.
	MinSessionIdleTTL       = 5 * time.Minute
	MinSessionAbsoluteTTL   = 15 * time.Minute
	MaxSessionAbsoluteTTL   = 90 * 24 * time.Hour
	MinSessionTouchInterval = 30 * time.Second
	MinSessionSweepInterval = 1 * time.Minute
	MaxSessionsPerUserLimit = 100
	MinLoginBackoff         = 1 * time.Second
	MaxLoginBackoffLimit    = 24 * time.Hour
	MaxAddressFailuresLimit = 10000
	MinBootstrapTokenTTL    = 5 * time.Minute
	MaxBootstrapTokenTTL    = 24 * time.Hour
	MinAuditSummaryWindow   = 1 * time.Hour
	MaxTrustedProxyCount    = 64

	// Planner bounds.
	MinPlannerInterval      = 1 * time.Minute
	MinPlannerPruneInterval = 1 * time.Minute
	MaxPlannerBatchSize     = 5000
	MaxPlannerContainers    = 200000
	MaxPolicyWriteRateLimit = 6000.0
	MaxPolicyWriteRateBurst = 1000

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

// ImageIntel holds settings for image intelligence and update discovery.
//
// This is the section that governs HarborMaster's ONLY outbound network egress.
// Every bound here is a politeness control toward a third party as much as a
// resource control for HarborMaster: a registry that decides HarborMaster is
// abusive rate-limits every client sharing the egress address, not just this
// one.
//
// Nothing here can configure a registry HOST. Destinations come only from image
// references the inventory already holds, which is what keeps the SSRF surface
// closed. There is likewise no credential setting: every lookup is anonymous by
// design, and a private repository reports "unauthorized" rather than becoming a
// reason to start handling secrets.
type ImageIntel struct {
	// Enabled turns the engine on or off. When false nothing is looked up, the
	// endpoints report the feature disabled, and NO OUTBOUND REQUEST IS EVER
	// MADE -- which is the setting an air-gapped deployment wants.
	Enabled bool

	// CollectOnStartup drains the due set once at boot.
	CollectOnStartup bool

	// RefreshInterval is how long a successful answer stays fresh.
	RefreshInterval time.Duration
	// CollectInterval is how often the due set is drained. Much shorter than
	// RefreshInterval because each pass processes a bounded batch; the pair is
	// what spreads a large estate over time instead of bursting.
	CollectInterval time.Duration

	// MaxConcurrentRequests bounds simultaneous registry requests across the
	// whole process.
	MaxConcurrentRequests int
	// MaxReferencesPerPass bounds one batch.
	MaxReferencesPerPass int
	// MaxTrackedReferences bounds how many distinct references are tracked.
	MaxTrackedReferences int
	// MaxTagPages bounds a tag listing. Past it the result is reported as
	// INCOMPLETE rather than as "no update available".
	MaxTagPages int

	// RequestTimeout bounds one registry request, MaxAttempts how many times a
	// transient failure is retried within it, and RetryBackoff the base delay
	// between those attempts.
	RequestTimeout time.Duration
	MaxAttempts    int
	RetryBackoff   time.Duration

	// FailureBackoff is the base delay before a failing reference or host is
	// retried, doubling each time up to MaxFailureBackoff.
	FailureBackoff    time.Duration
	MaxFailureBackoff time.Duration
	// UnsupportedInterval is how often a reference that cannot be looked up is
	// reconsidered.
	UnsupportedInterval time.Duration

	// HistoryRetention is how long an observed change is kept, and
	// PruneInterval how often that retention runs.
	HistoryRetention time.Duration
	PruneInterval    time.Duration
}

// Planner holds settings for change planning and risk analysis.
//
// A change plan is an ASSESSMENT of a proposed image change, built by combining
// data HarborMaster already holds. Nothing executes a plan: there is no setting
// here that pulls an image, recreates a container, restores one, or schedules
// any of that, because HarborMaster has no such capability and this phase adds
// none.
//
// Planning makes no network request and touches no Docker socket. It reads six
// tables and writes one.
type Planner struct {
	// Enabled turns the planner on or off. When false nothing is generated and
	// the endpoints report the feature disabled. Plans already stored remain
	// readable.
	Enabled bool

	// GenerateOnStartup runs one pass at boot, so a HarborMaster that was down
	// while the estate changed has current plans without waiting for the first
	// interval.
	GenerateOnStartup bool

	// Interval is the periodic pass. A pass also runs after every successful
	// inventory refresh. Zero disables the periodic one, leaving refresh and
	// request as the triggers.
	Interval time.Duration

	// BatchSize is how many containers one batch assesses. Each batch costs a
	// fixed number of grouped queries whatever its size, so this trades peak
	// memory against query count.
	BatchSize int
	// MaxContainers caps a whole pass.
	MaxContainers int
	// GenerationTimeout bounds one pass.
	GenerationTimeout time.Duration

	// RetentionAge is how long a SUPERSEDED plan is kept. The current plan for
	// a container is never pruned, whatever its age: it is the standing
	// assessment, and removing it would leave the container looking unplanned
	// rather than unchanged. Zero keeps superseded plans forever.
	RetentionAge time.Duration
	// PruneInterval is how often that retention runs.
	PruneInterval time.Duration
}

// Acquisition holds settings for safe image acquisition.
//
// # This is the one capability that changes the host
//
// Everything else HarborMaster does reads: a local Docker socket, a local
// SQLite file, and -- for image intelligence -- a public registry. This feature
// downloads an approved image into the daemon's local image store, which is a
// write.
//
// **It does not update containers.** A container keeps running the image it was
// created from; an acquired image is another entry in the store beside it.
// There is no setting here that recreates, restarts, or reconfigures anything,
// because HarborMaster has no such capability.
//
// # Off by default
//
// Alone among HarborMaster's features. A deployment gains the ability to write
// to its Docker host only by asking for it.
type Acquisition struct {
	// Enabled turns acquisition on. When false the endpoints report the feature
	// disabled and no pull can be requested or performed. Records already
	// stored remain readable.
	Enabled bool

	// RequireSnapshot refuses an acquisition for a container with no usable
	// configuration snapshot.
	//
	// On by default. Downloading an image puts nothing at risk by itself, so
	// this gate is about what an operator does NEXT: having a recorded
	// configuration to refer back to is the difference between a considered
	// change and an irreversible one. A deployment that does not capture
	// snapshots can turn it off.
	RequireSnapshot bool

	// MaxConcurrent bounds simultaneous transfers across all registries, and
	// MaxPerRegistry bounds them against any one registry.
	//
	// The second is the one that matters to a third party: anonymous rate
	// limits are shared by egress address, and a host that opens several
	// transfers at once against a public registry gets everything behind that
	// address throttled.
	MaxConcurrent  int
	MaxPerRegistry int

	// PullTimeout bounds one transfer. A pull that exceeds it is cancelled and
	// recorded as timed out rather than left holding a slot.
	PullTimeout time.Duration

	// RequestTTL is how long a queued request stays valid. Past it the request
	// EXPIRES unstarted: the evidence behind the approval has aged, and running
	// it later would be acting on a check nobody has repeated.
	RequestTTL time.Duration

	// RegistryFreshness is how recent a successful registry lookup must be for
	// its digest to be acted on. Older evidence does not establish that the
	// digest is still what is being served, and the digest is the entire safety
	// property.
	RegistryFreshness time.Duration

	// MaxEventsPerAcquisition bounds the audit trail for one acquisition. The
	// daemon's progress stream is unbounded and registry-influenced, so this is
	// the last of three independent bounds on how much of it is persisted.
	MaxEventsPerAcquisition int

	// SweepInterval is how often the queue is re-examined for expired requests
	// and for work that a limit was blocking. Zero disables the periodic sweep,
	// leaving request and completion as the triggers.
	SweepInterval time.Duration

	// RetentionAge is how long a COMPLETED acquisition record is kept, and
	// PruneInterval how often that runs. Zero retention keeps them forever,
	// which is valid but unbounded.
	//
	// A completed record is the evidence that an image was downloaded, so this
	// is deliberately long and the most recent record per container is never
	// pruned.
	RetentionAge  time.Duration
	PruneInterval time.Duration
}

// Auth holds settings for authentication, sessions, and the audit log.
//
// # There is no way to switch authentication off
//
// Every other feature in HarborMaster has an Enabled flag. This one does not,
// and the omission is the point: a flag that could disable authentication would
// be a flag that gets flipped by a copied compose file, by a stale environment,
// or by an attacker who already has enough of a foothold to set one. Phase 9.5
// exists so that no anonymous request can reach a Docker mutation, and an
// off-switch would make that a configuration property rather than a fact.
//
// # HTTPS is the operator's responsibility, and HarborMaster says so
//
// Session cookies are marked Secure when the request arrived over HTTPS or
// through a trusted proxy that says it did. Over plain HTTP on loopback -- the
// default deployment -- the cookie cannot be Secure, because a browser would
// then refuse to send it at all. See CookieSecure.
type Auth struct {
	// SessionIdleTTL is how long a session survives without use, and
	// SessionAbsoluteTTL the hard ceiling regardless of use. Both are enforced:
	// the first bounds an abandoned session, the second a stolen one being kept
	// deliberately warm.
	SessionIdleTTL     time.Duration
	SessionAbsoluteTTL time.Duration
	// SessionTouchInterval is how often idle expiry is written forward. It
	// exists so a read does not become a write on every request.
	SessionTouchInterval time.Duration
	// MaxSessionsPerUser bounds concurrent sessions for one account. Past it
	// the oldest is superseded.
	MaxSessionsPerUser int
	// SessionRetention is how long a revoked session row is kept before
	// pruning, and SessionSweepInterval how often expiry and pruning run.
	SessionRetention     time.Duration
	SessionSweepInterval time.Duration

	// MaxLoginBackoff caps the per-account exponential backoff. A cap rather
	// than a hard lockout: a lockout lets anyone who knows a username deny that
	// account service by guessing at it.
	MaxLoginBackoff time.Duration
	// MaxAddressFailures and AddressFailureWindow throttle one source address.
	// Zero failures disables the address throttle, leaving only the per-account
	// backoff.
	MaxAddressFailures   int
	AddressFailureWindow time.Duration

	// Argon2id cost. Stored alongside each credential, so raising these does
	// not invalidate existing passwords -- a login below the current policy is
	// transparently re-hashed.
	ArgonMemoryKiB   int
	ArgonIterations  int
	ArgonParallelism int

	// BootstrapTokenTTL is how long the one-time claim token lasts. Re-minted
	// at every startup of an unclaimed installation.
	BootstrapTokenTTL time.Duration

	// AuditRetention and SecurityAuditRetention are how long operational and
	// security audit events are kept. Zero keeps them forever.
	AuditRetention         time.Duration
	SecurityAuditRetention time.Duration
	// AuditSummaryWindow is how far back the audit page's counters look, and
	// AuditPruneInterval how often retention runs.
	AuditSummaryWindow time.Duration
	AuditPruneInterval time.Duration

	// TrustedProxies are CIDR ranges whose X-Forwarded-For HarborMaster will
	// believe.
	//
	// EMPTY BY DEFAULT, and that default is load-bearing. A forwarding header
	// is attacker-controlled text; trusting it unconditionally would let anyone
	// spoof the source address in the audit log and evade the per-address
	// throttle by rotating it. With no trusted proxies configured, the address
	// is always the transport peer, which cannot be forged.
	TrustedProxies []string

	// CookieSecure forces the Secure attribute on session cookies regardless of
	// how the request arrived.
	//
	// Needed when HarborMaster sits behind a TLS-terminating proxy that is not
	// in TrustedProxies -- the request reaches HarborMaster over plain HTTP,
	// but the browser's connection was HTTPS and the cookie must be marked to
	// match. Setting it on a genuinely plain-HTTP deployment makes the browser
	// discard the cookie and nobody can log in, which is a loud failure rather
	// than a silent weakening.
	CookieSecure bool
	// CookieSameSiteLax relaxes SameSite from Strict to Lax.
	//
	// Strict is the default and the right answer for a tool with no
	// cross-origin flows. Lax exists for the documented compatibility case: an
	// operator following a link into HarborMaster from a chat client or a
	// ticket system arrives without their cookie under Strict, which reads as a
	// spurious logout. It remains safe for this application because every
	// state-changing request additionally requires a CSRF header that a
	// cross-site navigation cannot set.
	CookieSameSiteLax bool
}

// Execution holds settings for manual container recreation.
//
// # This is the capability that changes something that is RUNNING
//
// Acquisition writes to the image store, which affects nothing that is serving.
// This stops a container, creates a replacement, starts it, proves it, and
// removes what it replaced. It is the largest privilege HarborMaster has, and
// every setting here bounds it rather than extends it.
//
// **It is manual, single, and single-use.** No timer starts a recreation, no
// setting makes one automatic, nothing here acts on more than one container,
// and a succeeded acquisition can be executed exactly once. There is
// deliberately no rollback setting, because there is no rollback.
//
// # Off by default
//
// Along with acquisition, and for a stronger reason.
type Execution struct {
	// Enabled turns recreation on. When false the endpoints report the feature
	// disabled and no recreation can be requested or performed. Records already
	// stored remain readable.
	Enabled bool

	// StartupTimeout bounds the wait for a replacement to become healthy. A
	// replacement that exceeds it is treated as failed: quarantined, with the
	// original preserved and a recovery plan recorded.
	StartupTimeout time.Duration

	// StabilityPeriod is how long a container with NO health check must stay
	// running to count as stable.
	//
	// Weaker evidence than a health check and treated as such. It establishes
	// that the container did not crash on startup, which is the most that can
	// be established about a container that does not report on itself.
	StabilityPeriod time.Duration

	// HealthPollInterval is how often the replacement is re-inspected while
	// waiting. Bounded below so a wait cannot become a busy loop against the
	// Docker socket.
	HealthPollInterval time.Duration

	// StopTimeout is how long the ORIGINAL is given to exit on its own before
	// the daemon terminates it.
	StopTimeout time.Duration

	// MaxConcurrent bounds simultaneous recreations. One by default: a
	// recreation stops something that is serving, and several at once turn a
	// contained failure into an outage nobody chose.
	MaxConcurrent int

	// RequestTTL is how long a queued request stays valid. Past it the request
	// EXPIRES unstarted, having changed nothing.
	RequestTTL time.Duration

	// The freshness windows. Each answers the same question about a different
	// piece of evidence: is this recent enough to act on? Acting on a stale
	// answer is the failure mode the whole preflight exists to prevent, and a
	// timestamp is the only way to detect the kind of staleness that is nobody's
	// fault -- evidence that was correct and simply got old.
	AcquisitionFreshness time.Duration
	InventoryFreshness   time.Duration
	PolicyFreshness      time.Duration

	// RequireSnapshot refuses a recreation for a container with no usable
	// configuration snapshot.
	//
	// On by default, and a stronger gate here than for acquisition. Recreating
	// without a recorded baseline means that if the replacement is wrong, there
	// is no authoritative account of what the container looked like before.
	RequireSnapshot bool

	// MaxEventsPerExecution bounds the audit trail for one recreation.
	MaxEventsPerExecution int

	// SweepInterval is how often the queue is re-examined for expired requests
	// and for work a limit was blocking. Zero disables the periodic sweep.
	SweepInterval time.Duration

	// RetentionAge is how long a COMPLETED record is kept, and PruneInterval
	// how often that runs. Zero retention keeps them forever.
	//
	// A failure that left containers on the host is NEVER pruned, whatever this
	// says: removing it would leave an operator with two unexplained containers
	// and nothing accounting for them.
	RetentionAge  time.Duration
	PruneInterval time.Duration
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
	ImageIntel  ImageIntel
	Planner     Planner
	Acquisition Acquisition
	Execution   Execution
	Auth        Auth
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

	cfg.ImageIntel.Enabled, err = boolVar(lookup, "IMAGE_INTEL_ENABLED", DefaultImageIntelEnabled)
	collect(err)
	cfg.ImageIntel.CollectOnStartup, err = boolVar(lookup, "IMAGE_INTEL_COLLECT_ON_STARTUP", DefaultImageIntelCollectOnStartup)
	collect(err)
	cfg.ImageIntel.RefreshInterval, err = durationVar(lookup, "IMAGE_INTEL_REFRESH_INTERVAL", DefaultImageIntelRefreshInterval)
	collect(err)
	cfg.ImageIntel.CollectInterval, err = durationVar(lookup, "IMAGE_INTEL_COLLECT_INTERVAL", DefaultImageIntelCollectInterval)
	collect(err)
	cfg.ImageIntel.RequestTimeout, err = durationVar(lookup, "IMAGE_INTEL_REQUEST_TIMEOUT", DefaultImageIntelRequestTimeout)
	collect(err)
	cfg.ImageIntel.RetryBackoff, err = durationVar(lookup, "IMAGE_INTEL_RETRY_BACKOFF", DefaultImageIntelRetryBackoff)
	collect(err)
	cfg.ImageIntel.FailureBackoff, err = durationVar(lookup, "IMAGE_INTEL_FAILURE_BACKOFF", DefaultImageIntelFailureBackoff)
	collect(err)
	cfg.ImageIntel.MaxFailureBackoff, err = durationVar(lookup, "IMAGE_INTEL_MAX_FAILURE_BACKOFF", DefaultImageIntelMaxFailureBackoff)
	collect(err)
	cfg.ImageIntel.UnsupportedInterval, err = durationVar(lookup, "IMAGE_INTEL_UNSUPPORTED_INTERVAL", DefaultImageIntelUnsupportedInterval)
	collect(err)
	cfg.ImageIntel.HistoryRetention, err = durationVar(lookup, "IMAGE_INTEL_HISTORY_RETENTION", DefaultImageIntelHistoryRetention)
	collect(err)
	cfg.ImageIntel.PruneInterval, err = durationVar(lookup, "IMAGE_INTEL_PRUNE_INTERVAL", DefaultImageIntelPruneInterval)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"IMAGE_INTEL_MAX_CONCURRENT_REQUESTS", DefaultImageIntelMaxConcurrentRequests, &cfg.ImageIntel.MaxConcurrentRequests},
		{"IMAGE_INTEL_MAX_REFERENCES_PER_PASS", DefaultImageIntelMaxReferencesPerPass, &cfg.ImageIntel.MaxReferencesPerPass},
		{"IMAGE_INTEL_MAX_TRACKED_REFERENCES", DefaultImageIntelMaxTrackedReferences, &cfg.ImageIntel.MaxTrackedReferences},
		{"IMAGE_INTEL_MAX_TAG_PAGES", DefaultImageIntelMaxTagPages, &cfg.ImageIntel.MaxTagPages},
		{"IMAGE_INTEL_MAX_ATTEMPTS", DefaultImageIntelMaxAttempts, &cfg.ImageIntel.MaxAttempts},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	cfg.Planner.Enabled, err = boolVar(lookup, "PLANNER_ENABLED", DefaultPlannerEnabled)
	collect(err)
	cfg.Planner.GenerateOnStartup, err = boolVar(lookup, "PLANNER_GENERATE_ON_STARTUP", DefaultPlannerGenerateOnStartup)
	collect(err)
	cfg.Planner.Interval, err = durationVar(lookup, "PLANNER_INTERVAL", DefaultPlannerInterval)
	collect(err)
	cfg.Planner.GenerationTimeout, err = durationVar(lookup, "PLANNER_GENERATION_TIMEOUT", DefaultPlannerGenerationTimeout)
	collect(err)
	cfg.Planner.RetentionAge, err = durationVar(lookup, "PLANNER_RETENTION_AGE", DefaultPlannerRetentionAge)
	collect(err)
	cfg.Planner.PruneInterval, err = durationVar(lookup, "PLANNER_PRUNE_INTERVAL", DefaultPlannerPruneInterval)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"PLANNER_BATCH_SIZE", DefaultPlannerBatchSize, &cfg.Planner.BatchSize},
		{"PLANNER_MAX_CONTAINERS", DefaultPlannerMaxContainers, &cfg.Planner.MaxContainers},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	cfg.Acquisition.Enabled, err = boolVar(lookup, "ACQUISITION_ENABLED", DefaultAcquisitionEnabled)
	collect(err)
	cfg.Acquisition.RequireSnapshot, err = boolVar(lookup, "ACQUISITION_REQUIRE_SNAPSHOT", DefaultAcquisitionRequireSnapshot)
	collect(err)
	cfg.Acquisition.PullTimeout, err = durationVar(lookup, "ACQUISITION_PULL_TIMEOUT", DefaultAcquisitionPullTimeout)
	collect(err)
	cfg.Acquisition.RequestTTL, err = durationVar(lookup, "ACQUISITION_REQUEST_TTL", DefaultAcquisitionRequestTTL)
	collect(err)
	cfg.Acquisition.RegistryFreshness, err = durationVar(lookup, "ACQUISITION_REGISTRY_FRESHNESS", DefaultAcquisitionRegistryFreshness)
	collect(err)
	cfg.Acquisition.SweepInterval, err = durationVar(lookup, "ACQUISITION_SWEEP_INTERVAL", DefaultAcquisitionSweepInterval)
	collect(err)
	cfg.Acquisition.RetentionAge, err = durationVar(lookup, "ACQUISITION_RETENTION_AGE", DefaultAcquisitionRetentionAge)
	collect(err)
	cfg.Acquisition.PruneInterval, err = durationVar(lookup, "ACQUISITION_PRUNE_INTERVAL", DefaultAcquisitionPruneInterval)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"ACQUISITION_MAX_CONCURRENT", DefaultAcquisitionMaxConcurrent, &cfg.Acquisition.MaxConcurrent},
		{"ACQUISITION_MAX_PER_REGISTRY", DefaultAcquisitionMaxPerRegistry, &cfg.Acquisition.MaxPerRegistry},
		{"ACQUISITION_MAX_EVENTS", DefaultAcquisitionMaxEvents, &cfg.Acquisition.MaxEventsPerAcquisition},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	cfg.Execution.Enabled, err = boolVar(lookup, "EXECUTION_ENABLED", DefaultExecutionEnabled)
	collect(err)
	cfg.Execution.RequireSnapshot, err = boolVar(lookup, "EXECUTION_REQUIRE_SNAPSHOT", true)
	collect(err)

	for _, target := range []struct {
		name     string
		fallback time.Duration
		into     *time.Duration
	}{
		{"EXECUTION_STARTUP_TIMEOUT", DefaultExecutionStartupTimeout, &cfg.Execution.StartupTimeout},
		{"EXECUTION_STABILITY_PERIOD", DefaultExecutionStabilityPeriod, &cfg.Execution.StabilityPeriod},
		{"EXECUTION_HEALTH_POLL_INTERVAL", DefaultExecutionHealthPollInterval, &cfg.Execution.HealthPollInterval},
		{"EXECUTION_STOP_TIMEOUT", DefaultExecutionStopTimeout, &cfg.Execution.StopTimeout},
		{"EXECUTION_REQUEST_TTL", DefaultExecutionRequestTTL, &cfg.Execution.RequestTTL},
		{"EXECUTION_ACQUISITION_FRESHNESS", DefaultExecutionAcquisitionFreshness, &cfg.Execution.AcquisitionFreshness},
		{"EXECUTION_INVENTORY_FRESHNESS", DefaultExecutionInventoryFreshness, &cfg.Execution.InventoryFreshness},
		{"EXECUTION_POLICY_FRESHNESS", DefaultExecutionPolicyFreshness, &cfg.Execution.PolicyFreshness},
		{"EXECUTION_SWEEP_INTERVAL", DefaultExecutionSweepInterval, &cfg.Execution.SweepInterval},
		{"EXECUTION_RETENTION_AGE", DefaultExecutionRetentionAge, &cfg.Execution.RetentionAge},
		{"EXECUTION_PRUNE_INTERVAL", DefaultExecutionPruneInterval, &cfg.Execution.PruneInterval},
	} {
		value, convErr := durationVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"EXECUTION_MAX_CONCURRENT", DefaultExecutionMaxConcurrent, &cfg.Execution.MaxConcurrent},
		{"EXECUTION_MAX_EVENTS", DefaultExecutionMaxEvents, &cfg.Execution.MaxEventsPerExecution},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	// ---- authentication ----------------------------------------------------
	//
	// No AUTH_ENABLED. Authentication is always on: see the Auth type.

	for _, target := range []struct {
		name     string
		fallback time.Duration
		into     *time.Duration
	}{
		{"SESSION_IDLE_TTL", DefaultSessionIdleTTL, &cfg.Auth.SessionIdleTTL},
		{"SESSION_ABSOLUTE_TTL", DefaultSessionAbsoluteTTL, &cfg.Auth.SessionAbsoluteTTL},
		{"SESSION_TOUCH_INTERVAL", DefaultSessionTouchInterval, &cfg.Auth.SessionTouchInterval},
		{"SESSION_RETENTION", DefaultSessionRetention, &cfg.Auth.SessionRetention},
		{"SESSION_SWEEP_INTERVAL", DefaultSessionSweepInterval, &cfg.Auth.SessionSweepInterval},
		{"LOGIN_MAX_BACKOFF", DefaultMaxLoginBackoff, &cfg.Auth.MaxLoginBackoff},
		{"LOGIN_ADDRESS_WINDOW", DefaultAddressFailureWindow, &cfg.Auth.AddressFailureWindow},
		{"BOOTSTRAP_TOKEN_TTL", DefaultBootstrapTokenTTL, &cfg.Auth.BootstrapTokenTTL},
		{"AUDIT_RETENTION", DefaultAuditRetention, &cfg.Auth.AuditRetention},
		{"AUDIT_SECURITY_RETENTION", DefaultSecurityAuditRetention, &cfg.Auth.SecurityAuditRetention},
		{"AUDIT_SUMMARY_WINDOW", DefaultAuditSummaryWindow, &cfg.Auth.AuditSummaryWindow},
		{"AUDIT_PRUNE_INTERVAL", DefaultAuditPruneInterval, &cfg.Auth.AuditPruneInterval},
	} {
		value, convErr := durationVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	for _, target := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"SESSION_MAX_PER_USER", DefaultMaxSessionsPerUser, &cfg.Auth.MaxSessionsPerUser},
		{"LOGIN_MAX_ADDRESS_FAILURES", DefaultMaxAddressFailures, &cfg.Auth.MaxAddressFailures},
		{"ARGON_MEMORY_KIB", DefaultArgonMemoryKiB, &cfg.Auth.ArgonMemoryKiB},
		{"ARGON_ITERATIONS", DefaultArgonIterations, &cfg.Auth.ArgonIterations},
		{"ARGON_PARALLELISM", DefaultArgonParallelism, &cfg.Auth.ArgonParallelism},
	} {
		value, convErr := intVar(lookup, target.name, target.fallback)
		collect(convErr)
		*target.into = value
	}

	cfg.Auth.CookieSecure, err = boolVar(lookup, "COOKIE_SECURE", false)
	collect(err)
	cfg.Auth.CookieSameSiteLax, err = boolVar(lookup, "COOKIE_SAMESITE_LAX", false)
	collect(err)
	cfg.Auth.TrustedProxies = listVar(lookup, "TRUSTED_PROXIES", nil)

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
	errs = append(errs, c.ImageIntel.validate()...)
	errs = append(errs, c.Planner.validate()...)
	errs = append(errs, c.Acquisition.validate()...)
	errs = append(errs, c.Execution.validate()...)
	errs = append(errs, c.Auth.validate()...)

	// Recreation without acquisition is not a configuration, it is a
	// contradiction: an execution names an ACQUISITION, and with acquisition
	// switched off there can never be one to name.
	//
	// Caught at startup rather than surfacing as a permanent refusal the first
	// time an operator presses the button. A cross-section check, so it lives
	// here rather than in either section's own validate.
	if c.Execution.Enabled && !c.Acquisition.Enabled {
		errs = append(errs, fmt.Errorf(
			"%sEXECUTION_ENABLED requires %sACQUISITION_ENABLED: a recreation names an "+
				"acquisition, and with acquisition switched off there can never be one",
			envPrefix, envPrefix))
	}

	return errors.Join(errs...)
}

// validate checks the authentication settings.
//
// Every failure here is a startup refusal. Authentication is the one subsystem
// where a setting that is merely odd is a setting that gets somebody
// compromised, and a misconfiguration discovered at the first login is
// discovered by an operator who is already locked out.
func (a Auth) validate() []error {
	var errs []error

	if a.SessionIdleTTL < MinSessionIdleTTL {
		errs = append(errs, fmt.Errorf("%sSESSION_IDLE_TTL must be at least %s",
			envPrefix, MinSessionIdleTTL))
	}
	if a.SessionAbsoluteTTL < MinSessionAbsoluteTTL || a.SessionAbsoluteTTL > MaxSessionAbsoluteTTL {
		errs = append(errs, fmt.Errorf("%sSESSION_ABSOLUTE_TTL must be between %s and %s",
			envPrefix, MinSessionAbsoluteTTL, MaxSessionAbsoluteTTL))
	}
	// An idle window longer than the absolute ceiling is not a stricter
	// setting, it is a setting with no effect -- and an operator who wrote it
	// believes the ceiling is longer than it is.
	if a.SessionIdleTTL > a.SessionAbsoluteTTL {
		errs = append(errs, fmt.Errorf(
			"%sSESSION_IDLE_TTL (%s) must not exceed %sSESSION_ABSOLUTE_TTL (%s)",
			envPrefix, a.SessionIdleTTL, envPrefix, a.SessionAbsoluteTTL))
	}
	if a.SessionTouchInterval < MinSessionTouchInterval {
		errs = append(errs, fmt.Errorf("%sSESSION_TOUCH_INTERVAL must be at least %s",
			envPrefix, MinSessionTouchInterval))
	}
	// A touch interval past the idle window means the idle expiry is never
	// written forward, so every session dies at exactly the idle TTL however
	// actively it is used.
	if a.SessionTouchInterval >= a.SessionIdleTTL {
		errs = append(errs, fmt.Errorf(
			"%sSESSION_TOUCH_INTERVAL (%s) must be shorter than %sSESSION_IDLE_TTL (%s), "+
				"or an active session would still expire on schedule",
			envPrefix, a.SessionTouchInterval, envPrefix, a.SessionIdleTTL))
	}
	if a.SessionSweepInterval < MinSessionSweepInterval {
		errs = append(errs, fmt.Errorf("%sSESSION_SWEEP_INTERVAL must be at least %s",
			envPrefix, MinSessionSweepInterval))
	}
	if a.SessionRetention < 0 {
		errs = append(errs, fmt.Errorf("%sSESSION_RETENTION must not be negative", envPrefix))
	}

	if a.MaxSessionsPerUser < 1 || a.MaxSessionsPerUser > MaxSessionsPerUserLimit {
		errs = append(errs, fmt.Errorf("%sSESSION_MAX_PER_USER must be between 1 and %d",
			envPrefix, MaxSessionsPerUserLimit))
	}

	if a.MaxLoginBackoff < MinLoginBackoff || a.MaxLoginBackoff > MaxLoginBackoffLimit {
		errs = append(errs, fmt.Errorf("%sLOGIN_MAX_BACKOFF must be between %s and %s",
			envPrefix, MinLoginBackoff, MaxLoginBackoffLimit))
	}
	// Zero disables the address throttle, which is a documented choice for a
	// deployment behind a proxy that already rate-limits. Negative is not a way
	// to express anything.
	if a.MaxAddressFailures < 0 || a.MaxAddressFailures > MaxAddressFailuresLimit {
		errs = append(errs, fmt.Errorf("%sLOGIN_MAX_ADDRESS_FAILURES must be between 0 and %d",
			envPrefix, MaxAddressFailuresLimit))
	}
	if a.MaxAddressFailures > 0 && a.AddressFailureWindow <= 0 {
		errs = append(errs, fmt.Errorf(
			"%sLOGIN_ADDRESS_WINDOW must be positive when %sLOGIN_MAX_ADDRESS_FAILURES is set",
			envPrefix, envPrefix))
	}

	if a.BootstrapTokenTTL < MinBootstrapTokenTTL || a.BootstrapTokenTTL > MaxBootstrapTokenTTL {
		errs = append(errs, fmt.Errorf("%sBOOTSTRAP_TOKEN_TTL must be between %s and %s",
			envPrefix, MinBootstrapTokenTTL, MaxBootstrapTokenTTL))
	}

	if a.AuditRetention < 0 {
		errs = append(errs, fmt.Errorf("%sAUDIT_RETENTION must not be negative", envPrefix))
	}
	if a.SecurityAuditRetention < 0 {
		errs = append(errs, fmt.Errorf("%sAUDIT_SECURITY_RETENTION must not be negative", envPrefix))
	}
	// Security events must outlive operational ones. The reverse would mean the
	// authentication history is pruned before the inventory refreshes it is
	// meant to explain.
	if a.AuditRetention > 0 && a.SecurityAuditRetention > 0 &&
		a.SecurityAuditRetention < a.AuditRetention {
		errs = append(errs, fmt.Errorf(
			"%sAUDIT_SECURITY_RETENTION (%s) must be at least %sAUDIT_RETENTION (%s)",
			envPrefix, a.SecurityAuditRetention, envPrefix, a.AuditRetention))
	}
	if a.AuditSummaryWindow < MinAuditSummaryWindow {
		errs = append(errs, fmt.Errorf("%sAUDIT_SUMMARY_WINDOW must be at least %s",
			envPrefix, MinAuditSummaryWindow))
	}
	if a.AuditPruneInterval < MinPlannerPruneInterval {
		errs = append(errs, fmt.Errorf("%sAUDIT_PRUNE_INTERVAL must be at least %s",
			envPrefix, MinPlannerPruneInterval))
	}

	errs = append(errs, a.validateTrustedProxies()...)
	return errs
}

// validateTrustedProxies checks the forwarding-header allowlist.
//
// # Why this is validated so strictly
//
// A trusted proxy entry is permission to believe an attacker-controlled header
// about where a request came from. Getting it wrong does not fail loudly: it
// silently makes the audit log's source addresses forgeable and the per-address
// throttle evadable, which is exactly the kind of weakening nobody notices.
//
// So an unparseable entry refuses startup rather than being skipped, and the
// list is bounded so a configuration mistake cannot make every request walk a
// long list.
func (a Auth) validateTrustedProxies() []error {
	if len(a.TrustedProxies) == 0 {
		return nil
	}

	var errs []error
	if len(a.TrustedProxies) > MaxTrustedProxyCount {
		errs = append(errs, fmt.Errorf("%sTRUSTED_PROXIES must list at most %d entries",
			envPrefix, MaxTrustedProxyCount))
	}

	for _, entry := range a.TrustedProxies {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		// A bare address is accepted and read as a single-host range, because
		// "127.0.0.1" is what an operator writes and refusing it would be
		// pedantry. Anything else must be a CIDR.
		if _, err := netip.ParseAddr(trimmed); err == nil {
			continue
		}
		if _, err := netip.ParsePrefix(trimmed); err != nil {
			// The offending value is NOT echoed. It is operator-supplied text
			// that reaches a startup log, and naming the variable is enough to
			// find it.
			errs = append(errs, fmt.Errorf(
				"%sTRUSTED_PROXIES contains an entry that is neither an IP address nor a CIDR range",
				envPrefix))
			break
		}
	}
	return errs
}

// validate checks the container recreation settings.
//
// Validated even when recreation is disabled, and the reasoning that applies to
// every other section applies most sharply here: the day someone flips this one
// on is the day HarborMaster gains the ability to stop a running container, and
// that is not the day to discover a timeout is nonsense.
func (e Execution) validate() []error {
	var errs []error

	for _, b := range []struct {
		name     string
		value    time.Duration
		min, max time.Duration
	}{
		{"EXECUTION_STARTUP_TIMEOUT", e.StartupTimeout,
			MinExecutionStartupTimeout, MaxExecutionStartupTimeout},
		{"EXECUTION_STABILITY_PERIOD", e.StabilityPeriod,
			MinExecutionStabilityPeriod, MaxExecutionStabilityPeriod},
		{"EXECUTION_STOP_TIMEOUT", e.StopTimeout,
			MinExecutionStopTimeout, MaxExecutionStopTimeout},
	} {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %s and %s",
				envPrefix, b.name, b.min, b.max))
		}
	}

	// Bounded below so a wait cannot become a busy loop against the Docker
	// socket, and below the startup timeout so at least one poll happens.
	if e.HealthPollInterval < MinExecutionHealthPollInterval {
		errs = append(errs, fmt.Errorf("%sEXECUTION_HEALTH_POLL_INTERVAL must be at least %s",
			envPrefix, MinExecutionHealthPollInterval))
	}
	if e.HealthPollInterval > e.StartupTimeout {
		errs = append(errs, fmt.Errorf(
			"%sEXECUTION_HEALTH_POLL_INTERVAL (%s) must not exceed %sEXECUTION_STARTUP_TIMEOUT (%s), "+
				"or the replacement would never be checked",
			envPrefix, e.HealthPollInterval, envPrefix, e.StartupTimeout))
	}

	if e.RequestTTL < MinExecutionRequestTTL {
		errs = append(errs, fmt.Errorf("%sEXECUTION_REQUEST_TTL must be at least %s",
			envPrefix, MinExecutionRequestTTL))
	}
	// Zero is the documented way to disable the periodic sweep.
	if e.SweepInterval != 0 && e.SweepInterval < MinExecutionSweepInterval {
		errs = append(errs, fmt.Errorf("%sEXECUTION_SWEEP_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinExecutionSweepInterval))
	}
	if e.PruneInterval < MinPlannerPruneInterval {
		errs = append(errs, fmt.Errorf("%sEXECUTION_PRUNE_INTERVAL must be at least %s",
			envPrefix, MinPlannerPruneInterval))
	}
	if e.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sEXECUTION_RETENTION_AGE must not be negative", envPrefix))
	}

	// Every freshness window must be positive. A zero or negative one would
	// mean "any age is acceptable", which does not relax the check -- it
	// removes it, while leaving the configuration looking as though the check
	// is still there.
	for _, window := range []struct {
		name  string
		value time.Duration
	}{
		{"EXECUTION_ACQUISITION_FRESHNESS", e.AcquisitionFreshness},
		{"EXECUTION_INVENTORY_FRESHNESS", e.InventoryFreshness},
		{"EXECUTION_POLICY_FRESHNESS", e.PolicyFreshness},
	} {
		if window.value <= 0 {
			errs = append(errs, fmt.Errorf("%s%s must be positive", envPrefix, window.name))
		}
	}

	for _, b := range []struct {
		name            string
		value, min, max int
	}{
		{"EXECUTION_MAX_CONCURRENT", e.MaxConcurrent, 1, MaxExecutionConcurrent},
		{"EXECUTION_MAX_EVENTS", e.MaxEventsPerExecution, 1, MaxExecutionEvents},
	} {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	return errs
}

// validate checks the image acquisition settings.
//
// Validated even when acquisition is disabled, for the same reason every other
// section is: a configuration error that only surfaces the day someone flips
// the feature on is a worse failure than one caught at startup. That reasoning
// is sharper here than anywhere else, because the day someone flips this one on
// is the day HarborMaster gains write access to a privileged socket.
func (a Acquisition) validate() []error {
	var errs []error

	if a.PullTimeout < MinAcquisitionPullTimeout {
		errs = append(errs, fmt.Errorf("%sACQUISITION_PULL_TIMEOUT must be at least %s",
			envPrefix, MinAcquisitionPullTimeout))
	}
	if a.RequestTTL < MinAcquisitionRequestTTL {
		errs = append(errs, fmt.Errorf("%sACQUISITION_REQUEST_TTL must be at least %s",
			envPrefix, MinAcquisitionRequestTTL))
	}
	// Zero is the documented way to disable the periodic sweep, leaving request
	// and completion as the triggers. A tiny non-zero one would poll the queue
	// continuously for no benefit.
	if a.SweepInterval != 0 && a.SweepInterval < MinAcquisitionSweepInterval {
		errs = append(errs, fmt.Errorf("%sACQUISITION_SWEEP_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinAcquisitionSweepInterval))
	}
	if a.PruneInterval < MinPlannerPruneInterval {
		errs = append(errs, fmt.Errorf("%sACQUISITION_PRUNE_INTERVAL must be at least %s",
			envPrefix, MinPlannerPruneInterval))
	}
	// Registry evidence older than this is refused. A zero or negative window
	// would mean "any age is acceptable", which would defeat the freshness
	// check entirely rather than relaxing it.
	if a.RegistryFreshness <= 0 {
		errs = append(errs, fmt.Errorf("%sACQUISITION_REGISTRY_FRESHNESS must be positive", envPrefix))
	}
	// Zero keeps completed records forever, which is valid but unbounded.
	// Negative is not a way to do anything.
	if a.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sACQUISITION_RETENTION_AGE must not be negative", envPrefix))
	}

	for _, b := range []struct {
		name            string
		value, min, max int
	}{
		{"ACQUISITION_MAX_CONCURRENT", a.MaxConcurrent, 1, MaxAcquisitionConcurrent},
		{"ACQUISITION_MAX_PER_REGISTRY", a.MaxPerRegistry, 1, MaxAcquisitionConcurrent},
		{"ACQUISITION_MAX_EVENTS", a.MaxEventsPerAcquisition, 1, MaxAcquisitionEvents},
	} {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	// A per-registry limit above the global one is not an error the bounds
	// above can catch, and it would be a quietly misleading configuration: the
	// stated per-registry ceiling would never be reachable.
	if a.MaxPerRegistry > a.MaxConcurrent {
		errs = append(errs, fmt.Errorf(
			"%sACQUISITION_MAX_PER_REGISTRY (%d) must not exceed %sACQUISITION_MAX_CONCURRENT (%d)",
			envPrefix, a.MaxPerRegistry, envPrefix, a.MaxConcurrent))
	}

	return errs
}

// validate checks the change planner settings.
//
// Validated even when the planner is disabled, for the same reason every other
// section is: a configuration error that only surfaces the day someone flips
// the feature on is a worse failure than one caught at startup.
func (p Planner) validate() []error {
	var errs []error

	// Zero is the documented way to disable the periodic pass, leaving refresh
	// and request as the triggers. A tiny non-zero one would re-plan the estate
	// continuously for no benefit, since an unchanged estate writes nothing.
	if p.Interval != 0 && p.Interval < MinPlannerInterval {
		errs = append(errs, fmt.Errorf("%sPLANNER_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinPlannerInterval))
	}
	if p.PruneInterval < MinPlannerPruneInterval {
		errs = append(errs, fmt.Errorf("%sPLANNER_PRUNE_INTERVAL must be at least %s",
			envPrefix, MinPlannerPruneInterval))
	}
	if p.GenerationTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%sPLANNER_GENERATION_TIMEOUT must be positive", envPrefix))
	}
	// Zero keeps superseded plans forever, which is valid but unbounded.
	// Negative is not a way to do anything.
	if p.RetentionAge < 0 {
		errs = append(errs, fmt.Errorf("%sPLANNER_RETENTION_AGE must not be negative", envPrefix))
	}

	for _, b := range []struct {
		name            string
		value, min, max int
	}{
		{"PLANNER_BATCH_SIZE", p.BatchSize, 1, MaxPlannerBatchSize},
		{"PLANNER_MAX_CONTAINERS", p.MaxContainers, 1, MaxPlannerContainers},
	} {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	return errs
}

// validate checks the image intelligence settings.
//
// Validated even when the engine is disabled, for the same reason the drift and
// policy settings are: a configuration error that only surfaces the day someone
// flips the feature on is a worse failure than one caught at startup.
func (i ImageIntel) validate() []error {
	var errs []error

	if i.RefreshInterval < MinImageIntelRefreshInterval {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_REFRESH_INTERVAL must be at least %s",
			envPrefix, MinImageIntelRefreshInterval))
	}
	// Zero is the documented way to disable the collection tick, leaving the
	// engine to run only when asked. A tiny non-zero one would poll registries
	// continuously, which is how a client gets blocked.
	if i.CollectInterval != 0 && i.CollectInterval < MinImageIntelCollectInterval {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_COLLECT_INTERVAL must be 0 (disabled) or at least %s",
			envPrefix, MinImageIntelCollectInterval))
	}
	if i.PruneInterval < MinImageIntelPruneInterval {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_PRUNE_INTERVAL must be at least %s",
			envPrefix, MinImageIntelPruneInterval))
	}
	if i.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_REQUEST_TIMEOUT must be positive", envPrefix))
	}
	if i.RetryBackoff <= 0 {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_RETRY_BACKOFF must be positive", envPrefix))
	}
	if i.FailureBackoff <= 0 {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_FAILURE_BACKOFF must be positive", envPrefix))
	}
	if i.MaxFailureBackoff < i.FailureBackoff {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_MAX_FAILURE_BACKOFF must be at least %sIMAGE_INTEL_FAILURE_BACKOFF",
			envPrefix, envPrefix))
	}
	if i.UnsupportedInterval <= 0 {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_UNSUPPORTED_INTERVAL must be positive", envPrefix))
	}
	// Zero keeps history forever, which is valid but unbounded. Negative is not
	// a way to do anything.
	if i.HistoryRetention < 0 {
		errs = append(errs, fmt.Errorf("%sIMAGE_INTEL_HISTORY_RETENTION must not be negative", envPrefix))
	}

	for _, b := range []struct {
		name            string
		value, min, max int
	}{
		{"IMAGE_INTEL_MAX_CONCURRENT_REQUESTS", i.MaxConcurrentRequests, 1, MaxImageIntelConcurrentRequests},
		{"IMAGE_INTEL_MAX_REFERENCES_PER_PASS", i.MaxReferencesPerPass, 1, MaxImageIntelReferencesPerPass},
		{"IMAGE_INTEL_MAX_TRACKED_REFERENCES", i.MaxTrackedReferences, 1, MaxImageIntelTrackedReferences},
		{"IMAGE_INTEL_MAX_TAG_PAGES", i.MaxTagPages, 1, MaxImageIntelTagPages},
		{"IMAGE_INTEL_MAX_ATTEMPTS", i.MaxAttempts, 1, MaxImageIntelAttempts},
	} {
		if b.value < b.min || b.value > b.max {
			errs = append(errs, fmt.Errorf("%s%s must be between %d and %d",
				envPrefix, b.name, b.min, b.max))
		}
	}

	return errs
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
