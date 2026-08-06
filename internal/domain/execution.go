package domain

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Container recreation.
//
// # What an execution is
//
// An operator looked at a change plan, downloaded the image it proposed, saw
// that the download was verified, and asked HarborMaster to move the container
// onto it. An execution is the record of that one act.
//
// This is HarborMaster's first CONTAINER mutation. Phase 8 could add an image
// to the local store, which changes nothing that is running. This stops a
// container, creates a replacement from its own configuration, starts it,
// proves it healthy, and only then removes what it replaced.
//
// # What an execution is NOT
//
// It is not an update system. Nothing here runs on a schedule, nothing acts on
// more than one container, and nothing rolls back. There is no automatic
// retry: a recreation that fails stops and waits for a person.
//
// # The single-use rule
//
// An execution names an ACQUISITION, and a succeeded acquisition may be
// executed exactly once. There is no override. A second recreation of the same
// container needs a fresh plan, assessed against the world as it is now, and a
// fresh acquisition to prove the image is still there. Reusing an old approval
// is the failure mode this rule exists to make impossible.
//
// # It fails closed, and it fails LOUDLY
//
// Every failure after the mutation point leaves BOTH containers on the host --
// the parked original and the quarantined replacement -- and records a manual
// recovery plan. HarborMaster does not undo its own work, because an automatic
// undo is another unattended mutation and this phase has exactly one.

// ExecutionState is where one recreation has got to.
//
// A strict, forward-only lifecycle. The states before creating change nothing
// on the host; the states from creating onward describe a container that is
// being replaced.
type ExecutionState string

// Execution states.
const (
	// ExecutionQueued means accepted and waiting for a worker slot. Nothing has
	// been checked and nothing on the host has changed.
	ExecutionQueued ExecutionState = "queued"
	// ExecutionValidating means the preflight is running: the acquisition, the
	// plan, the container, the snapshot, the policy evaluation, and the local
	// image are all being re-read and re-checked.
	ExecutionValidating ExecutionState = "validating"
	// ExecutionCapturing means the container's live configuration is being read
	// so the replacement can reproduce it. Still a read: nothing has changed.
	ExecutionCapturing ExecutionState = "capturing"

	// ExecutionCreating is THE MUTATION POINT. The original is stopped and
	// parked and the replacement is created. Everything before this state is
	// reversible by doing nothing; nothing after it is.
	ExecutionCreating ExecutionState = "creating"
	// ExecutionStarting means the replacement has been created and is being
	// started.
	ExecutionStarting ExecutionState = "starting"
	// ExecutionVerifying means the replacement is running and is being proved:
	// health or stability, image digest, configuration preservation, and
	// network attachment. A recreation that reaches here has not yet succeeded.
	ExecutionVerifying ExecutionState = "verifying"

	// ExecutionSucceeded means every verification passed, the success was
	// recorded durably, and the parked original was removed. All four, in that
	// order.
	ExecutionSucceeded ExecutionState = "succeeded"
	// ExecutionFailed means the recreation stopped for a reason that is not an
	// operator's decision. Failure is the DEFAULT outcome of anything
	// unexpected.
	ExecutionFailed ExecutionState = "failed"
	// ExecutionCancelled means an operator stopped it BEFORE the mutation
	// point. Cancellation is impossible afterwards, so this state always means
	// the host is untouched.
	ExecutionCancelled ExecutionState = "cancelled"
	// ExecutionExpired means the request sat queued past its deadline and was
	// abandoned unstarted.
	ExecutionExpired ExecutionState = "expired"
)

// ExecutionStates lists every state in lifecycle order.
var ExecutionStates = []ExecutionState{
	ExecutionQueued, ExecutionValidating, ExecutionCapturing,
	ExecutionCreating, ExecutionStarting, ExecutionVerifying,
	ExecutionSucceeded, ExecutionFailed, ExecutionCancelled, ExecutionExpired,
}

// ValidExecutionState reports whether name is a known state.
func ValidExecutionState(name string) bool {
	for _, state := range ExecutionStates {
		if string(state) == name {
			return true
		}
	}
	return false
}

// Active reports whether the recreation is still in progress.
func (s ExecutionState) Active() bool {
	switch s {
	case ExecutionQueued, ExecutionValidating, ExecutionCapturing,
		ExecutionCreating, ExecutionStarting, ExecutionVerifying:
		return true
	default:
		return false
	}
}

// Terminal reports whether the recreation has finished.
func (s ExecutionState) Terminal() bool { return !s.Active() }

