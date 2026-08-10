package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Lineage reconciliation: keeping HarborMaster's claim about a container honest
// against a host somebody else can change.
//
// # What this is for
//
// Lineage says "this container follows repo:tag and is running digest A". Docker
// is the ground truth for what is actually running, and an operator can change
// it at any moment with `docker run`. Every disagreement between the two has to
// resolve to one of exactly three outcomes:
//
//	ESTABLISH    there is no claim yet, and the observation supports making one
//	CONFIRM      the claim and the observation agree; nothing to do
//	RE-ESTABLISH the claim is contradicted, so it is replaced by what is true
//
// What must never happen is the fourth thing: keeping a claim that the host
// contradicts. That is what would let HarborMaster compare a registry against a
// digest nothing is running, and then report an update it did not perform.
//
// # Why a contradiction re-establishes rather than refuses
//
// The planner refuses to plan a container whose lineage disagrees with what is
// observed -- it cannot assess a change from a starting point it does not know.
// That refusal is only safe if something eventually resolves the disagreement,
// or the container would be stuck out of automation forever, which is the very
// defect this phase exists to close. So reconciliation adopts the OBSERVED
// state: HarborMaster stops claiming what it believed and records what is
// there. It never claims to have performed the change.

// LineageReconciler keeps the lineage table consistent with the inventory.
type LineageReconciler struct {
	store  LineageReconcileStore
	logger *slog.Logger
	now    func() time.Time
}

// LineageReconcileStore is the capability reconciliation needs.
type LineageReconcileStore interface {
	Observations(ctx context.Context, limit int) ([]store.LineageObservation, error)
	Get(ctx context.Context, containerName string) (domain.ImageLineage, error)
	Upsert(ctx context.Context, lineage domain.ImageLineage) error
	PruneAbsent(ctx context.Context, batch int) (int64, error)
}

// NewLineageReconciler builds a reconciler.
func NewLineageReconciler(
	lineageStore LineageReconcileStore,
	logger *slog.Logger,
	now func() time.Time,
) *LineageReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &LineageReconciler{store: lineageStore, logger: logger, now: now}
}

// LineageReconcileResult reports what one pass did.
type LineageReconcileResult struct {
	// Established counts containers that gained lineage.
	Established int
	// Confirmed counts containers whose lineage already matched.
	Confirmed int
	// Reestablished counts containers whose lineage was contradicted by the
	// host and was replaced with what is actually running.
	Reestablished int
	// Untracked counts containers left deliberately unfollowed -- a genuine
	// digest-pinned workload nobody asked HarborMaster to track.
	Untracked int
	// Pruned counts lineage rows removed for containers that are gone.
	Pruned int64
}

// Reconcile brings lineage into agreement with the present estate.
//
// Runs on every inventory projection, BEFORE update discovery seeds its
// references, so a container that gained lineage this pass has its tracking
// reference resolved in the same pass rather than the next one.
func (r *LineageReconciler) Reconcile(ctx context.Context) (LineageReconcileResult, error) {
	var result LineageReconcileResult
	if r == nil || r.store == nil {
		return result, nil
	}

	observations, err := r.store.Observations(ctx, store.MaxLineageRows)
	if err != nil {
		return result, err
	}

	now := r.now().UTC()
	for _, observation := range observations {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		outcome, reconcileErr := r.reconcileOne(ctx, observation, now)
		if reconcileErr != nil {
			// One container's lineage failing must not stop the estate's.
			r.logger.WarnContext(ctx, "could not reconcile image lineage for a container",
				slog.String("containerName", observation.ContainerName),
				slog.String("error", reconcileErr.Error()))
			continue
		}
		switch outcome {
		case lineageEstablished:
			result.Established++
		case lineageConfirmed:
			result.Confirmed++
		case lineageReestablished:
			result.Reestablished++
		case lineageLeftUntracked:
			result.Untracked++
		}
	}

	pruned, err := r.store.PruneAbsent(ctx, 500)
	if err != nil {
		r.logger.WarnContext(ctx, "could not prune lineage for departed containers",
			slog.String("error", err.Error()))
	}
	result.Pruned = pruned
	return result, nil
}

type lineageOutcome int

const (
	lineageConfirmed lineageOutcome = iota
	lineageEstablished
	lineageReestablished
	lineageLeftUntracked
)

// reconcileOne resolves a single container.
func (r *LineageReconciler) reconcileOne(
	ctx context.Context,
	observation store.LineageObservation,
	now time.Time,
) (lineageOutcome, error) {
	if !domain.ValidContainerName(observation.ContainerName) {
		return lineageConfirmed, nil
	}

	observed, parseErr := domain.NormalizeImageRef(observation.ImageRef)
	if parseErr != nil {
		// A reference HarborMaster cannot parse is one it cannot follow. Left
		// alone rather than guessed at, and NOT an error: one odd container
		// must not stop the estate's reconciliation.
		//nolint:nilerr // an unparseable reference is a skip, not a failure.
		return lineageConfirmed, nil
	}

	// The digest actually running.
	//
	// From the reference when it is pinned, and otherwise from the LOCAL
	// IMAGE's RepoDigests -- which is the only place a tag-created container's
	// digest exists. Reading it out of the declared reference alone, as this
	// did originally, meant reconciliation observed nothing for the ordinary
	// case and so could never correct a lineage record the host contradicted.
	//
	// Never resolved from a registry: this establishes what IS running, and a
	// remote lookup answers a different question. Never taken from the tracking
	// label either -- that is an attacker-controlled claim about what to FOLLOW
	// and says nothing about what is executing.
	runningDigest, _ := domain.RunningDigestFor(observed, observation.RepoDigests)
	if runningDigest != "" && !domain.ValidImageDigest(runningDigest) {
		runningDigest = ""
	}

	existing, err := r.store.Get(ctx, observation.ContainerName)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return r.establish(ctx, observation, observed, runningDigest, now)
	case err != nil:
		return lineageConfirmed, err
	}

	return r.confirmOrReplace(ctx, existing, observation, observed, runningDigest, now)
}

