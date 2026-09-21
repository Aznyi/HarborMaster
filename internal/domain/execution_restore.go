package domain

import "strings"

// Automatic restoration of a failed update.
//
// # What it is
//
// A recreation that fails after the mutation point leaves the original parked
// and stopped. For an unattended update the automation follower asks the
// rollback service to put it back, when the policy allows. For a MANUAL update
// the execution service now asks the same service, through the same request,
// unless the deployment switched that off -- because "a person asked for the
// update" is not a reason to leave their service down when the preserved
// original can be put back safely.
//
// # What it is not
//
// It is not a second rollback implementation. The execution service submits a
// RollbackRequest and records what came of it; the rollback service runs its
// own preflight against the live host and may refuse. Nothing here moves a
// container.
//
// # The outcome is recorded on the update
//
// An operator reading a failed update needs one answer: is the service back?
// The rollback record says what the ROLLBACK did; this says what it meant for
// the update, in a closed vocabulary the schema enforces.

// ExecutionRestoreState is where the automatic restoration of one failed
// update has got to.
type ExecutionRestoreState string

// Restore states.
const (
	// RestoreRequested means the rollback was accepted and is running. The
	// service may be down or already back; the rollback record says which.
	RestoreRequested ExecutionRestoreState = "requested"
	// RestoreRestored means the rollback succeeded: the original is serving
	// under its own name and passed verification.
	RestoreRestored ExecutionRestoreState = "restored"
	// RestoreFailed means the rollback was accepted and did not complete. The
	// service is still down and a person is needed now.
	RestoreFailed ExecutionRestoreState = "failed"
	// RestoreRefused means the rollback preflight said no: the host is not in
	// an arrangement HarborMaster can safely undo. A person is needed now.
	RestoreRefused ExecutionRestoreState = "refused"
	// RestoreUnavailable means the deployment holds no rollback capability, so
	// nothing could be asked. A person is needed now.
	RestoreUnavailable ExecutionRestoreState = "unavailable"
)

// ExecutionRestoreStates lists every state. The empty string is the absence
// of a restore: the update did not fail after the mutation point, or it is an
// unattended update whose recovery the automation follower owns.
var ExecutionRestoreStates = []ExecutionRestoreState{
	RestoreRequested, RestoreRestored, RestoreFailed, RestoreRefused, RestoreUnavailable,
}

// ValidExecutionRestoreState reports whether name is a known state or empty.
func ValidExecutionRestoreState(name string) bool {
	if name == "" {
		return true
	}
	for _, state := range ExecutionRestoreStates {
		if string(state) == name {
			return true
		}
	}
	return false
}

// ServiceRestored reports whether the original is serving again.
func (s ExecutionRestoreState) ServiceRestored() bool { return s == RestoreRestored }

// Settled reports whether the restore reached a conclusion, good or bad.
func (s ExecutionRestoreState) Settled() bool {
	switch s {
	case RestoreRestored, RestoreFailed, RestoreRefused, RestoreUnavailable:
		return true
	default:
		return false
	}
}

// ExecutionRestore is the outcome of restoring a failed update, as recorded on
// the update itself.
type ExecutionRestore struct {
	State ExecutionRestoreState `json:"state,omitempty"`
	// RollbackID names the rollback that performed, or is performing, the
	// restore. Empty when none was accepted.
	RollbackID string `json:"rollbackId,omitempty"`
	// Detail is HarborMaster's own sentence about a refusal, a failure, or an
	// absent capability. Never a daemon string.
	Detail string `json:"detail,omitempty"`
}

// ManualRestoreRequestKeyPrefix marks a rollback the execution service
// requested on behalf of a failed manual update.
//
// The same shape as AutomationRequestKeyPrefix, and for the same reason: the
// requester on the record is the operator who asked for the UPDATE, carried
// forward for the audit trail, and the key is what says nobody asked for the
// rollback itself. Like that prefix, it decides wording and idempotency and
// nothing else -- no gate, no capability, no mutation reads it.
const ManualRestoreRequestKeyPrefix = "manual-restore:"

// ManualRestoreRequestKey is the idempotency key for restoring one update.
//
// One key per execution, so however many times the request is made -- from
// the failing pipeline, from the restart recovery pass, from a sweep -- the
// rollback service creates at most one rollback for it.
func ManualRestoreRequestKey(executionID string) string {
	return ManualRestoreRequestKeyPrefix + executionID
}

// ManualRestoreRequest reports whether a request key marks a manual restore.
func ManualRestoreRequest(requestKey string) bool {
	return strings.HasPrefix(requestKey, ManualRestoreRequestKeyPrefix)
}
