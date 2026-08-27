package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Snapshot assurance: making sure a container has a baseline, without letting
// that baseline retroactively authorise anything.
//
// # The load-bearing invariant
//
//	The snapshot/readiness evidence used to authorise an update must be part of
//	the immutable evidence of the change plan that is executed.
//
// (Spelled in words rather than in type names throughout this file. The
// architecture guard that forbids this code from reaching a plan is a text
// check, and it cannot tell a mention from a use.)
//
// Automatic updates are unusable if an operator has to snapshot every container
// by hand before HarborMaster may touch it. The obvious fix -- capture a
// snapshot at the moment the pipeline discovers it needs one -- is the one this
// file must not implement. A plan whose risk assessment was computed against
// "no baseline exists" would then be executed as though it had been computed
// against a baseline that did exist. The assessment an operator reviewed would
// not be the assessment that authorised the change, and the whole point of an
// immutable plan is that those two are the same thing.
//
// So assurance CREATES evidence and never REINTERPRETS it. Nothing here writes
// to a change plan, and there is no code path from this file to one.
//
// # Where it is called from, and why those two places
//
//  1. BEFORE A PLANNER PASS, in bulk and bounded. A container with no baseline
//     gets one, and the pass that follows moments later reads it through the
//     planner's own grouped baseline query -- so the plan it writes CONTAINS the
//     evidence rather than needing a second pass to notice it. This is the call
//     that makes first-time automatic updates work at all.
//
//  2. BEFORE A RECREATION, for one container, exactly. Here assurance is not a
//     preparation but a MEASUREMENT: if capturing produces a new snapshot row,
//     the container's configuration has moved since it was assessed, and the
//     plan is stale. The execution preflight then refuses. The new snapshot is
//     kept -- it is the evidence the next plan will be built from -- but it does
//     not bless the plan that was already in flight.
//
// It is deliberately NOT called at acquisition time. The acquisition preflight
// reads the plan's own snapshot evidence, and a plan written without a baseline
// must be replanned rather than topped up. See TestCaseA_* in
// plan_snapshot_evidence_test.go, which pins that refusal and the zero pulls
// behind it.
//
// # This holds no Docker capability, and cannot acquire one
//
// It reaches exactly one collaborator, SnapshotCapturer, which is one method.
// SnapshotService behind it reads HarborMaster's own container repository and
// never a socket -- see the comment on SnapshotContainers. Pinned by
// TestSnapshotAssuranceHoldsNoDockerCapability and by the source guard in
// internal/arch.

// AssuranceOutcome is what one assurance attempt established.
//
// A closed vocabulary, and the zero value is not one of them: an uninitialised
// result must not read as success. Every caller switches on it exhaustively and
// treats anything unrecognised as a refusal.
type AssuranceOutcome string

const (
	// AssuranceCurrent means the container's configuration is unchanged and the
	// existing snapshot already describes it. Nothing was written.
	//
	// The common case on a settled estate, and the one that has to be cheap.
	AssuranceCurrent AssuranceOutcome = "current"

	// AssuranceCaptured means a new snapshot row was written.
	//
	// Read carefully at the two call sites, because it means opposite things.
	// Before a plan it is preparation. Before a recreation it is the discovery
	// that the container changed after it was assessed.
	AssuranceCaptured AssuranceOutcome = "captured"

	// AssuranceNotReady means a baseline exists but has FAILED its restore
	// readiness check, so it cannot serve as a reference point.
	//
	// Distinct from unavailable: HarborMaster looked and found something
	// unusable, rather than being unable to look.
	AssuranceNotReady AssuranceOutcome = "notReady"

	// AssuranceUnavailable means no baseline could be established.
	//
	// Snapshots switched off, a capture already running, a container that could
	// not be read, a write that failed. Every one of them fails CLOSED: a
	// baseline that could not be established is not a baseline.
	AssuranceUnavailable AssuranceOutcome = "unavailable"
)

