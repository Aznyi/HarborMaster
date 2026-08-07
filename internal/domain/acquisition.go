package domain

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Image acquisition.
//
// # What an acquisition is
//
// An operator looked at a change plan, decided the proposed image was worth
// having on the host, and asked HarborMaster to download it. That is the whole
// of it: layers arrive in the daemon's local image store.
//
// # What an acquisition is NOT
//
// It is not an update. **No container is touched.** A container keeps running
// the image it was created from, identified by image ID; a newly pulled image
// is another entry in the store beside it. Nothing here stops, starts,
// recreates, or reconfigures anything, and HarborMaster has no capability to.
//
// This distinction is the single most important thing the UI has to convey, and
// the reason the model carries no "applied" state: there is nothing to apply.
//
// # Why a plan is required
//
// A pull is HarborMaster's first privileged write, so it is deliberately not
// something an operator can aim freely. An acquisition names a CHANGE PLAN, and
// the plan supplies the registry, the repository, and the digest. There is no
// field anywhere in this model that accepts a registry URL, a repository, or a
// pull option from a caller.
//
// The plan is then revalidated immediately before the pull rather than trusted
// from when it was written -- see AcquisitionRefusal for what that check can
// conclude. The gap between "a plan said this was reasonable" and "we are about
// to download it" is a time-of-check/time-of-use window, and closing it is what
// the preflight exists for.

// AcquisitionState is where one acquisition has got to.
//
// A strict lifecycle. Every transition is recorded, and the terminal states are
// terminal: nothing moves out of succeeded, failed, cancelled, or expired.
type AcquisitionState string

// Acquisition states.
const (
	// AcquisitionQueued means accepted and waiting for a worker slot. The
	// request has been recorded but nothing has been checked yet.
	AcquisitionQueued AcquisitionState = "queued"
	// AcquisitionValidating means the preflight is running: the plan and every
	// piece of supporting evidence are being re-read and re-checked.
	AcquisitionValidating AcquisitionState = "validating"
	// AcquisitionPulling means the daemon is transferring layers. This is the
	// only state in which HarborMaster is changing the host.
	AcquisitionPulling AcquisitionState = "pulling"
	// AcquisitionVerifying means the transfer finished and the local image is
	// being re-inspected to confirm it is what was asked for. A pull that
	// reaches here has not yet succeeded.
	AcquisitionVerifying AcquisitionState = "verifying"

	// AcquisitionSucceeded means the image is present locally and its digest
	// and platform were confirmed by read-only inspection.
	AcquisitionSucceeded AcquisitionState = "succeeded"
	// AcquisitionFailed means the acquisition stopped for a reason that is not
	// an operator's decision. Failure is the DEFAULT outcome of anything
	// unexpected: this feature fails closed.
	AcquisitionFailed AcquisitionState = "failed"
	// AcquisitionCancelled means an operator asked for it to stop, or a
	// shutdown interrupted it. Distinguished from failed because a cancellation
	// is a decision rather than a fault.
	AcquisitionCancelled AcquisitionState = "cancelled"
	// AcquisitionExpired means the request sat queued for longer than its
	// deadline and was abandoned unstarted.
	//
	// A separate state, deliberately. An expired request was never validated
	// and never pulled, and the evidence behind it is now old enough that
	// running it later would be acting on a stale approval.
	AcquisitionExpired AcquisitionState = "expired"
)

// AcquisitionStates lists every state in lifecycle order.
var AcquisitionStates = []AcquisitionState{
	AcquisitionQueued, AcquisitionValidating, AcquisitionPulling,
	AcquisitionVerifying, AcquisitionSucceeded, AcquisitionFailed,
	AcquisitionCancelled, AcquisitionExpired,
}

// ValidAcquisitionState reports whether name is a known state.
func ValidAcquisitionState(name string) bool {
	for _, state := range AcquisitionStates {
		if string(state) == name {
			return true
		}
	}
	return false
}

