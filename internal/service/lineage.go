package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Image lineage, service side.
//
// The store owns the record and the domain owns the rules; this file is the
// narrow port through which the acting services -- recreation and rollback --
// read and advance it, plus the reconciliation that keeps it honest against a
// host somebody changed by hand.
//
// # Why this is a port and not a *store.LineageRepository
//
// The same reason every other capability here is: a service holds the smallest
// interface that does its job, so what a service can do is visible from its
// type. Recreation may read one row and write one row. It cannot enumerate the
// estate, and it cannot delete.

// LineageStore is the lineage capability the acting services hold.
type LineageStore interface {
	Get(ctx context.Context, containerName string) (domain.ImageLineage, error)
	Upsert(ctx context.Context, lineage domain.ImageLineage) error
}

// LineageReader is the read-only capability the planner and the API hold.
type LineageReader interface {
	Get(ctx context.Context, containerName string) (domain.ImageLineage, error)
	All(ctx context.Context) ([]domain.ImageLineage, error)
	Tracked(ctx context.Context) ([]domain.ImageLineage, error)
}

// TrackingReferenceFor derives the reference a recreation should follow, from
// the plan that was APPROVED.
//
// Not from the container, not from a label, and not from a caller: the proposed
// reference is the one the whole evidence chain was built around, so using
// anything else here would let the tracking reference and the approved change
// disagree.
//
// Returns an empty string when the plan proposed something that cannot be
// followed -- a digest-only reference, or nothing at all. An empty result writes
// no label and leaves the existing tracking reference alone, which is the
// fail-closed direction: it keeps whatever was already established rather than
// replacing it with a guess.
func TrackingReferenceFor(plan domain.ChangePlan) (domain.NormalizedRef, bool) {
	proposed := strings.TrimSpace(plan.ProposedImage)
	if proposed == "" || len(proposed) > domain.MaxLineageReferenceBytes {
		return domain.NormalizedRef{}, false
	}
	parsed, err := domain.NormalizeImageRef(proposed)
	if err != nil || parsed.Path == "" || parsed.Tag == "" || parsed.Digest != "" {
		return domain.NormalizedRef{}, false
	}
	return parsed, true
}

// AdvanceLineageAfterRecreation records that a verified recreation moved a
// container onto a new digest, without losing what it follows.
//
// Called only from the success path, after verification passed. Errors are
// returned for the caller to log rather than to fail on: the host is already in
// the desired state, and reporting a working replacement as a failed recreation
// would be the more harmful inaccuracy. Stale lineage degrades safely -- the
// next pass proposes a digest that is already running, and the execution
// preflight refuses it.
func AdvanceLineageAfterRecreation(
	ctx context.Context,
	lineageStore LineageStore,
	containerName string,
	plan domain.ChangePlan,
	approvedDigest string,
	replacementID string,
	now func() time.Time,
) error {
	if lineageStore == nil {
		return nil
	}

	previous, err := lineageStore.Get(ctx, containerName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if errors.Is(err, store.ErrNotFound) {
		previous = domain.ImageLineage{
			ContainerName: containerName,
			State:         domain.LineageUntracked,
			Origin:        domain.LineageRecreated,
			CreatedAt:     now().UTC(),
		}
	}

	approved, _ := TrackingReferenceFor(plan)
	next := domain.AdvanceLineage(previous, approved, approvedDigest, replacementID, now().UTC())
	return lineageStore.Upsert(ctx, next)
}

// RestoreLineageAfterRollback returns lineage to the digest that is running
// again.
//
// The tracking reference is deliberately untouched: a rollback undoes which
// ARTEFACT runs, never which tag the operator asked HarborMaster to follow.
// Leaving lineage claiming the replacement's digest would have the next pass
// compare against a digest that is not running and conclude there was nothing
// to do -- which is the original defect, reintroduced through the failure path.
func RestoreLineageAfterRollback(
	ctx context.Context,
	lineageStore LineageStore,
	containerName string,
	restoredDigest string,
	originalID string,
	now func() time.Time,
) error {
	if lineageStore == nil {
		return nil
	}

	previous, err := lineageStore.Get(ctx, containerName)
	if errors.Is(err, store.ErrNotFound) {
		// Nothing was being followed, so there is nothing to put back. A
		// rollback does not establish lineage: it restores a container that
		// existed before HarborMaster's involvement in this change.
		return nil
	}
	if err != nil {
		return err
	}

	restored := domain.RestoreLineage(previous, restoredDigest, originalID, now().UTC())
	return lineageStore.Upsert(ctx, restored)
}