// Usable reports whether the outcome establishes a baseline that may be relied
// on.
//
// An allowlist rather than a check against the two failures, so an outcome
// added later is unusable until somebody says otherwise.
func (o AssuranceOutcome) Usable() bool {
	switch o {
	case AssuranceCurrent, AssuranceCaptured:
		return true
	default:
		return false
	}
}

// Explain renders an outcome in HarborMaster's own words.
//
// A fixed phrase per outcome. Never assembled from a daemon error, a store
// error, or caller input -- these strings reach an execution record and a
// browser.
func (o AssuranceOutcome) Explain() string {
	switch o {
	case AssuranceCurrent:
		return "the container's existing configuration snapshot still describes it"
	case AssuranceCaptured:
		return "a new configuration snapshot was captured"
	case AssuranceNotReady:
		return "the container's configuration snapshot failed its restore readiness check"
	case AssuranceUnavailable:
		return "HarborMaster could not establish a configuration snapshot for this container"
	default:
		return "the assurance outcome is one this build does not understand"
	}
}

// AssuranceResult is one container's assurance, with the evidence it produced.
//
// SnapshotID is the identity everything downstream compares against. It is the
// row HarborMaster wrote, never a value a caller supplied: there is no field on
// any request type in this package that a snapshot id could be put into.
type AssuranceResult struct {
	Outcome    AssuranceOutcome
	SnapshotID int64
	Readiness  domain.ReadinessStatus
	// Detail is HarborMaster's own sentence about the outcome.
	Detail string
}

// Usable reports whether this result establishes a baseline.
func (r AssuranceResult) Usable() bool { return r.Outcome.Usable() && r.SnapshotID != 0 }

// SnapshotCapturer is the one collaborator assurance has.
//
// Two methods, both already on SnapshotService. An interface rather than the
// concrete type so the compile-time surface states exactly what assurance can
// do -- and so what it CANNOT do is visible without reading the implementation.
type SnapshotCapturer interface {
	Capture(ctx context.Context, req CaptureRequest) (domain.Snapshot, error)
	Enabled() bool
}

// SnapshotAssuranceOptions configures a SnapshotAssurance.
//
// Note what is absent: every Docker capability interface, every plan writer,
// and every pipeline. There is nowhere to wire one.
type SnapshotAssuranceOptions struct {
	Capturer SnapshotCapturer
	Logger   *slog.Logger
}

// SnapshotAssurance establishes that a container has a current baseline.
type SnapshotAssurance struct {
	capturer SnapshotCapturer
	logger   *slog.Logger
}

// NewSnapshotAssurance builds a SnapshotAssurance.
func NewSnapshotAssurance(opts SnapshotAssuranceOptions) *SnapshotAssurance {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &SnapshotAssurance{capturer: opts.Capturer, logger: logger}
}

// Available reports whether assurance can do anything at all.
//
// False when snapshots are switched off or nothing was wired. Callers use this
// to distinguish "the operator turned this off" from "the baseline could not be
// established", which are different sentences and different remedies.
func (a *SnapshotAssurance) Available() bool {
	return a != nil && a.capturer != nil && a.capturer.Enabled()
}