// Active reports whether the acquisition is still in progress.
//
// Used by the duplicate-work check and by cancellation. The set is defined
// here, once, so the database constraint, the service, and the UI cannot
// disagree about what "active" means.
func (s AcquisitionState) Active() bool {
	switch s {
	case AcquisitionQueued, AcquisitionValidating, AcquisitionPulling, AcquisitionVerifying:
		return true
	default:
		return false
	}
}

// Terminal reports whether the acquisition has finished.
func (s AcquisitionState) Terminal() bool { return !s.Active() }

// Cancellable reports whether an operator may still stop this acquisition.
//
// Verifying is deliberately excluded: the transfer has already happened, the
// bytes are already on the host, and stopping the confirmation step would leave
// an unverified image with no record saying so. Once a pull completes, the only
// safe direction is forwards through verification.
func (s AcquisitionState) Cancellable() bool {
	switch s {
	case AcquisitionQueued, AcquisitionValidating, AcquisitionPulling:
		return true
	default:
		return false
	}
}

// AcquisitionFailure classifies why an acquisition did not succeed.
//
// A closed vocabulary rather than a message, because the classification decides
// whether a retry could ever help and drives what the UI says. The message an
// operator reads is built from this, never from a daemon or registry string.
type AcquisitionFailure string

// Failure classifications.
const (
	AcquisitionFailureNone AcquisitionFailure = ""

	// AcquisitionFailurePreflight means the request did not survive
	// revalidation. The world moved between planning and pulling, and this is
	// the outcome the whole safety model exists to produce.
	AcquisitionFailurePreflight AcquisitionFailure = "preflight"
	// AcquisitionFailureDockerUnavailable means the daemon did not answer.
	// Transient by nature.
	AcquisitionFailureDockerUnavailable AcquisitionFailure = "dockerUnavailable"
	// AcquisitionFailureRegistry means the registry refused or could not serve
	// the image -- not found, or requiring credentials HarborMaster does not
	// hold.
	AcquisitionFailureRegistry AcquisitionFailure = "registry"
	// AcquisitionFailureTransfer means the transfer started and did not
	// complete. Retryable.
	AcquisitionFailureTransfer AcquisitionFailure = "transfer"
	// AcquisitionFailureTimeout means the operation exceeded its budget.
	AcquisitionFailureTimeout AcquisitionFailure = "timeout"

	// AcquisitionFailureDigestMismatch means the image now present locally is
	// NOT the one that was approved.
	//
	// The most serious classification in this list, and never retried
	// automatically. It means either a daemon that resolved the reference
	// differently than expected or a registry serving different content, and
	// both warrant a person looking rather than another attempt.
	AcquisitionFailureDigestMismatch AcquisitionFailure = "digestMismatch"
	// AcquisitionFailurePlatformMismatch means the acquired image does not
	// target the platform the container runs on. It would not run here.
	AcquisitionFailurePlatformMismatch AcquisitionFailure = "platformMismatch"
	// AcquisitionFailureVerification means the acquired image could not be
	// inspected at all, so nothing about it was established. Distinct from a
	// mismatch: a failed check is not a failed comparison.
	AcquisitionFailureVerification AcquisitionFailure = "verification"

	// AcquisitionFailureInternal means HarborMaster itself failed -- a database
	// error, an unexpected condition.
	AcquisitionFailureInternal AcquisitionFailure = "internal"
)

// AcquisitionFailures lists every classification.
var AcquisitionFailures = []AcquisitionFailure{
	AcquisitionFailurePreflight, AcquisitionFailureDockerUnavailable,
	AcquisitionFailureRegistry, AcquisitionFailureTransfer,
	AcquisitionFailureTimeout, AcquisitionFailureDigestMismatch,
	AcquisitionFailurePlatformMismatch, AcquisitionFailureVerification,
	AcquisitionFailureInternal,
}

// ValidAcquisitionFailure reports whether name is a known classification.
func ValidAcquisitionFailure(name string) bool {
	if name == "" {
		return true
	}
	for _, failure := range AcquisitionFailures {
		if string(failure) == name {
			return true
		}
	}
	return false
}

