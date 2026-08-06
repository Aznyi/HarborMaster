package domain

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Manual rollback: returning a container to the state a recreation moved it
// from.
//
// # What a rollback is
//
// A recreation stopped a container, renamed it aside, and started a replacement
// in its place. A rollback stops the replacement, renames it aside, renames the
// preserved original back to the name it had, starts it, and proves it.
//
// It is the inverse of the second half of a recreation, and NOTHING ELSE. It is
// not a restore: it cannot rebuild a container from a snapshot, cannot create a
// container, and cannot invent configuration. It moves containers that are
// already on the host, by exact id, back to an arrangement HarborMaster itself
// recorded.
//
// # What a rollback is NOT
//
// It is not automatic. Phase 9 deliberately refused to undo its own work,
// because an automatic undo is an unattended mutation performed at exactly the
// moment HarborMaster has demonstrated its model of the host is wrong. That
// judgement has not changed: a rollback happens because a person looked at a
// failed recreation, read the recovery plan, and decided.
//
// It is not a retry, a schedule, or a fleet operation. One rollback acts on one
// execution's two containers.
//
// It cannot target anything an operator names. The request carries an EXECUTION
// ID. Every container id, every name, and the image identity all come from the
// execution record HarborMaster wrote itself, and every one of them is
// re-verified against the live host before anything moves.
//
// # It preserves the failed replacement
//
// There is no remove capability in the rollback interface at all. The
// replacement that failed is the evidence of WHY the recreation failed, and a
// rollback that could delete it would be a rollback that destroys the reason it
// was needed. It is stopped and renamed aside; removing it is an operator's
// decision with their own tooling.
//
// # It fails closed, and it never guesses
//
// Every failure after the mutation point leaves BOTH containers on the host and
// records a manual recovery plan naming each by id. HarborMaster does not
// decide which container should serve traffic when it cannot prove the answer.

// ---------------------------------------------------------------- identity --

// Rollback identifier shape. Same construction as every other public id here:
// a fixed prefix and hex from the system entropy source, so an identifier
// carries no information and cannot be guessed.
const (
	RollbackIDPrefix    = "rbk_"
	RollbackIDHexLength = 20
)

// NewRollbackID generates a public rollback identifier.
func NewRollbackID() string {
	var raw [RollbackIDHexLength / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Unreachable in practice: crypto/rand does not fail on any supported
		// platform. Panicking is right anyway -- an identifier that is not
		// unpredictable would let one operator guess another's rollback id.
		panic("rollback id: system entropy source unavailable")
	}
	return RollbackIDPrefix + hex.EncodeToString(raw[:])
}