// EnsureCurrent establishes that a container has a snapshot describing it now.
//
// # Why this is a capture rather than a comparison
//
// "Is the existing snapshot still current" cannot be answered without building
// the current document and checksumming it, because the checksum IS the
// comparison. Building it and then discarding it would be the same work with a
// worse outcome, so assurance builds it and offers it to the repository, whose
// ON CONFLICT (container_id, checksum) DO NOTHING settles the question
// transactionally. An unchanged container writes nothing and reports current; a
// changed one writes exactly one row.
//
// That is also why there is no cheaper "check first" path here. There is no
// cheaper check.
//
// # Every failure is closed
//
// Snapshots disabled, a capture already in flight, a container that cannot be
// read, a write that fails: all four return AssuranceUnavailable. None of them
// returns an error to the caller, because a caller that must fail closed should
// not have to remember to. The error is logged, not propagated, and the outcome
// carries HarborMaster's own sentence.
func (a *SnapshotAssurance) EnsureCurrent(
	ctx context.Context,
	containerID string,
	trigger domain.SnapshotTrigger,
) AssuranceResult {
	if !a.Available() {
		return AssuranceResult{
			Outcome: AssuranceUnavailable,
			Detail:  "configuration snapshots are switched off in this deployment",
		}
	}

	snapshot, err := a.capturer.Capture(ctx, CaptureRequest{
		ContainerID: containerID,
		Trigger:     trigger,
		// A FIXED sentence, chosen by trigger. Never interpolated, and never
		// operator text: this string is persisted on the snapshot row and
		// rendered in a browser.
		Reason: assuranceReason(trigger),
	})
	switch {
	case errors.Is(err, ErrSnapshotsDisabled):
		return AssuranceResult{
			Outcome: AssuranceUnavailable,
			Detail:  "configuration snapshots are switched off in this deployment",
		}
	case errors.Is(err, ErrCaptureInProgress):
		// Deferred rather than refused permanently. Another caller is already
		// establishing exactly this baseline, and the next attempt will
		// deduplicate onto it.
		return AssuranceResult{
			Outcome: AssuranceUnavailable,
			Detail: "a configuration snapshot for this container is already " +
				"being captured; HarborMaster will try again",
		}
	case err != nil:
		// The underlying error is LOGGED and never returned to the caller or
		// put on the result: it can carry a store path or a container detail,
		// and this string reaches an execution record.
		a.logger.WarnContext(ctx, "could not establish a configuration snapshot",
			slog.String("containerId", domain.ShortenID(containerID)),
			slog.String("trigger", string(trigger)),
			slog.Any("error", err))
		return AssuranceResult{
			Outcome: AssuranceUnavailable,
			Detail:  AssuranceUnavailable.Explain(),
		}
	}

	if snapshot.ID == 0 {
		// A capture that reported success without a row. Not reachable through
		// the current repository, and treated as a failure rather than trusted:
		// a zero id compared against a plan's zero id would read as agreement.
		a.logger.ErrorContext(ctx, "a configuration snapshot was reported captured without an identifier",
			slog.String("containerId", domain.ShortenID(containerID)))
		return AssuranceResult{
			Outcome: AssuranceUnavailable,
			Detail:  AssuranceUnavailable.Explain(),
		}
	}

	result := AssuranceResult{
		SnapshotID: snapshot.ID,
		Readiness:  snapshot.ReadinessStatus,
	}

	// A FAILED readiness check means the baseline exists and cannot be relied
	// on. Only a deduplicated capture can report this: a freshly written row
	// carries ReadinessUnknown until the detached evaluation lands, and unknown
	// is not failure -- the same reading the acquisition and execution gates
	// already take.
	if snapshot.ReadinessStatus == domain.ReadinessNotReady {
		result.Outcome = AssuranceNotReady
		result.Detail = AssuranceNotReady.Explain()
		return result
	}

	if snapshot.Deduplicated {
		result.Outcome = AssuranceCurrent
	} else {
		result.Outcome = AssuranceCaptured
	}
	result.Detail = result.Outcome.Explain()
	return result
}

// assuranceReason is the fixed prose stored on an assurance capture.
//
// A function over the closed trigger vocabulary rather than a caller-supplied
// string, so there is no path by which request text reaches the reason column.
func assuranceReason(trigger domain.SnapshotTrigger) string {
	switch trigger {
	case domain.SnapshotTriggerPreUpdate:
		return "captured by HarborMaster before a recreation"
	case domain.SnapshotTriggerScheduled:
		return "captured by HarborMaster so this container can be assessed for updates"
	default:
		return "captured by HarborMaster"
	}
}