// Retryable reports whether another attempt could plausibly succeed.
//
// Advisory only: HarborMaster never retries an acquisition by itself, because
// an automatic retry is an automatic pull and this phase has none. It tells the
// UI whether to suggest that an operator try again.
//
// A digest or platform mismatch is deliberately NOT retryable. Repeating a pull
// that produced the wrong content is how a transient-looking substitution
// becomes a persistent one.
func (f AcquisitionFailure) Retryable() bool {
	switch f {
	case AcquisitionFailureDockerUnavailable, AcquisitionFailureTransfer,
		AcquisitionFailureTimeout:
		return true
	default:
		return false
	}
}

// AcquisitionRefusal is why a preflight check refused to proceed.
//
// A closed vocabulary, and the reason each one exists is that the alternative
// is downloading something nobody approved. These are not errors in the usual
// sense: a refusal means the safety model WORKED.
type AcquisitionRefusal string

// Preflight refusals.
const (
	AcquisitionRefusalNone AcquisitionRefusal = ""

	// AcquisitionRefusalPlanMissing means the plan no longer exists.
	AcquisitionRefusalPlanMissing AcquisitionRefusal = "planMissing"
	// AcquisitionRefusalPlanSuperseded means a newer plan exists for the
	// container, so the operator approved an assessment that has been replaced.
	AcquisitionRefusalPlanSuperseded AcquisitionRefusal = "planSuperseded"
	// AcquisitionRefusalPlanStale means the plan's inputs no longer produce its
	// fingerprint. The evidence behind the approval has moved.
	//
	// This is the TOCTOU check, and it is exact rather than heuristic: the
	// planner's fingerprint is recomputed from current data and compared.
	AcquisitionRefusalPlanStale AcquisitionRefusal = "planStale"
	// AcquisitionRefusalRecommendation means the plan does not recommend the
	// change. Both "not recommended" and "unknown" refuse: a gap in evidence is
	// not permission.
	AcquisitionRefusalRecommendation AcquisitionRefusal = "recommendation"
	// AcquisitionRefusalDigestUnavailable means the plan carries no proposed
	// digest, so there is no immutable target to pull.
	AcquisitionRefusalDigestUnavailable AcquisitionRefusal = "digestUnavailable"
	// AcquisitionRefusalDigestChanged means the digest on offer now differs
	// from the one the operator approved.
	AcquisitionRefusalDigestChanged AcquisitionRefusal = "digestChanged"
	// AcquisitionRefusalPlatformUnavailable means the image does not publish
	// the platform this host runs.
	AcquisitionRefusalPlatformUnavailable AcquisitionRefusal = "platformUnavailable"
	// AcquisitionRefusalRestoreReadiness means the container's snapshot cannot
	// serve as a reference point, under the documented policy.
	AcquisitionRefusalRestoreReadiness AcquisitionRefusal = "restoreReadiness"
	// AcquisitionRefusalPolicyViolation means the container has an unresolved
	// critical policy violation.
	AcquisitionRefusalPolicyViolation AcquisitionRefusal = "policyViolation"
	// AcquisitionRefusalRegistryStale means the registry evidence is missing,
	// never collected, or older than the configured freshness window.
	AcquisitionRefusalRegistryStale AcquisitionRefusal = "registryStale"
	// AcquisitionRefusalDuplicate means another acquisition for the same target
	// is already in flight.
	AcquisitionRefusalDuplicate AcquisitionRefusal = "duplicate"
	// AcquisitionRefusalDockerUnavailable means the daemon is not answering, so
	// nothing can be established about the host.
	AcquisitionRefusalDockerUnavailable AcquisitionRefusal = "dockerUnavailable"
	// AcquisitionRefusalLimit means a configured concurrency or size limit
	// would be exceeded.
	AcquisitionRefusalLimit AcquisitionRefusal = "limit"
	// AcquisitionRefusalTargetRefused means the assembled target is not a legal
	// pull target. A last-line check; reaching it means an earlier layer let
	// something through.
	AcquisitionRefusalTargetRefused AcquisitionRefusal = "targetRefused"
	// AcquisitionRefusalDisabled means image acquisition is switched off.
	AcquisitionRefusalDisabled AcquisitionRefusal = "disabled"
	// AcquisitionRefusalContainerMissing means the container the plan describes
	// is no longer present.
	AcquisitionRefusalContainerMissing AcquisitionRefusal = "containerMissing"

	// AcquisitionRefusalSelfUpdate means the plan names the container
	// HarborMaster is running in.
	//
	// Refused at the ACQUISITION rather than only at the recreation, even
	// though a download changes no container. Downloading the image is the
	// first half of an operation whose second half cannot complete, and
	// allowing it would leave an operator with a succeeded acquisition that can
	// never be spent -- which reads as a working feature right up to the moment
	// it is not.
	AcquisitionRefusalSelfUpdate AcquisitionRefusal = "selfUpdate"
)