// ValidRollbackID reports whether an identifier has the generated shape.
//
// Checked before an id reaches a query or a derived container name. A caller
// supplies one on the detail and cancel routes, so it is untrusted text until
// this says otherwise.
func ValidRollbackID(id string) bool {
	if len(id) != len(RollbackIDPrefix)+RollbackIDHexLength {
		return false
	}
	if id[:len(RollbackIDPrefix)] != RollbackIDPrefix {
		return false
	}
	for _, r := range id[len(RollbackIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------- the states --

// RollbackState is where one rollback has got to.
//
// Forward-only. The states before stoppingReplacement change nothing on the
// host; every state from it onward describes containers that are being moved.
type RollbackState string

// Rollback states.
const (
	// RollbackQueued means accepted and waiting for a worker slot. Nothing has
	// been re-checked and nothing on the host has changed.
	RollbackQueued RollbackState = "queued"
	// RollbackValidating means the preflight is running: the execution record,
	// both container identities, the inventory freshness, and the original's
	// configuration are all being re-read against the live host.
	RollbackValidating RollbackState = "validating"

	// RollbackStoppingReplacement is THE MUTATION POINT. Everything before it is
	// reversible by doing nothing; nothing after it is.
	RollbackStoppingReplacement RollbackState = "stoppingReplacement"
	// RollbackRestoringName means the replacement has been parked and the
	// original is being renamed back to the name it had.
	RollbackRestoringName RollbackState = "restoringName"
	// RollbackStartingOriginal means the original carries its own name again and
	// is being started.
	RollbackStartingOriginal RollbackState = "startingOriginal"
	// RollbackVerifyingOriginal means the original is running and is being
	// proved: health or stability, image identity, and configuration.
	RollbackVerifyingOriginal RollbackState = "verifyingOriginal"

	// RollbackSucceeded means every proof passed and the success was recorded
	// durably. The replacement is still on the host, stopped and parked.
	RollbackSucceeded RollbackState = "succeeded"
	// RollbackFailed means the rollback stopped for a reason that is not an
	// operator's decision. Failure is the DEFAULT outcome of anything
	// unexpected.
	RollbackFailed RollbackState = "failed"
	// RollbackCancelled means an operator stopped it BEFORE the mutation point,
	// so it always means the host is untouched.
	RollbackCancelled RollbackState = "cancelled"
	// RollbackExpired means the request sat queued past its deadline.
	RollbackExpired RollbackState = "expired"
)

// RollbackStates lists every state in lifecycle order.
var RollbackStates = []RollbackState{
	RollbackQueued, RollbackValidating,
	RollbackStoppingReplacement, RollbackRestoringName,
	RollbackStartingOriginal, RollbackVerifyingOriginal,
	RollbackSucceeded, RollbackFailed, RollbackCancelled, RollbackExpired,
}

// ValidRollbackState reports whether name is a known state.
func ValidRollbackState(name string) bool {
	for _, state := range RollbackStates {
		if string(state) == name {
			return true
		}
	}
	return false
}

// Active reports whether the rollback is still in progress.
func (s RollbackState) Active() bool {
	switch s {
	case RollbackQueued, RollbackValidating, RollbackStoppingReplacement,
		RollbackRestoringName, RollbackStartingOriginal, RollbackVerifyingOriginal:
		return true
	default:
		return false
	}
}

// Terminal reports whether the rollback has finished.
func (s RollbackState) Terminal() bool { return !s.Active() }

// Cancellable reports whether an operator may still stop this rollback.
//
// Only BEFORE the mutation point, for the same reason a recreation is: once the
// replacement has been stopped, abandoning the operation partway would leave
// the container an operator depends on neither running nor recorded as down.
// The only safe direction is forwards, to a recorded conclusion.
func (s RollbackState) Cancellable() bool {
	switch s {
	case RollbackQueued, RollbackValidating:
		return true
	default:
		return false
	}
}

// Mutating reports whether this state implies the host has been changed.
func (s RollbackState) Mutating() bool {
	switch s {
	case RollbackStoppingReplacement, RollbackRestoringName,
		RollbackStartingOriginal, RollbackVerifyingOriginal:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------- checkpointing --

// RollbackCheckpoint records the last Docker mutation known to have COMPLETED
// and to have been recorded durably.
//
// The state says what HarborMaster was doing; the checkpoint says what is true
// of the host. After a crash only the second question matters. See
// ExecutionCheckpoint, which documents the reasoning at length -- this is the
// same mechanism applied to the inverse operation.
type RollbackCheckpoint string

// Checkpoints, in the order the pipeline reaches them.
const (
	// RollbackCheckpointNone means nothing on the host has been changed.
	RollbackCheckpointNone RollbackCheckpoint = ""
	// RollbackCheckpointReplacementStopped means the replacement is stopped and
	// still carries the production name.
	RollbackCheckpointReplacementStopped RollbackCheckpoint = "replacementStopped"
	// RollbackCheckpointReplacementParked means the replacement is stopped and
	// renamed aside, so the production name is free.
	RollbackCheckpointReplacementParked RollbackCheckpoint = "replacementParked"
	// RollbackCheckpointOriginalRestored means the original carries the
	// production name again and has not been started.
	RollbackCheckpointOriginalRestored RollbackCheckpoint = "originalRestored"
	// RollbackCheckpointOriginalStarted means the original is running and has
	// not yet been proved.
	RollbackCheckpointOriginalStarted RollbackCheckpoint = "originalStarted"
	// RollbackCheckpointOriginalVerified means health or stability, image
	// identity, and configuration have ALL passed.
	RollbackCheckpointOriginalVerified RollbackCheckpoint = "originalVerified"
)

// RollbackCheckpoints lists every checkpoint.
var RollbackCheckpoints = []RollbackCheckpoint{
	RollbackCheckpointReplacementStopped, RollbackCheckpointReplacementParked,
	RollbackCheckpointOriginalRestored, RollbackCheckpointOriginalStarted,
	RollbackCheckpointOriginalVerified,
}

// ValidRollbackCheckpoint reports whether name is a known checkpoint.
func ValidRollbackCheckpoint(name string) bool {
	if name == "" {
		return true
	}
	for _, checkpoint := range RollbackCheckpoints {
		if string(checkpoint) == name {
			return true
		}
	}
	return false
}

// HostChanged reports whether this checkpoint means the host was modified.
func (c RollbackCheckpoint) HostChanged() bool { return c != RollbackCheckpointNone }

// Explain renders the checkpoint as a statement about the host.
//
// A fixed phrase per checkpoint, never assembled from a daemon string: this
// text reaches a log, a column, an API response, and a browser.
func (c RollbackCheckpoint) Explain() string {
	switch c {
	case RollbackCheckpointReplacementStopped:
		return "the replacement container was stopped and still carries the production name"
	case RollbackCheckpointReplacementParked:
		return "the replacement container was stopped and renamed aside"
	case RollbackCheckpointOriginalRestored:
		return "the original container carries the production name again and is not started"
	case RollbackCheckpointOriginalStarted:
		return "the original container was started and not yet proved"
	case RollbackCheckpointOriginalVerified:
		return "the original container passed every verification"
	default:
		return "nothing on this host has been changed"
	}
}

// ---------------------------------------------------------------- failures --

// RollbackFailure classifies why a rollback did not succeed.
//
// A closed vocabulary. The classification decides what an operator is told,
// whether a person has to settle the host, and what the recovery plan says.
type RollbackFailure string

// Failure classifications.
const (
	RollbackFailureNone RollbackFailure = ""

	// RollbackFailurePreflight means the request did not survive revalidation.
	// Nothing on the host was touched.
	RollbackFailurePreflight RollbackFailure = "preflight"

	// RollbackFailureStop means the replacement could not be stopped. The
	// checkpoint says whether it is stopped or still running.
	RollbackFailureStop RollbackFailure = "stop"
	// RollbackFailureRename means a rename did not take. The most awkward point
	// to fail: the production name may be held by neither container, which is
	// why the recovery plan names both by id.
	RollbackFailureRename RollbackFailure = "rename"
	// RollbackFailureStart means the original would not start. The worst
	// ordinary outcome: the replacement is stopped and the original will not
	// run, so nothing is serving and a person is needed immediately.
	RollbackFailureStart RollbackFailure = "start"

	// RollbackFailureHealthTimeout means the original never became healthy
	// within its budget.
	RollbackFailureHealthTimeout RollbackFailure = "healthTimeout"
	// RollbackFailureUnhealthy means the original reported unhealthy. Not
	// waited out: an explicit verdict is an answer.
	RollbackFailureUnhealthy RollbackFailure = "unhealthy"
	// RollbackFailureNotStable means an original with no health check did not
	// stay running for its stability period.
	RollbackFailureNotStable RollbackFailure = "notStable"

	// RollbackFailureImageMismatch means the restored original is not on the
	// image the execution recorded it as running. Something other than
	// HarborMaster changed it.
	RollbackFailureImageMismatch RollbackFailure = "imageMismatch"
	// RollbackFailurePreservation means the original's configuration is not
	// what it was when the rollback began. The most serious verification
	// failure: the container now running is not the one that was validated.
	RollbackFailurePreservation RollbackFailure = "preservation"
	// RollbackFailureNetwork means an expected network attachment is missing,
	// so the restored container is reachable by the wrong things.
	RollbackFailureNetwork RollbackFailure = "network"

	// RollbackFailureDockerUnavailable means the daemon stopped answering.
	RollbackFailureDockerUnavailable RollbackFailure = "dockerUnavailable"
	// RollbackFailureTimeout means the rollback exceeded its overall budget.
	RollbackFailureTimeout RollbackFailure = "timeout"

	// RollbackFailureInterrupted means HarborMaster restarted mid-rollback.
	//
	// Recorded as a FAILURE with this classification rather than as a distinct
	// terminal state, matching how a recreation settles the same condition.
	// Two vocabularies for one concept across two nearly identical features
	// would make the recovery pages disagree about what "interrupted" is.
	RollbackFailureInterrupted RollbackFailure = "interrupted"
	// RollbackFailurePersistence means a checkpoint could not be recorded, so
	// HarborMaster no longer knows whether its own last action is durable.
	//
	// The pipeline STOPS rather than retrying. Repeating a stop, a rename, or a
	// start against a host whose recorded state is uncertain is how a
	// recoverable situation becomes an unrecoverable one.
	RollbackFailurePersistence RollbackFailure = "persistence"
	// RollbackFailureInternal is the catch-all.
	RollbackFailureInternal RollbackFailure = "internal"
)

// RollbackFailures lists every classification.
var RollbackFailures = []RollbackFailure{
	RollbackFailurePreflight,
	RollbackFailureStop, RollbackFailureRename, RollbackFailureStart,
	RollbackFailureHealthTimeout, RollbackFailureUnhealthy, RollbackFailureNotStable,
	RollbackFailureImageMismatch, RollbackFailurePreservation, RollbackFailureNetwork,
	RollbackFailureDockerUnavailable, RollbackFailureTimeout,
	RollbackFailureInterrupted, RollbackFailurePersistence, RollbackFailureInternal,
}

// ValidRollbackFailure reports whether name is a known classification.
func ValidRollbackFailure(name string) bool {
	if name == "" {
		return true
	}
	for _, failure := range RollbackFailures {
		if string(failure) == name {
			return true
		}
	}
	return false
}

// NeedsOperator reports whether this failure left containers a person has to
// settle.
//
// The preflight refusals do not: they happen before the mutation point and the
// host is untouched. Everything else does.
func (f RollbackFailure) NeedsOperator() bool {
	switch f {
	case RollbackFailureNone, RollbackFailurePreflight:
		return false
	default:
		return true
	}
}

// Explain renders the failure in operator-facing words.
//
// Fixed sentences. Never a daemon string: these reach a column, a response, and
// a browser.
func (f RollbackFailure) Explain() string {
	switch f {
	case RollbackFailurePreflight:
		return "the safety checks refused this rollback; nothing on this host was changed"
	case RollbackFailureStop:
		return "the replacement container could not be stopped"
	case RollbackFailureRename:
		return "a container could not be renamed, so the names on this host may not be the ones you expect"
	case RollbackFailureStart:
		return "the original container would not start, so nothing is serving this name"
	case RollbackFailureHealthTimeout:
		return "the restored original did not become healthy within its time budget"
	case RollbackFailureUnhealthy:
		return "the restored original reported unhealthy"
	case RollbackFailureNotStable:
		return "the restored original did not stay running long enough to be considered stable"
	case RollbackFailureImageMismatch:
		return "the restored original is not on the image the recreation recorded it as running"
	case RollbackFailurePreservation:
		return "the restored original's configuration is not what it was when this rollback began"
	case RollbackFailureNetwork:
		return "the restored original is not attached to every network it was on"
	case RollbackFailureDockerUnavailable:
		return "the Docker daemon stopped answering partway through"
	case RollbackFailureTimeout:
		return "the rollback exceeded its time budget"
	case RollbackFailureInterrupted:
		return "HarborMaster restarted while this rollback was in progress, so its outcome was never confirmed"
	case RollbackFailurePersistence:
		return "HarborMaster could not record what it had just done, so it stopped rather than act again on an uncertain record"
	case RollbackFailureInternal:
		return "HarborMaster could not complete the rollback"
	default:
		return "the rollback did not succeed"
	}
}

// -------------------------------------------------------------- refusals --

// RollbackRefusal is why a preflight check refused to proceed.
//
// A refusal means the safety model WORKED. Every one of these is a reason the
// host is not what the execution record says it should be.
type RollbackRefusal string

// Preflight refusals.
const (
	RollbackRefusalNone RollbackRefusal = ""

	// RollbackRefusalDisabled means rollback is switched off by configuration.
	RollbackRefusalDisabled RollbackRefusal = "disabled"

	// RollbackRefusalExecutionMissing means the execution record is gone.
	RollbackRefusalExecutionMissing RollbackRefusal = "executionMissing"
	// RollbackRefusalExecutionActive means the recreation has not finished. A
	// rollback while the pipeline is still moving containers would have two
	// writers on one host.
	RollbackRefusalExecutionActive RollbackRefusal = "executionActive"
	// RollbackRefusalNothingToRollBack means the recreation never reached the
	// point where the original was parked, so there is no arrangement to undo.
	RollbackRefusalNothingToRollBack RollbackRefusal = "nothingToRollBack"
	// RollbackRefusalOriginalRemoved means the recreation completed and removed
	// the original. There is nothing to restore, and HarborMaster will not
	// recreate one from a snapshot -- that is a restore, and it is not this.
	RollbackRefusalOriginalRemoved RollbackRefusal = "originalRemoved"
	// RollbackRefusalCheckpointUncertain means the recreation's checkpoint does
	// not establish where its containers are.
	//
	// Refused deliberately. Acting on an uncertain record is how a recoverable
	// situation becomes an unrecoverable one; the operator gets the recovery
	// plan and settles it by hand.
	RollbackRefusalCheckpointUncertain RollbackRefusal = "checkpointUncertain"

	// RollbackRefusalAlreadyRolledBack means this execution has already been
	// rolled back successfully. Single use, with no override.
	RollbackRefusalAlreadyRolledBack RollbackRefusal = "alreadyRolledBack"
	// RollbackRefusalConflict means another rollback or another recreation is
	// active for this container.
	RollbackRefusalConflict RollbackRefusal = "conflict"
	// RollbackRefusalLimit means the configured concurrency limit would be
	// exceeded.
	RollbackRefusalLimit RollbackRefusal = "limit"

	// RollbackRefusalOriginalMissing means the preserved original is not on the
	// host any more. Somebody removed it.
	RollbackRefusalOriginalMissing RollbackRefusal = "originalMissing"
	// RollbackRefusalOriginalIdentity means the container at the recorded id is
	// not the one the execution parked -- a different name, or a different
	// image from the one recorded.
	RollbackRefusalOriginalIdentity RollbackRefusal = "originalIdentity"
	// RollbackRefusalReplacementMissing means the replacement is not on the
	// host. Its absence means somebody has already been rearranging things.
	RollbackRefusalReplacementMissing RollbackRefusal = "replacementMissing"
	// RollbackRefusalReplacementIdentity means the container at the recorded id
	// is not the replacement the execution created.
	RollbackRefusalReplacementIdentity RollbackRefusal = "replacementIdentity"
	// RollbackRefusalNameUnavailable means the production name is held by some
	// third container, so restoring it would collide.
	RollbackRefusalNameUnavailable RollbackRefusal = "nameUnavailable"

	// RollbackRefusalInventoryStale means HarborMaster's view of the host is
	// older than the configured window.
	RollbackRefusalInventoryStale RollbackRefusal = "inventoryStale"
	// RollbackRefusalDockerUnavailable means the daemon is not answering.
	RollbackRefusalDockerUnavailable RollbackRefusal = "dockerUnavailable"
	// RollbackRefusalUnverifiable means the original's configuration could not
	// be projected, so the post-restore comparison would have nothing to check
	// against. A proof that cannot be performed is not a proof.
	RollbackRefusalUnverifiable RollbackRefusal = "unverifiable"
)

// RollbackRefusals lists every refusal.
var RollbackRefusals = []RollbackRefusal{
	RollbackRefusalDisabled,
	RollbackRefusalExecutionMissing, RollbackRefusalExecutionActive,
	RollbackRefusalNothingToRollBack, RollbackRefusalOriginalRemoved,
	RollbackRefusalCheckpointUncertain,
	RollbackRefusalAlreadyRolledBack, RollbackRefusalConflict, RollbackRefusalLimit,
	RollbackRefusalOriginalMissing, RollbackRefusalOriginalIdentity,
	RollbackRefusalReplacementMissing, RollbackRefusalReplacementIdentity,
	RollbackRefusalNameUnavailable,
	RollbackRefusalInventoryStale, RollbackRefusalDockerUnavailable,
	RollbackRefusalUnverifiable,
}

// ValidRollbackRefusal reports whether name is a known refusal.
func ValidRollbackRefusal(name string) bool {
	if name == "" {
		return true
	}
	for _, refusal := range RollbackRefusals {
		if string(refusal) == name {
			return true
		}
	}
	return false
}

// Explain renders the refusal in operator-facing words.
func (r RollbackRefusal) Explain() string {
	switch r {
	case RollbackRefusalDisabled:
		return "rollback is not enabled on this deployment"
	case RollbackRefusalExecutionMissing:
		return "the recreation this rollback refers to no longer exists"
	case RollbackRefusalExecutionActive:
		return "that recreation is still running; a rollback can only follow a finished one"
	case RollbackRefusalNothingToRollBack:
		return "that recreation never reached the point of replacing the container, so there is nothing to undo"
	case RollbackRefusalOriginalRemoved:
		return "that recreation completed and removed the original container; HarborMaster will not recreate one"
	case RollbackRefusalCheckpointUncertain:
		return "that recreation's record does not establish where its containers are, so a rollback would be acting on a guess"
	case RollbackRefusalAlreadyRolledBack:
		return "that recreation has already been rolled back"
	case RollbackRefusalConflict:
		return "another rollback or recreation is already running for this container"
	case RollbackRefusalLimit:
		return "too many rollbacks are already running; try again once one finishes"
	case RollbackRefusalOriginalMissing:
		return "the preserved original container is no longer on this host"
	case RollbackRefusalOriginalIdentity:
		return "the container at the recorded id is not the original this recreation preserved"
	case RollbackRefusalReplacementMissing:
		return "the replacement container is no longer on this host"
	case RollbackRefusalReplacementIdentity:
		return "the container at the recorded id is not the replacement this recreation created"
	case RollbackRefusalNameUnavailable:
		return "the container name this rollback needs is held by some other container"
	case RollbackRefusalInventoryStale:
		return "HarborMaster's view of this host is too old to act on"
	case RollbackRefusalDockerUnavailable:
		return "the Docker daemon is not answering, so nothing can be established about this host"
	case RollbackRefusalUnverifiable:
		return "the original container's configuration could not be read, so the rollback could not be proved afterwards"
	default:
		return "the preflight checks did not pass"
	}
}

// ------------------------------------------------------------ eligibility --

// RollbackEligibility is whether one execution can be rolled back, and why not.
//
// Answered without changing anything, so the UI can render an honest control
// rather than offering a button that will be refused. The service answers the
// same question again, against the live host, immediately before it acts --
// this is advice, not permission.
type RollbackEligibility struct {
	// Eligible reports the verdict.
	Eligible bool `json:"eligible"`
	// Refusal is why not. Empty when eligible.
	Refusal RollbackRefusal `json:"refusal,omitempty"`
	// Reason is the refusal in operator-facing words.
	Reason string `json:"reason,omitempty"`

	// The identities a confirmation dialogue must show. All from the execution
	// record; none from a caller.
	ContainerName  string `json:"containerName,omitempty"`
	OriginalID     string `json:"originalId,omitempty"`
	ParkedName     string `json:"parkedName,omitempty"`
	ReplacementID  string `json:"replacementId,omitempty"`
	ReplacementRef string `json:"replacementImage,omitempty"`
	OriginalRef    string `json:"originalImage,omitempty"`
}

// RollbackSufficientCheckpoint reports whether a recreation got far enough for
// its arrangement to be undoable.
//
// The window is exact. Below originalParked the original still carries its own
// name and no replacement holds it, so there is no arrangement to undo -- and
// starting a stopped container is not a rollback. At originalRemoved the
// original is gone.
//
// The uncertain case is NOT in the window: a checkpoint of none on an execution
// that attempted a mutation means HarborMaster does not know what it did, and a
// rollback would be acting on a guess.
func RollbackSufficientCheckpoint(checkpoint ExecutionCheckpoint) bool {
	switch checkpoint {
	case CheckpointOriginalParked, CheckpointReplacementCreated,
		CheckpointReplacementStarted, CheckpointReplacementVerified,
		CheckpointReplacementQuarantined:
		return true
	default:
		return false
	}
}

// -------------------------------------------------------------- the record --

// Rollback is one immutable record of one manual rollback.
//
// Identity and evidence are written once; the lifecycle fields advance forwards
// and stop. A second rollback is a second row.
type Rollback struct {
	// ID is the internal row id. Not part of the API contract.
	ID int64 `json:"-"`
	// RollbackID is the IMMUTABLE public identifier, generated server-side.
	RollbackID string `json:"rollbackId"`

	// ExecutionID is the recreation being undone. The ONLY thing a caller
	// supplies, and everything below is derived from the record it names.
	ExecutionID string `json:"executionId"`

	// ContainerName is the production name both containers contend for.
	ContainerName string `json:"containerName"`

	// OriginalID is the preserved original and ParkedName the name it is parked
	// under. ReplacementID is the container the recreation created.
	OriginalID    string `json:"originalId"`
	ParkedName    string `json:"parkedName"`
	ReplacementID string `json:"replacementId"`
	// ReplacementParkedName is where the replacement is renamed to so the
	// production name comes free. Empty until the rename happens.
	ReplacementParkedName string `json:"replacementParkedName,omitempty"`

	// OriginalImage and OriginalImageID are what the execution recorded the
	// original as running. Compared against the live container both at
	// preflight and after the restore.
	OriginalImage   string `json:"originalImage,omitempty"`
	OriginalImageID string `json:"originalImageId,omitempty"`
	// ReplacementImage is what the recreation moved the container onto,
	// recorded so the operator sees what is being backed out of.
	ReplacementImage string `json:"replacementImage,omitempty"`

	State RollbackState `json:"state"`
	// Checkpoint is the last mutation known to have completed AND to have been
	// recorded. It, not the state, says what is true of the host.
	Checkpoint RollbackCheckpoint `json:"checkpoint,omitempty"`

	Failure RollbackFailure `json:"failure,omitempty"`
	Refusal RollbackRefusal `json:"refusal,omitempty"`
	// Message is HarborMaster's own sentence about the outcome. NEVER a daemon
	// string.
	Message string `json:"message,omitempty"`

	// Verification records what each proof concluded. Absent means the proof
	// was never reached, which is distinct from having failed.
	Verification RollbackVerification `json:"verification"`

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
	// RequestedBy is the account that asked. Carried on the record because the
	// OUTCOME is audited by a worker with no request and no session.
	RequestedBy Requester `json:"requestedBy,omitzero"`
}

// Duration reports how long the rollback took, when it has finished.
func (r Rollback) Duration() time.Duration {
	if r.StartedAt == nil || r.CompletedAt == nil {
		return 0
	}
	return r.CompletedAt.Sub(*r.StartedAt)
}

// HostChanged reports whether this rollback modified the host.
func (r Rollback) HostChanged() bool { return r.Checkpoint.HostChanged() }

// NeedsOperator reports whether a person has to settle the host state.
func (r Rollback) NeedsOperator() bool {
	return r.State == RollbackFailed && r.Failure.NeedsOperator()
}

// ------------------------------------------------------------ verification --

// RollbackVerification is what each proof concluded about the restored
// original.
//
// Every field is a tri-state: unknown means the proof was never reached. A
// check that could not be PERFORMED establishes nothing, and reporting it as a
// pass or a fail would be asserting something nobody knows.
type RollbackVerification struct {
	// Health is the verdict of the health or stability wait.
	Health VerificationResult `json:"health"`
	// HealthState is the container health the wait observed, when it has one.
	HealthState HealthState `json:"healthState,omitempty"`
	// HealthChecked reports whether the container declares a health check at
	// all. False means Health reflects the stability window instead.
	HealthChecked bool `json:"healthChecked"`
	// StabilitySeconds is how long the original was required to stay running
	// when it has no health check.
	StabilitySeconds int `json:"stabilitySeconds,omitempty"`

	// Image is whether the restored original carries the image identity the
	// execution recorded for it.
	Image VerificationResult `json:"image"`

	// Preservation is whether the original's configuration after the restore
	// matches the projection taken during validation, BEFORE anything moved.
	//
	// That is the property a rollback needs to establish: the original was
	// already in its pre-execution configuration, and the rollback's job is to
	// restore its name and run it, not to change it. Comparing before and after
	// proves the rollback did not.
	Preservation VerificationResult  `json:"preservation"`
	Report       *PreservationReport `json:"preservationReport,omitempty"`

	// Network is whether every attachment the original had before the rollback
	// is present after it.
	Network VerificationResult `json:"network"`
}

// Passed reports whether every proof concluded successfully.
//
// An unknown is NOT a pass. This is the fail-closed rule in one place: a
// rollback is recorded as succeeded only when this returns true.
func (v RollbackVerification) Passed() bool {
	return v.Health == VerificationPassed &&
		v.Image == VerificationPassed &&
		v.Preservation == VerificationPassed &&
		v.Network == VerificationPassed
}

// ------------------------------------------------------------------ events --

// RollbackEvent is one bounded entry in a rollback's audit trail.
type RollbackEvent struct {
	ID         int64         `json:"-"`
	RollbackID string        `json:"-"`
	State      RollbackState `json:"state"`
	// Checkpoint is what was true of the host at this point.
	Checkpoint RollbackCheckpoint `json:"checkpoint,omitempty"`
	// Detail is a short, sanitised note in HarborMaster's own words.
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// RollbackSummary is the dashboard aggregate.
type RollbackSummary struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	// NeedsAttention counts failures that left containers on the host for an
	// operator to settle. The number that matters most on this page.
	NeedsAttention int `json:"needsAttention"`
}

// ------------------------------------------------------------------- names --

// RollbackParkedNameSuffix marks a replacement that a rollback moved aside.
//
// Distinct from QuarantineNameSuffix, which marks a replacement that FAILED its
// own verification. A replacement backed out by a rollback may have been
// perfectly healthy; conflating the two would tell an operator the wrong story
// about why a container is sitting there.
const RollbackParkedNameSuffix = ".hm-rolledback-"

// maxRollbackSuffixBytes is the longest suffix a rollback derivation adds.
const maxRollbackSuffixBytes = len(RollbackParkedNameSuffix) + len(RollbackIDPrefix) + RollbackIDHexLength

// MaxRollbackableNameBytes is the longest production name a rollback can
// handle.
//
// A longer one would produce a parked name past the bound, and silently
// truncating it would break the uniqueness the rollback id provides.
const MaxRollbackableNameBytes = MaxContainerNameBytes - maxRollbackSuffixBytes

// RollbackParkedName derives the name a rolled-back replacement is moved to.
//
// Derived from a container name HarborMaster read from the daemon and a
// rollback id it generated itself, exactly as the recreation names are. There
// is no code path in which caller text becomes a container name.
//
// Returns "" when the input cannot produce a legal name, so a caller that
// forgets to check gets a value the validators reject rather than a truncated
// name that collides with something.
func RollbackParkedName(containerName, rollbackID string) string {
	name := NormaliseContainerName(containerName)
	if name == "" || len(name) > MaxRollbackableNameBytes {
		return ""
	}
	if !ValidRollbackID(rollbackID) {
		return ""
	}

	derived := name + RollbackParkedNameSuffix + rollbackID
	if !ValidContainerName(derived) || len(derived) > MaxContainerNameBytes {
		return ""
	}
	return derived
}

// RollbackableContainerName reports whether a rollback could derive its names
// from this one.
func RollbackableContainerName(name string) bool {
	normalised := NormaliseContainerName(name)
	return ValidContainerName(normalised) && len(normalised) <= MaxRollbackableNameBytes
}