// establish writes lineage for a container that had none.
func (r *LineageReconciler) establish(
	ctx context.Context,
	observation store.LineageObservation,
	observed domain.NormalizedRef,
	runningDigest string,
	now time.Time,
) (lineageOutcome, error) {
	lineage := domain.EstablishLineage(
		observation.ContainerName, observation.ContainerID, observed, runningDigest, now)

	// A label CANNOT establish lineage. corroborated is false here by
	// definition: there is no HarborMaster record for this container, so a
	// label claiming one is exactly the forgery LineageFromLabel refuses.
	// Passed through anyway so the refusal is exercised and its reason logged
	// rather than the label being silently ignored.
	if observation.TrackingLabel != "" && !lineage.Tracked() {
		if _, refusal := domain.LineageFromLabel(
			observation.TrackingLabel, observed, false); refusal != domain.LineageRefusalNone {
			r.logger.InfoContext(ctx, "a container carries a HarborMaster tracking label that was not accepted",
				slog.String("containerName", observation.ContainerName),
				slog.String("reason", string(refusal)),
				slog.String("detail", refusal.Explain()))
		}
	}

	if err := r.store.Upsert(ctx, lineage); err != nil {
		return lineageConfirmed, err
	}
	if lineage.Tracked() {
		return lineageEstablished, nil
	}
	return lineageLeftUntracked, nil
}

// confirmOrReplace resolves an existing claim against what is observed.
//
// The external-change cases from §7 of the phase all land here:
//
//	A  managed repo:latest, user recreates at a different digest of the SAME tag
//	   -> the tag still matches; the running digest is adopted.
//	B  managed repo:latest, user recreates as repo@digestB
//	   -> the container no longer declares a tag. Lineage is KEPT, because the
//	      operator did not say to stop following it, and the running digest is
//	      adopted. This is the one case where a digest-pinned container stays
//	      tracked, and it is safe precisely because HarborMaster already held
//	      the claim before the change.
//	C  user changes the tag to repo:stable
//	   -> the declared tag wins. The operator has said what to follow.
//	D  container deleted and recreated
//	   -> a new id under the same name; treated as C or A by its reference.
//	E  identity changed, configuration equivalent
//	   -> the id is refreshed as evidence; nothing else moves.
//	F  database and Docker disagree
//	   -> Docker wins for what is RUNNING. HarborMaster never claims it
//	      performed the change; only that this is what it now sees.
func (r *LineageReconciler) confirmOrReplace(
	ctx context.Context,
	existing domain.ImageLineage,
	observation store.LineageObservation,
	observed domain.NormalizedRef,
	runningDigest string,
	now time.Time,
) (lineageOutcome, error) {
	next := existing
	changed := false

	// Case C: the container now declares a tag, and it is not the one being
	// followed. The operator's declaration is authoritative over a record
	// HarborMaster wrote earlier.
	if observed.Tag != "" && observed.Digest == "" && observed.Path != "" {
		if !strings.EqualFold(observed.Canonical, existing.TrackingReference) {
			next.State = domain.LineageTracked
			next.Origin = domain.LineageObserved
			next.TrackingReference = observed.Canonical
			next.TrackingFamiliar = observed.Familiar
			next.Repository = observed.Path
			changed = true
		}
	}

	// Cases A, B, D, F: what is RUNNING has moved without HarborMaster.
	if runningDigest != "" && !strings.EqualFold(runningDigest, existing.RunningDigest) {
		next.RunningDigest = runningDigest
		// The origin becomes 'observed': HarborMaster is recording what it
		// sees, and must not leave a record implying it performed this change.
		next.Origin = domain.LineageObserved
		changed = true
	}

	// Case E: same everything, new container id. Evidence only.
	if observation.ContainerID != "" && observation.ContainerID != existing.ContainerID {
		next.ContainerID = observation.ContainerID
		changed = true
	}

	if !changed {
		return lineageConfirmed, nil
	}

	next.UpdatedAt = now
	if err := r.store.Upsert(ctx, next); err != nil {
		return lineageConfirmed, err
	}

	// Only a change to what is RUNNING or FOLLOWED counts as re-establishment;
	// a refreshed id is bookkeeping.
	if !strings.EqualFold(next.RunningDigest, existing.RunningDigest) ||
		!strings.EqualFold(next.TrackingReference, existing.TrackingReference) {
		r.logger.InfoContext(ctx, "a managed container changed outside HarborMaster; "+
			"its image lineage was re-established from what is running",
			slog.String("containerName", observation.ContainerName))
		return lineageReestablished, nil
	}
	return lineageConfirmed, nil
}