// AcquisitionRefusals lists every refusal.
var AcquisitionRefusals = []AcquisitionRefusal{
	AcquisitionRefusalPlanMissing, AcquisitionRefusalPlanSuperseded,
	AcquisitionRefusalPlanStale, AcquisitionRefusalRecommendation,
	AcquisitionRefusalDigestUnavailable, AcquisitionRefusalDigestChanged,
	AcquisitionRefusalPlatformUnavailable, AcquisitionRefusalRestoreReadiness,
	AcquisitionRefusalPolicyViolation, AcquisitionRefusalRegistryStale,
	AcquisitionRefusalDuplicate, AcquisitionRefusalDockerUnavailable,
	AcquisitionRefusalLimit, AcquisitionRefusalTargetRefused,
	AcquisitionRefusalDisabled, AcquisitionRefusalContainerMissing,
	AcquisitionRefusalSelfUpdate,
}

// ValidAcquisitionRefusal reports whether name is a known refusal.
func ValidAcquisitionRefusal(name string) bool {
	if name == "" {
		return true
	}
	for _, refusal := range AcquisitionRefusals {
		if string(refusal) == name {
			return true
		}
	}
	return false
}

// Explain renders the refusal in HarborMaster's own words.
//
// A fixed phrase per refusal. Never assembled from a daemon error, a registry
// response, or caller input -- this string reaches a log, a database column,
// an API response, and a browser.
func (r AcquisitionRefusal) Explain() string {
	switch r {
	case AcquisitionRefusalSelfUpdate:
		return "this plan names the container HarborMaster is running in; HarborMaster is updated from outside itself"
	case AcquisitionRefusalPlanMissing:
		return "the change plan no longer exists, so there is nothing that approved this image"
	case AcquisitionRefusalPlanSuperseded:
		return "a newer assessment exists for this container, so the approved one is out of date"
	case AcquisitionRefusalPlanStale:
		return "the evidence behind the plan has changed since it was written, so the approval no longer describes the current situation"
	case AcquisitionRefusalRecommendation:
		return "the plan does not recommend this change; an image is only acquired when the assessment supports it"
	case AcquisitionRefusalDigestUnavailable:
		return "the plan names no proposed digest, so there is no immutable image to acquire"
	case AcquisitionRefusalDigestChanged:
		return "the digest on offer has changed since the plan was written, so this is no longer the image that was approved"
	case AcquisitionRefusalPlatformUnavailable:
		return "the image does not publish the platform this host runs, so it could not run here"
	case AcquisitionRefusalRestoreReadiness:
		return "this container has no usable configuration snapshot to refer back to"
	case AcquisitionRefusalPolicyViolation:
		return "this container has an unresolved critical policy violation, which should be settled first"
	case AcquisitionRefusalRegistryStale:
		return "the registry information for this image is missing or too old to act on"
	case AcquisitionRefusalDuplicate:
		return "an acquisition for this image is already in progress"
	case AcquisitionRefusalDockerUnavailable:
		return "the Docker daemon is not answering, so nothing can be established about this host"
	case AcquisitionRefusalLimit:
		return "too many acquisitions are already running; try again once one finishes"
	case AcquisitionRefusalTargetRefused:
		return "the resolved image reference is not a legal acquisition target"
	case AcquisitionRefusalDisabled:
		return "image acquisition is switched off in this deployment"
	case AcquisitionRefusalContainerMissing:
		return "the container this plan describes is no longer present"
	default:
		return "the preflight checks did not pass"
	}
}