// Cancellable reports whether an operator may still stop this recreation.
//
// Only BEFORE the mutation point. Once the original has been stopped and a
// replacement created, abandoning the operation partway would leave a container
// in a state nobody chose -- so the only safe direction is forwards, through
// verification, to a recorded conclusion.
func (s ExecutionState) Cancellable() bool {
	switch s {
	case ExecutionQueued, ExecutionValidating, ExecutionCapturing:
		return true
	default:
		return false
	}
}

// Mutating reports whether this state implies the host has been changed.
func (s ExecutionState) Mutating() bool {
	switch s {
	case ExecutionCreating, ExecutionStarting, ExecutionVerifying:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------- checkpointing --

// ExecutionCheckpoint records the last Docker mutation that is known to have
// COMPLETED and to have been recorded durably.
//
// # Why a checkpoint rather than a state
//
// The state says what HarborMaster was doing. The checkpoint says what is true
// of the host. They are different questions, and after a crash only the second
// one matters: a process that died in `creating` may have created nothing, or
// may have stopped the original, parked it, and created the replacement.
//
// The checkpoint is written AFTER each mutation succeeds and BEFORE the next
// one is attempted. So a recovered execution knows the last thing that is
// certainly done, and the next mutation is at worst attempted once more --
// never a mutation that was already known to have completed.
//
// # The uncertain case
//
// If the checkpoint write itself fails, HarborMaster does not know whether the
// mutation is recorded. It does not retry the mutation. It fails the execution
// with ExecutionFailurePersistence and records a recovery plan, because
// repeating a stop, a rename, or a remove against a host whose state is unknown
// is exactly how a recoverable situation becomes an unrecoverable one.
type ExecutionCheckpoint string

// Checkpoints, in the order the pipeline reaches them.
const (
	// CheckpointNone means nothing on the host has been changed.
	CheckpointNone ExecutionCheckpoint = ""
	// CheckpointOriginalStopped means the original container is stopped and
	// still carries its own name.
	CheckpointOriginalStopped ExecutionCheckpoint = "originalStopped"
	// CheckpointOriginalParked means the original is stopped and renamed out of
	// the way, so the replacement can take its name.
	CheckpointOriginalParked ExecutionCheckpoint = "originalParked"
	// CheckpointReplacementCreated means the replacement exists and has not
	// been started.
	CheckpointReplacementCreated ExecutionCheckpoint = "replacementCreated"
	// CheckpointReplacementStarted means the replacement is running and has not
	// yet been proved.
	CheckpointReplacementStarted ExecutionCheckpoint = "replacementStarted"
	// CheckpointReplacementVerified means health or stability, image digest,
	// configuration preservation, and network attachment have ALL passed.
	CheckpointReplacementVerified ExecutionCheckpoint = "replacementVerified"
	// CheckpointReplacementQuarantined means a failed replacement was stopped
	// and renamed aside for diagnosis.
	CheckpointReplacementQuarantined ExecutionCheckpoint = "replacementQuarantined"
	// CheckpointOriginalRemoved means the parked original is gone. Reached only
	// after the success was recorded durably.
	CheckpointOriginalRemoved ExecutionCheckpoint = "originalRemoved"
)

// ExecutionCheckpoints lists every checkpoint.
var ExecutionCheckpoints = []ExecutionCheckpoint{
	CheckpointOriginalStopped, CheckpointOriginalParked,
	CheckpointReplacementCreated, CheckpointReplacementStarted,
	CheckpointReplacementVerified, CheckpointReplacementQuarantined,
	CheckpointOriginalRemoved,
}

// ValidExecutionCheckpoint reports whether name is a known checkpoint.
func ValidExecutionCheckpoint(name string) bool {
	if name == "" {
		return true
	}
	for _, checkpoint := range ExecutionCheckpoints {
		if string(checkpoint) == name {
			return true
		}
	}
	return false
}

// HostChanged reports whether this checkpoint means the host was modified.
func (c ExecutionCheckpoint) HostChanged() bool { return c != CheckpointNone }

// Explain renders the checkpoint as a statement about the host.
//
// A fixed phrase per checkpoint. Never assembled from a daemon string: this
// text reaches a log, a database column, an API response, and a browser.
func (c ExecutionCheckpoint) Explain() string {
	switch c {
	case CheckpointOriginalStopped:
		return "the original container was stopped and still carries its own name"
	case CheckpointOriginalParked:
		return "the original container was stopped and renamed aside"
	case CheckpointReplacementCreated:
		return "the replacement container was created and not started"
	case CheckpointReplacementStarted:
		return "the replacement container was started and not yet proved"
	case CheckpointReplacementVerified:
		return "the replacement container passed every verification"
	case CheckpointReplacementQuarantined:
		return "the replacement container was stopped and renamed aside for diagnosis"
	case CheckpointOriginalRemoved:
		return "the original container was removed and the recreation is complete"
	default:
		return "nothing on this host has been changed"
	}
}

// ------------------------------------------------------------- failures --

// ExecutionFailure classifies why a recreation did not succeed.
//
// A closed vocabulary. The classification decides what an operator is told,
// whether the situation needs a person, and what the recovery plan says. The
// message is never a daemon string.
type ExecutionFailure string

// Failure classifications.
const (
	ExecutionFailureNone ExecutionFailure = ""

	// ExecutionFailurePreflight means the request did not survive revalidation.
	// Nothing on the host was touched: preflight runs entirely before the
	// mutation point.
	ExecutionFailurePreflight ExecutionFailure = "preflight"
	// ExecutionFailureCapture means the container's live configuration could
	// not be read, so there was nothing to reproduce. Nothing was touched.
	ExecutionFailureCapture ExecutionFailure = "capture"

	// ExecutionFailureStop means the original could not be stopped. Nothing was
	// created; the original may be stopped or may still be running, and the
	// checkpoint says which.
	ExecutionFailureStop ExecutionFailure = "stop"
	// ExecutionFailureRename means a rename did not take. The most awkward
	// point to fail: the name an operator expects may be held by neither
	// container, which is why the recovery plan names both by id.
	ExecutionFailureRename ExecutionFailure = "rename"
	// ExecutionFailureCreate means the replacement could not be created. The
	// original is parked and intact.
	ExecutionFailureCreate ExecutionFailure = "create"
	// ExecutionFailureStart means the replacement was created but would not
	// start.
	ExecutionFailureStart ExecutionFailure = "start"

	// ExecutionFailureHealthTimeout means the replacement never reached a
	// healthy state within its budget.
	ExecutionFailureHealthTimeout ExecutionFailure = "healthTimeout"
	// ExecutionFailureUnhealthy means the replacement reported unhealthy. Not
	// waited out: an explicit unhealthy verdict is an answer, and waiting for
	// the timeout would delay the operator without adding information.
	ExecutionFailureUnhealthy ExecutionFailure = "unhealthy"
	// ExecutionFailureNotStable means a container with no health check did not
	// stay running for its stability period.
	ExecutionFailureNotStable ExecutionFailure = "notStable"

	// ExecutionFailureImageMismatch means the running replacement is not on the
	// image that was acquired and approved.
	ExecutionFailureImageMismatch ExecutionFailure = "imageMismatch"
	// ExecutionFailurePreservation means the replacement's configuration does
	// not match what was captured. The most serious verification failure: it
	// means the container that is now running is not the one that was
	// described.
	ExecutionFailurePreservation ExecutionFailure = "preservation"
	// ExecutionFailureNetwork means an expected network attachment is missing.
	// Separated from preservation because a container on the wrong networks is
	// reachable by the wrong things, which is a security outcome rather than a
	// fidelity one.
	ExecutionFailureNetwork ExecutionFailure = "network"

	// ExecutionFailureSecretUnavailable means a required configuration value
	// could not be reproduced because HarborMaster does not hold it. Raised
	// before the mutation point.
	ExecutionFailureSecretUnavailable ExecutionFailure = "secretUnavailable"

	// ExecutionFailureDockerUnavailable means the daemon stopped answering.
	ExecutionFailureDockerUnavailable ExecutionFailure = "dockerUnavailable"
	// ExecutionFailureTimeout means the recreation exceeded its overall budget.
	ExecutionFailureTimeout ExecutionFailure = "timeout"

	// ExecutionFailureInterrupted means HarborMaster restarted mid-recreation.
	// The checkpoint says how far it had got; the recovery plan says what to do
	// about it.
	ExecutionFailureInterrupted ExecutionFailure = "interrupted"
	// ExecutionFailurePersistence means a checkpoint could not be recorded, so
	// HarborMaster no longer knows whether its own last action is durable.
	//
	// The pipeline STOPS here rather than retrying. Repeating a stop, a rename,
	// or a remove against a host whose recorded state is uncertain is how a
	// recoverable situation becomes an unrecoverable one.
	ExecutionFailurePersistence ExecutionFailure = "persistence"

	// ExecutionFailureInternal means HarborMaster itself failed.
	ExecutionFailureInternal ExecutionFailure = "internal"
)

// ExecutionFailures lists every classification.
var ExecutionFailures = []ExecutionFailure{
	ExecutionFailurePreflight, ExecutionFailureCapture,
	ExecutionFailureStop, ExecutionFailureRename,
	ExecutionFailureCreate, ExecutionFailureStart,
	ExecutionFailureHealthTimeout, ExecutionFailureUnhealthy,
	ExecutionFailureNotStable, ExecutionFailureImageMismatch,
	ExecutionFailurePreservation, ExecutionFailureNetwork,
	ExecutionFailureSecretUnavailable, ExecutionFailureDockerUnavailable,
	ExecutionFailureTimeout, ExecutionFailureInterrupted,
	ExecutionFailurePersistence, ExecutionFailureInternal,
}

// ValidExecutionFailure reports whether name is a known classification.
func ValidExecutionFailure(name string) bool {
	if name == "" {
		return true
	}
	for _, failure := range ExecutionFailures {
		if string(failure) == name {
			return true
		}
	}
	return false
}

// NeedsOperator reports whether this failure left the host in a state a person
// has to settle.
//
// The dividing line is the mutation point. A preflight refusal changed nothing
// and needs no cleanup; anything from the stop onward leaves containers that
// only an operator can reconcile, because HarborMaster does not roll back.
func (f ExecutionFailure) NeedsOperator() bool {
	switch f {
	case ExecutionFailureNone, ExecutionFailurePreflight,
		ExecutionFailureCapture, ExecutionFailureSecretUnavailable:
		return false
	default:
		return true
	}
}

// Explain renders a failure classification in HarborMaster's own words.
func (f ExecutionFailure) Explain() string {
	switch f {
	case ExecutionFailurePreflight:
		return "the safety checks refused this recreation; nothing on this host was changed"
	case ExecutionFailureCapture:
		return "the container's current configuration could not be read, so there was nothing to reproduce; nothing was changed"
	case ExecutionFailureStop:
		return "the original container could not be stopped"
	case ExecutionFailureRename:
		return "a container could not be renamed, so the names on this host may not be the ones you expect"
	case ExecutionFailureCreate:
		return "the replacement container could not be created; the original is stopped and preserved"
	case ExecutionFailureStart:
		return "the replacement container was created but would not start"
	case ExecutionFailureHealthTimeout:
		return "the replacement container did not become healthy within its time budget"
	case ExecutionFailureUnhealthy:
		return "the replacement container reported unhealthy"
	case ExecutionFailureNotStable:
		return "the replacement container did not stay running long enough to be considered stable"
	case ExecutionFailureImageMismatch:
		return "the running replacement is not on the image that was approved and acquired"
	case ExecutionFailurePreservation:
		return "the replacement container's configuration does not match what was captured from the original"
	case ExecutionFailureNetwork:
		return "the replacement container is not attached to every network the original was on"
	case ExecutionFailureSecretUnavailable:
		return "a value this container needs could not be reproduced, so the recreation was refused before anything was changed"
	case ExecutionFailureDockerUnavailable:
		return "the Docker daemon stopped answering partway through"
	case ExecutionFailureTimeout:
		return "the recreation exceeded its time budget"
	case ExecutionFailureInterrupted:
		return "HarborMaster restarted while this recreation was in progress, so its outcome was never confirmed"
	case ExecutionFailurePersistence:
		return "HarborMaster could not record what it had just done, so it stopped rather than act again on an uncertain record"
	case ExecutionFailureInternal:
		return "HarborMaster could not complete the recreation"
	default:
		return "the recreation did not succeed"
	}
}

// -------------------------------------------------------------- refusals --

// ExecutionRefusal is why a preflight check refused to proceed.
//
// A refusal means the safety model WORKED. Every one of these is a reason the
// world is not what the operator's approval assumed it was.
type ExecutionRefusal string

// Preflight refusals.
const (
	ExecutionRefusalNone ExecutionRefusal = ""

	// ExecutionRefusalDisabled means container recreation is switched off.
	ExecutionRefusalDisabled ExecutionRefusal = "disabled"

	// ExecutionRefusalAcquisitionMissing means the acquisition no longer exists.
	ExecutionRefusalAcquisitionMissing ExecutionRefusal = "acquisitionMissing"
	// ExecutionRefusalAcquisitionNotSucceeded means the acquisition did not
	// finish successfully, so there is no image known to be present.
	ExecutionRefusalAcquisitionNotSucceeded ExecutionRefusal = "acquisitionNotSucceeded"
	// ExecutionRefusalAcquisitionStale means the acquisition is older than the
	// configured window. An old download does not establish that the image is
	// still on the host, or that the assessment behind it still holds.
	ExecutionRefusalAcquisitionStale ExecutionRefusal = "acquisitionStale"
	// ExecutionRefusalAcquisitionConsumed means this acquisition has already
	// been executed.
	//
	// SINGLE USE, with no override. A second recreation needs a fresh plan and
	// a fresh acquisition, so that what is applied has been assessed against
	// the world as it is now.
	ExecutionRefusalAcquisitionConsumed ExecutionRefusal = "acquisitionConsumed"

	// ExecutionRefusalPlanMissing means the change plan no longer exists.
	ExecutionRefusalPlanMissing ExecutionRefusal = "planMissing"
	// ExecutionRefusalPlanSuperseded means a newer assessment exists.
	ExecutionRefusalPlanSuperseded ExecutionRefusal = "planSuperseded"
	// ExecutionRefusalPlanChanged means the acquisition's approved plan
	// fingerprint no longer matches the plan's own.
	ExecutionRefusalPlanChanged ExecutionRefusal = "planChanged"
	// ExecutionRefusalRecommendation means the plan does not recommend the
	// change. "unknown" refuses alongside "not recommended": a gap in evidence
	// is not permission.
	ExecutionRefusalRecommendation ExecutionRefusal = "recommendation"

	// ExecutionRefusalContainerMissing means the container is no longer present.
	ExecutionRefusalContainerMissing ExecutionRefusal = "containerMissing"
	// ExecutionRefusalContainerChanged means the container is not running the
	// image the plan assessed, so something else has already changed it.
	ExecutionRefusalContainerChanged ExecutionRefusal = "containerChanged"
	// ExecutionRefusalContainerState means the container is in a state a
	// recreation cannot safely start from -- removing, restarting, dead.
	ExecutionRefusalContainerState ExecutionRefusal = "containerState"

	// ExecutionRefusalInventoryStale means HarborMaster's view of the host is
	// older than the configured window, so it would be acting on a stale
	// picture.
	ExecutionRefusalInventoryStale ExecutionRefusal = "inventoryStale"
	// ExecutionRefusalSnapshotMissing means the container has no immutable
	// snapshot to refer back to.
	ExecutionRefusalSnapshotMissing ExecutionRefusal = "snapshotMissing"
	// ExecutionRefusalRestoreReadiness means the snapshot cannot serve as a
	// reference point under the documented policy.
	ExecutionRefusalRestoreReadiness ExecutionRefusal = "restoreReadiness"
	// ExecutionRefusalPolicyViolation means the container has an unresolved
	// critical policy violation.
	ExecutionRefusalPolicyViolation ExecutionRefusal = "policyViolation"
	// ExecutionRefusalPolicyStale means the policy evaluation is missing or too
	// old to establish compliance.
	ExecutionRefusalPolicyStale ExecutionRefusal = "policyStale"
	// ExecutionRefusalRegistryStale means the registry evidence behind the plan
	// is missing or too old.
	ExecutionRefusalRegistryStale ExecutionRefusal = "registryStale"

	// ExecutionRefusalImageMissing means the acquired image is not present on
	// the host any more.
	ExecutionRefusalImageMissing ExecutionRefusal = "imageMissing"
	// ExecutionRefusalDigestMismatch means the local image at that reference no
	// longer carries the approved digest.
	ExecutionRefusalDigestMismatch ExecutionRefusal = "digestMismatch"
	// ExecutionRefusalPlatformMismatch means the local image does not target
	// this host's platform.
	ExecutionRefusalPlatformMismatch ExecutionRefusal = "platformMismatch"

	// ExecutionRefusalConflict means another recreation is already running for
	// this container.
	ExecutionRefusalConflict ExecutionRefusal = "conflict"
	// ExecutionRefusalLimit means the configured concurrency limit would be
	// exceeded.
	ExecutionRefusalLimit ExecutionRefusal = "limit"
	// ExecutionRefusalDockerUnavailable means the daemon is not answering.
	ExecutionRefusalDockerUnavailable ExecutionRefusal = "dockerUnavailable"
	// ExecutionRefusalSecretUnavailable means a required value cannot be
	// reproduced.
	ExecutionRefusalSecretUnavailable ExecutionRefusal = "secretUnavailable"
	// ExecutionRefusalNameUnavailable means the parked or quarantine name this
	// execution would need is already taken on the host.
	ExecutionRefusalNameUnavailable ExecutionRefusal = "nameUnavailable"
)

// ExecutionRefusals lists every refusal.
var ExecutionRefusals = []ExecutionRefusal{
	ExecutionRefusalDisabled,
	ExecutionRefusalAcquisitionMissing, ExecutionRefusalAcquisitionNotSucceeded,
	ExecutionRefusalAcquisitionStale, ExecutionRefusalAcquisitionConsumed,
	ExecutionRefusalPlanMissing, ExecutionRefusalPlanSuperseded,
	ExecutionRefusalPlanChanged, ExecutionRefusalRecommendation,
	ExecutionRefusalContainerMissing, ExecutionRefusalContainerChanged,
	ExecutionRefusalContainerState, ExecutionRefusalInventoryStale,
	ExecutionRefusalSnapshotMissing, ExecutionRefusalRestoreReadiness,
	ExecutionRefusalPolicyViolation, ExecutionRefusalPolicyStale,
	ExecutionRefusalRegistryStale, ExecutionRefusalImageMissing,
	ExecutionRefusalDigestMismatch, ExecutionRefusalPlatformMismatch,
	ExecutionRefusalConflict, ExecutionRefusalLimit,
	ExecutionRefusalDockerUnavailable, ExecutionRefusalSecretUnavailable,
	ExecutionRefusalNameUnavailable,
}

// ValidExecutionRefusal reports whether name is a known refusal.
func ValidExecutionRefusal(name string) bool {
	if name == "" {
		return true
	}
	for _, refusal := range ExecutionRefusals {
		if string(refusal) == name {
			return true
		}
	}
	return false
}

// Explain renders the refusal in HarborMaster's own words.
//
// A fixed phrase per refusal, never assembled from a daemon error, a registry
// response, or caller input.
func (r ExecutionRefusal) Explain() string {
	switch r {
	case ExecutionRefusalDisabled:
		return "container recreation is switched off in this deployment"
	case ExecutionRefusalAcquisitionMissing:
		return "the image acquisition record no longer exists, so nothing establishes that this image was downloaded"
	case ExecutionRefusalAcquisitionNotSucceeded:
		return "that acquisition did not succeed, so there is no verified image to move this container onto"
	case ExecutionRefusalAcquisitionStale:
		return "that acquisition is too old to act on; download the image again so its presence is freshly established"
	case ExecutionRefusalAcquisitionConsumed:
		return "that acquisition has already been used for a recreation; generate a fresh plan and acquire the image again"
	case ExecutionRefusalPlanMissing:
		return "the change plan no longer exists, so there is nothing that approved this change"
	case ExecutionRefusalPlanSuperseded:
		return "a newer assessment exists for this container, so the approved one is out of date"
	case ExecutionRefusalPlanChanged:
		return "the plan has changed since the image was acquired, so the approval no longer describes this change"
	case ExecutionRefusalRecommendation:
		return "the plan does not recommend this change; a container is only recreated when the assessment supports it"
	case ExecutionRefusalContainerMissing:
		return "the container this plan describes is no longer present"
	case ExecutionRefusalContainerChanged:
		return "the container is no longer running the image the plan assessed, so something else has changed it"
	case ExecutionRefusalContainerState:
		return "the container is in a state a recreation cannot safely start from"
	case ExecutionRefusalInventoryStale:
		return "HarborMaster's view of this host is too old to act on; refresh the inventory first"
	case ExecutionRefusalSnapshotMissing:
		return "this container has no configuration snapshot, so there would be no record of what it looked like beforehand"
	case ExecutionRefusalRestoreReadiness:
		return "this container has no usable configuration snapshot to refer back to"
	case ExecutionRefusalPolicyViolation:
		return "this container has an unresolved critical policy violation, which should be settled first"
	case ExecutionRefusalPolicyStale:
		return "the policy evaluation for this container is missing or too old to establish compliance"
	case ExecutionRefusalRegistryStale:
		return "the registry information behind this plan is missing or too old to act on"
	case ExecutionRefusalImageMissing:
		return "the acquired image is no longer present on this host"
	case ExecutionRefusalDigestMismatch:
		return "the image on this host no longer carries the digest that was approved"
	case ExecutionRefusalPlatformMismatch:
		return "the acquired image does not target this host's platform, so it could not run here"
	case ExecutionRefusalConflict:
		return "another recreation is already running for this container"
	case ExecutionRefusalLimit:
		return "too many recreations are already running; try again once one finishes"
	case ExecutionRefusalDockerUnavailable:
		return "the Docker daemon is not answering, so nothing can be established about this host"
	case ExecutionRefusalSecretUnavailable:
		return "this container's configuration includes a value HarborMaster cannot reproduce, so the recreation is refused"
	case ExecutionRefusalNameUnavailable:
		return "a container name this recreation needs is already in use on this host"
	default:
		return "the preflight checks did not pass"
	}
}

// -------------------------------------------------------------- the record --

// ExecutionTarget is the immutable image a recreation moves a container onto.
//
// Derived entirely from the acquisition, which derived it from a plan. No field
// here is ever filled from caller input.
type ExecutionTarget struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	// Digest is the manifest digest. Always present on a legal target.
	Digest string `json:"digest"`
	// Reference is the familiar form for display, e.g. "nginx:1.27.1".
	Reference string `json:"reference"`
	// ImageID is the local image the acquisition verified. The container is
	// created from the DIGEST-PINNED reference, not from this; it is recorded so
	// an operator can tie the record to what inspection found.
	ImageID  string   `json:"imageId,omitempty"`
	Platform Platform `json:"platform,omitempty"`
}

// PinnedReference renders the digest-pinned form used to create the container.
//
// The container is created from this, never from a tag. A tag is a name that
// can move between approval and application; a digest is content.
func (t ExecutionTarget) PinnedReference() string {
	if t.Registry == "" || t.Repository == "" || t.Digest == "" {
		return ""
	}
	return t.Registry + "/" + t.Repository + "@" + t.Digest
}

// Valid reports whether the target names one immutable image.
func (t ExecutionTarget) Valid() bool {
	return ContactableRegistryHost(t.Registry) &&
		t.Repository != "" && len(t.Repository) <= MaxRepositoryBytes &&
		ValidImageDigest(t.Digest)
}

// Execution is one immutable record of one container recreation.
//
// Identity, target, and approval are written once. The lifecycle fields advance
// forwards and stop. A second recreation is a second row: history is never
// overwritten.
type Execution struct {
	// ID is the internal row id. Not part of the API contract.
	ID int64 `json:"-"`
	// ExecutionID is the IMMUTABLE public identifier, generated server-side.
	ExecutionID string `json:"executionId"`

	// The evidence chain, recorded even after the referenced rows are pruned so
	// the audit record stands on its own.
	AcquisitionID string `json:"acquisitionId"`
	PlanID        string `json:"planId"`
	SnapshotID    int64  `json:"snapshotId,omitempty"`

	// ContainerID is the ORIGINAL container. ContainerName is the name the
	// replacement takes over.
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`

	// OldImage is what the container was running, and Target what it is being
	// moved onto.
	OldImage       string          `json:"oldImage"`
	OldImageID     string          `json:"oldImageId,omitempty"`
	OldImageDigest string          `json:"oldImageDigest,omitempty"`
	Target         ExecutionTarget `json:"target"`

	State ExecutionState `json:"state"`
	// Checkpoint is the last mutation known to have completed AND to have been
	// recorded. It, not the state, is what says what is true of the host.
	Checkpoint ExecutionCheckpoint `json:"checkpoint,omitempty"`

	Failure ExecutionFailure `json:"failure,omitempty"`
	Refusal ExecutionRefusal `json:"refusal,omitempty"`
	// Message is HarborMaster's own sentence about the outcome. NEVER a daemon
	// or registry string.
	Message string `json:"message,omitempty"`

	// ReplacementID is the container that was created, and ParkedName /
	// QuarantineName the names the original and a failed replacement were moved
	// to. All three are what an operator needs to find things afterwards.
	ReplacementID   string `json:"replacementId,omitempty"`
	ParkedName      string `json:"parkedName,omitempty"`
	QuarantineName  string `json:"quarantineName,omitempty"`
	OriginalRemoved bool   `json:"originalRemoved"`

	// Verification records what each proof concluded. Absent means the proof
	// was never reached, which is deliberately distinct from having failed.
	Verification ExecutionVerification `json:"verification"`

	// Recovery is the manual recovery plan, present when a failure left
	// containers an operator has to settle. HarborMaster never executes it.
	Recovery *RecoveryPlan `json:"recovery,omitempty"`

	RequestedAt time.Time  `json:"requestedAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	// MutatedAt is when the host was first changed. Nil means it never was,
	// which is the single most useful fact on a failed record.
	MutatedAt   *time.Time `json:"mutatedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	ExpiresAt   time.Time  `json:"expiresAt"`

	// RequestKey is the idempotency key, when the caller supplied one.
	RequestKey string `json:"requestKey,omitempty"`

	// RequestedBy is the account that asked for this recreation.
	//
	// Carried on the record because the OUTCOME is audited by a worker with no
	// request and no session -- see migration 0012. Absent on records written
	// before HarborMaster recorded requesters.
	RequestedBy Requester `json:"requestedBy,omitzero"`
	// PlanDigest is the plan's input fingerprint AS APPROVED at acquisition
	// time. Preflight compares it against the plan's current fingerprint.
	PlanDigest string `json:"planDigest"`
}

// Duration reports how long the recreation took, when it has finished.
func (e Execution) Duration() time.Duration {
	if e.StartedAt == nil || e.CompletedAt == nil {
		return 0
	}
	return e.CompletedAt.Sub(*e.StartedAt)
}

// HostChanged reports whether this execution modified the host.
func (e Execution) HostChanged() bool { return e.Checkpoint.HostChanged() }

// NeedsOperator reports whether a person has to settle the host state.
func (e Execution) NeedsOperator() bool {
	return e.State == ExecutionFailed && e.Failure.NeedsOperator()
}

// ExecutionVerification is what each proof concluded.
//
// Every field is a tri-state: unknown means the proof was never reached. That
// is deliberately distinct from having failed -- a check that could not be
// PERFORMED establishes nothing, and reporting it as a pass or a fail would be
// asserting something nobody knows.
type ExecutionVerification struct {
	// Health is the verdict of the health or stability wait.
	Health VerificationResult `json:"health"`
	// HealthState is the container health the wait observed, when it has one.
	HealthState HealthState `json:"healthState,omitempty"`
	// HealthChecked reports whether the container declares a health check at
	// all. False means Health reflects the stability window instead.
	HealthChecked bool `json:"healthChecked"`
	// StabilitySeconds is how long the replacement was required to stay running
	// when it has no health check.
	StabilitySeconds int `json:"stabilitySeconds,omitempty"`

	// Image is whether the running replacement carries the approved digest.
	Image VerificationResult `json:"image"`
	// Preservation is whether the replacement's configuration matches what was
	// captured, and Report the field-by-field detail.
	Preservation VerificationResult  `json:"preservation"`
	Report       *PreservationReport `json:"preservationReport,omitempty"`
	// Network is whether every expected attachment is present.
	Network VerificationResult `json:"network"`
}

// Passed reports whether every proof concluded successfully.
//
// An unknown is NOT a pass. This is the fail-closed rule stated in one place:
// the parked original is removed only when this returns true.
func (v ExecutionVerification) Passed() bool {
	return v.Health == VerificationPassed &&
		v.Image == VerificationPassed &&
		v.Preservation == VerificationPassed &&
		v.Network == VerificationPassed
}

// VerificationResult is the outcome of one proof.
type VerificationResult string

// Verification results.
const (
	// VerificationUnknown means the proof was never reached or could not be
	// performed. Never treated as a pass.
	VerificationUnknown VerificationResult = "unknown"
	VerificationPassed  VerificationResult = "passed"
	VerificationFailed  VerificationResult = "failed"
)

// ValidVerificationResult reports whether name is a known result.
func ValidVerificationResult(name string) bool {
	switch VerificationResult(name) {
	case VerificationUnknown, VerificationPassed, VerificationFailed, "":
		return true
	default:
		return false
	}
}

// ExecutionEvent is one bounded entry in a recreation's audit trail.
type ExecutionEvent struct {
	ID          int64          `json:"-"`
	ExecutionID string         `json:"-"`
	State       ExecutionState `json:"state"`
	// Checkpoint is what was true of the host at this point.
	Checkpoint ExecutionCheckpoint `json:"checkpoint,omitempty"`
	// Detail is a short, sanitised note in HarborMaster's own words.
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// ExecutionSummary is the dashboard aggregate.
type ExecutionSummary struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	// NeedsAttention counts failures that left containers on the host for an
	// operator to settle. The number that matters most on this page.
	NeedsAttention int `json:"needsAttention"`

	ByState   map[ExecutionState]int   `json:"byState"`
	ByFailure map[ExecutionFailure]int `json:"byFailure"`

	LastCompletedAt *time.Time `json:"lastCompletedAt,omitempty"`
	// Enabled reports whether recreation is switched on at all, so an empty
	// list is not read as "nothing has ever been recreated".
	Enabled bool `json:"enabled"`
}

// MaxExecutionMessageBytes bounds an operator-facing message.
const MaxExecutionMessageBytes = 400

// ExecutionIDPrefix is the fixed prefix of a generated execution id.
const ExecutionIDPrefix = "exec_"

// ExecutionIDHexLength is how many hex characters follow the prefix.
const ExecutionIDHexLength = 20

// NewExecutionID generates an immutable public identifier.
//
// Random rather than sequential, for the same reason a plan id is: it appears
// in URLs and in CONTAINER NAMES on the host, and a sequential one would leak
// how many recreations have happened and invite a caller to walk them.
//
// Panics only if the system entropy source fails, which on every supported
// platform means the process cannot safely continue anyway.
func NewExecutionID() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("harbormaster: system entropy source unavailable: " + err.Error())
	}
	return ExecutionIDPrefix + hex.EncodeToString(raw[:])
}

// ValidExecutionID reports whether id has the shape of a generated id.
//
// Validated by SHAPE wherever it is read back, because it becomes part of a
// container name on the host as well as part of a URL.
func ValidExecutionID(id string) bool {
	if len(id) != len(ExecutionIDPrefix)+ExecutionIDHexLength {
		return false
	}
	if id[:len(ExecutionIDPrefix)] != ExecutionIDPrefix {
		return false
	}
	for _, r := range id[len(ExecutionIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