// Explain renders a failure classification in HarborMaster's own words.
func (f AcquisitionFailure) Explain() string {
	switch f {
	case AcquisitionFailurePreflight:
		return "the safety checks refused this acquisition"
	case AcquisitionFailureDockerUnavailable:
		return "the Docker daemon did not answer"
	case AcquisitionFailureRegistry:
		return "the registry did not serve this image"
	case AcquisitionFailureTransfer:
		return "the transfer did not complete"
	case AcquisitionFailureTimeout:
		return "the acquisition exceeded its time budget"
	case AcquisitionFailureDigestMismatch:
		return "the acquired image is not the one that was approved; nothing has been changed, and this warrants a look before trying again"
	case AcquisitionFailurePlatformMismatch:
		return "the acquired image does not target this host's platform"
	case AcquisitionFailureVerification:
		return "the acquired image could not be inspected, so nothing about it was confirmed"
	case AcquisitionFailureInternal:
		return "HarborMaster could not complete the acquisition"
	default:
		return "the acquisition did not succeed"
	}
}

// AcquisitionTarget is the immutable image an acquisition names.
//
// Derived entirely from a change plan. There is no constructor that takes
// caller text, and the API request type carries none of these fields.
type AcquisitionTarget struct {
	// Registry is the host the image lives on, e.g. "docker.io".
	Registry string `json:"registry"`
	// Repository is the path within it, e.g. "library/nginx".
	Repository string `json:"repository"`
	// Digest is the manifest digest. Always present on a legal target.
	Digest string `json:"digest"`
	// Reference is the familiar form for display, e.g. "nginx:1.27.1". Shown to
	// an operator; never used to perform the pull.
	Reference string `json:"reference"`
	// Platform is what the image must target to be usable on this host.
	Platform Platform `json:"platform,omitempty"`
}

// PinnedReference renders the digest-pinned form an operator should see in a
// confirmation.
//
// Deliberately the full, unambiguous spelling. A confirmation dialog that said
// "nginx:1.27.1" would be asking someone to approve a name; this asks them to
// approve content.
func (t AcquisitionTarget) PinnedReference() string {
	if t.Registry == "" || t.Repository == "" || t.Digest == "" {
		return ""
	}
	return t.Registry + "/" + t.Repository + "@" + t.Digest
}

// Valid reports whether the target names one immutable image.
func (t AcquisitionTarget) Valid() bool {
	return ContactableRegistryHost(t.Registry) &&
		t.Repository != "" && len(t.Repository) <= MaxRepositoryBytes &&
		ValidImageDigest(t.Digest)
}

// Acquisition is one immutable record of an attempt to acquire an image.
//
// Append-only in spirit: the row's identity, target, and approval never change.
// The lifecycle fields advance forwards through the states above and stop.
type Acquisition struct {
	// ID is the internal row id. Not part of the API contract.
	ID int64 `json:"-"`
	// AcquisitionID is the IMMUTABLE public identifier, generated server-side.
	AcquisitionID string `json:"acquisitionId"`

	// PlanID is the change plan that approved this image, and ContainerID the
	// container it was assessed for. Both recorded even after the plan is
	// pruned, so the audit record stands on its own.
	PlanID        string `json:"planId"`
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`

	Target AcquisitionTarget `json:"target"`

	State AcquisitionState `json:"state"`

	// Failure classifies a non-success outcome, and Message is HarborMaster's
	// own sentence about it. Message is NEVER a daemon or registry string.
	Failure AcquisitionFailure `json:"failure,omitempty"`
	Message string             `json:"message,omitempty"`
	// Refusal is set when the preflight refused, and is the specific check that
	// said no.
	Refusal AcquisitionRefusal `json:"refusal,omitempty"`

	// AcquiredImageID and AcquiredDigest are what verification actually found
	// on the host, rather than what was expected. On a mismatch they are the
	// evidence.
	AcquiredImageID string `json:"acquiredImageId,omitempty"`
	AcquiredDigest  string `json:"acquiredDigest,omitempty"`
	// AcquiredPlatform is the platform of the image that arrived.
	AcquiredPlatform Platform `json:"acquiredPlatform,omitempty"`
	// SizeBytes is the local image size reported by inspection. Authoritative,
	// unlike the transfer's own progress counters.
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// Layers and BytesTransferred summarise the transfer. Estimates for
	// display: layers already present locally are never counted.
	Layers           int   `json:"layers,omitempty"`
	BytesTransferred int64 `json:"bytesTransferred,omitempty"`

	// Progress is the most recent bounded status line, e.g. "Downloading".
	Progress string `json:"progress,omitempty"`

	RequestedAt time.Time  `json:"requestedAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	// ExpiresAt is when a still-queued request is abandoned.
	ExpiresAt time.Time `json:"expiresAt"`

	// RequestKey is the idempotency key, when the caller supplied one.
	RequestKey string `json:"requestKey,omitempty"`

	// RequestedBy is the account that asked for this download.
	//
	// Carried on the record because the OUTCOME is audited by a worker with no
	// request and no session -- see migration 0012. Absent on records written
	// before HarborMaster recorded requesters.
	RequestedBy Requester `json:"requestedBy,omitzero"`

	// PlanDigest is the plan's input fingerprint AS APPROVED. Preflight
	// recomputes the plan's fingerprint and compares against this, which is how
	// "the evidence has not moved" is established exactly rather than by
	// timestamp.
	PlanDigest string `json:"planDigest"`
}

// Duration reports how long the acquisition took, when it has finished.
func (a Acquisition) Duration() time.Duration {
	if a.StartedAt == nil || a.CompletedAt == nil {
		return 0
	}
	return a.CompletedAt.Sub(*a.StartedAt)
}

// AcquisitionEvent is one bounded entry in an acquisition's audit trail.
//
// The trail records STATE TRANSITIONS and periodic progress, both bounded. It
// is not a copy of the daemon's stream: that stream is unbounded and
// registry-influenced, and persisting it would make one operator action an
// unbounded write.
type AcquisitionEvent struct {
	ID            int64            `json:"-"`
	AcquisitionID string           `json:"-"`
	State         AcquisitionState `json:"state"`
	// Detail is a short, sanitised note. HarborMaster's own words, or a
	// sanitised progress status from the daemon.
	Detail string `json:"detail,omitempty"`
	// BytesTransferred and Layers are the counters at this point.
	BytesTransferred int64     `json:"bytesTransferred,omitempty"`
	Layers           int       `json:"layers,omitempty"`
	At               time.Time `json:"at"`
}

// AcquisitionSummary is the dashboard aggregate.
type AcquisitionSummary struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`

	ByState   map[AcquisitionState]int   `json:"byState"`
	ByFailure map[AcquisitionFailure]int `json:"byFailure"`

	LastCompletedAt *time.Time `json:"lastCompletedAt,omitempty"`
	// Enabled reports whether acquisition is switched on at all, so an empty
	// list is not read as "nothing has ever been acquired".
	Enabled bool `json:"enabled"`
}

// MaxAcquisitionMessageBytes bounds an operator-facing message.
const MaxAcquisitionMessageBytes = 300

// MaxAcquisitionProgressBytes bounds a stored progress line.
const MaxAcquisitionProgressBytes = 64

// NewAcquisitionID generates an immutable public identifier.
//
// Random rather than sequential, for the same reason a plan id is: it appears
// in URLs, and a sequential one would leak how many acquisitions exist and
// invite a caller to walk them.
//
// Panics only if the system entropy source fails, which on every supported
// platform means the process cannot safely continue anyway.
func NewAcquisitionID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return "acq_" + hex.EncodeToString(raw[:])
}
