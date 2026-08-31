package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Two identities, and the evidence that a container was actually checked (C3A).
//
// # Part A -- the registry comparison reaches the list row
//
// The planner writes no ChangePlan for a settled "nothing to do", so the
// ordinary case -- a container running the current image -- left no trace in
// the subsystem the row read, and reported "not checked". The comparison lives
// on the image intelligence record; these prove it is read for the page in one
// query and that nothing weaker than a real comparison can produce it.
//
// # Part B -- the historical id and the current one are different questions
//
// An acquisition records the container it was requested for, and that id must
// never move: it is what happened. But an update RECREATES the container, so
// the id is stale the moment the update it authorised is applied. A record has
// to answer both questions, and these prove it does across repeated
// recreations.

const c3aImage = "ghcr.io/acme/service:1.0.0"

// seedIntel records a reference and one check outcome for it.
func seedIntel(
	t *testing.T, db *store.DB, reference string, status domain.CheckStatus,
	update domain.UpdateType, succeeded bool,
) {
	t.Helper()
	ctx := context.Background()
	canonical, err := domain.NormalizeImageRef(reference)
	if err != nil {
		t.Fatalf("normalise %q: %v", reference, err)
	}
	if _, err := db.ImageIntel.SyncReferences(ctx, []store.ImageReferenceSeed{{
		Reference:  canonical.Canonical,
		Familiar:   reference,
		Kind:       canonical.Kind,
		Registry:   canonical.Host,
		Namespace:  canonical.Namespace,
		Repository: canonical.Path,
		Tag:        canonical.Tag,
	}}, time.Now().UTC()); err != nil {
		t.Fatalf("sync reference: %v", err)
	}

	outcome := store.CheckOutcome{
		Reference: canonical.Canonical,
		Status:    status,
		Update:    update,
	}
	if succeeded {
		outcome.RemoteDigest = "sha256:" + strings.Repeat("a", 64)
	}
	if err := db.ImageIntel.RecordCheck(ctx, outcome, time.Now().UTC()); err != nil {
		t.Fatalf("record check: %v", err)
	}
}

func c3aKeys(names ...string) []store.ContainerKey {
	keys := make([]store.ContainerKey, 0, len(names))
	for _, name := range names {
		keys = append(keys, store.ContainerKey{
			ID: name + "-id", Name: name, ImageRef: c3aImage,
		})
	}
	return keys
}

// -------------------------------------------------- Part A: the evidence --

func TestASuccessfulComparisonReachesTheContainerRow(t *testing.T) {
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")
	seedIntel(t, db, c3aImage, domain.CheckOK, domain.UpdateNone, true)

	evidence, err := db.Containers.Attention(ctx, c3aKeys("svc-a"))
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	row := evidence["svc-a-id"]

	if !row.CheckSettled {
		t.Fatal("a successful registry comparison did not reach the row; the " +
			"container would still report notChecked")
	}
	if row.CheckedUpdate != domain.UpdateNone {
		t.Errorf("checkedUpdate = %q, want none", row.CheckedUpdate)
	}
	if row.LastSuccessAt == nil {
		t.Error("no successful-check timestamp reached the row")
	}
	// And the whole point: the assessment can now say so, with no plan.
	if got := domain.AssessContainer(row); got.State != domain.AttentionUpToDate {
		t.Fatalf("assessed %q, want upToDate", got.State)
	}
}

func TestANeverSuccessfulComparisonSetsNoEvidence(t *testing.T) {
	// The invariant at the layer that actually reads the database. Each of
	// these rows carries update_type = none because that is the column
	// DEFAULT, which is precisely the value that must never become "up to
	// date".
	for _, status := range []domain.CheckStatus{
		domain.CheckPending,
		domain.CheckFailed,
		domain.CheckUnauthorized,
		domain.CheckNotFound,
		domain.CheckRateLimited,
		domain.CheckUnsupported,
	} {
		t.Run(string(status), func(t *testing.T) {
			db, ctx := preferenceRepo(t)
			commitContainers(t, db, "svc-a")
			seedIntel(t, db, c3aImage, status, domain.UpdateNone, false)

			evidence, err := db.Containers.Attention(ctx, c3aKeys("svc-a"))
			if err != nil {
				t.Fatalf("Attention: %v", err)
			}
			row := evidence["svc-a-id"]

			if row.CheckSettled {
				t.Fatalf("status %q was treated as a settled comparison", status)
			}
			if got := domain.AssessContainer(row); got.State == domain.AttentionUpToDate {
				t.Fatalf("status %q assessed upToDate from a defaulted column", status)
			}
		})
	}
}

func TestAFailedRecheckKeepsTheEarlierComparison(t *testing.T) {
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	// A real comparison answers...
	seedIntel(t, db, c3aImage, domain.CheckOK, domain.UpdateNone, true)
	// ...and the next attempt fails. RecordCheck deliberately does not
	// overwrite the verdict with a blank one (B1.1).
	seedIntel(t, db, c3aImage, domain.CheckFailed, "", false)

	evidence, err := db.Containers.Attention(ctx, c3aKeys("svc-a"))
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	row := evidence["svc-a-id"]

	if !row.CheckSettled {
		t.Fatal("a failed re-check discarded a real earlier comparison")
	}
	if row.CheckStatus != domain.CheckFailed {
		t.Errorf("checkStatus = %q, want failed -- the row must be able to say "+
			"the latest attempt did not answer", row.CheckStatus)
	}
	if row.LastSuccessAt == nil {
		t.Error("the verdict cannot be dated, so it cannot be qualified")
	}
}

func TestOneImageIsOneQueryHoweverManyContainersRunIt(t *testing.T) {
	// The cost rule. Image intelligence is per REFERENCE, so a page of
	// containers sharing an image must cost one row, not one lookup each.
	db, ctx := preferenceRepo(t)

	names := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		names = append(names, fmt.Sprintf("svc-%03d", i))
	}
	commitContainers(t, db, names...)
	seedIntel(t, db, c3aImage, domain.CheckOK, domain.UpdateNone, true)

	start := time.Now()
	evidence, err := db.Containers.Attention(ctx, c3aKeys(names...))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	if len(evidence) != len(names) {
		t.Fatalf("got %d rows, want %d", len(evidence), len(names))
	}
	for _, name := range names {
		if !evidence[name+"-id"].CheckSettled {
			t.Fatalf("%s did not receive the shared comparison", name)
		}
	}
	if elapsed > 2*time.Second {
		t.Fatalf("resolving %d containers took %s; a per-container query has "+
			"crept in", len(names), elapsed)
	}
}

func TestAnUnnormalisableReferenceAssertsNothing(t *testing.T) {
	// A reference NormalizeImageRef refuses has no intelligence record to find.
	// That is the same state as having none, and must stay the zero value
	// rather than becoming a claim.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "svc-a")

	evidence, err := db.Containers.Attention(ctx, []store.ContainerKey{
		{ID: "svc-a-id", Name: "svc-a", ImageRef: "not a valid @@ reference"},
	})
	if err != nil {
		t.Fatalf("an odd reference must not fail the page: %v", err)
	}
	row := evidence["svc-a-id"]
	if row.CheckSettled {
		t.Error("an unresolvable reference produced check evidence")
	}
	if got := domain.AssessContainer(row); got.State != domain.AttentionNotChecked {
		t.Errorf("assessed %q, want notChecked", got.State)
	}
}

// ------------------------------------------------- Part B: the two ids --

func TestAnAcquisitionKeepsItsHistoricalIdAndResolvesTheCurrentOne(t *testing.T) {
	db, ctx := preferenceRepo(t)

	// The container exists under its first id, and an acquisition is recorded
	// against it.
	commitContainers(t, db, "web")
	original := "web-id"
	if _, err := db.Acquisitions.Create(ctx,
		acquisitionForName("web", original), time.Now().UTC()); err != nil {
		t.Fatalf("create acquisition: %v", err)
	}

	read, _, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(read) != 1 {
		t.Fatalf("got %d acquisitions", len(read))
	}
	if read[0].ContainerID != original {
		t.Errorf("historical id = %q, want %q", read[0].ContainerID, original)
	}
	if read[0].CurrentContainerID != original {
		t.Errorf("current id = %q, want %q", read[0].CurrentContainerID, original)
	}
}

func TestTheCurrentIdFollowsRepeatedRecreations(t *testing.T) {
	// The property that matters for set-and-forget: after two updates the
	// record must still resolve the LATEST container, and must still say which
	// one the acquisition was originally for.
	db, ctx := preferenceRepo(t)

	commitContainers(t, db, "web")
	if _, err := db.Acquisitions.Create(ctx,
		acquisitionForName("web", "web-id"), time.Now().UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Two recreations. Each replaces the container with a new id under the
	// same NAME, which is HarborMaster's stable identity.
	for _, generation := range []string{"web-v2", "web-v3"} {
		commitContainersAs(t, db, map[string]string{"web": generation + "-id"})

		read, _, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{})
		if err != nil {
			t.Fatalf("list after %s: %v", generation, err)
		}
		if read[0].ContainerID != "web-id" {
			t.Errorf("after %s the HISTORICAL id moved to %q; history must not "+
				"be rewritten", generation, read[0].ContainerID)
		}
		if read[0].CurrentContainerID != generation+"-id" {
			t.Errorf("after %s the current id = %q, want %q",
				generation, read[0].CurrentContainerID, generation+"-id")
		}
	}
}

func TestARemovedContainerHasNoCurrentId(t *testing.T) {
	// The honest missing state. There is no current container, so there is
	// nothing to ask about -- and the absence is what tells a client to make no
	// request at all rather than one that 404s.
	db, ctx := preferenceRepo(t)

	commitContainers(t, db, "web", "other")
	if _, err := db.Acquisitions.Create(ctx,
		acquisitionForName("web", "web-id"), time.Now().UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// web is removed from the host.
	commitContainers(t, db, "other")

	read, _, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if read[0].ContainerID != "web-id" {
		t.Errorf("the historical id was lost: %q", read[0].ContainerID)
	}
	if read[0].CurrentContainerID != "" {
		t.Errorf("current id = %q, want empty -- no container of that name is "+
			"present", read[0].CurrentContainerID)
	}
}

func TestResolvingTheCurrentIdDoesNotCostAQueryPerAcquisition(t *testing.T) {
	// The N+1 guard for Part B. The resolution happens in the same statement
	// that reads the page, so a page of acquisitions stays one read.
	db, ctx := preferenceRepo(t)
	commitContainers(t, db, "web")

	for i := 0; i < 80; i++ {
		acquisition := acquisitionForName("web", "web-id")
		acquisition.Target.Digest = "sha256:" + fmt.Sprintf("%064d", i)
		if _, err := db.Acquisitions.Create(ctx, acquisition, time.Now().UTC()); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	start := time.Now()
	read, total, err := db.Acquisitions.List(ctx, store.AcquisitionFilter{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 80 {
		t.Fatalf("total = %d, want 80", total)
	}
	for _, acquisition := range read {
		if acquisition.CurrentContainerID != "web-id" {
			t.Fatalf("an acquisition did not resolve the current id: %+v", acquisition)
		}
	}
	if elapsed > 2*time.Second {
		t.Fatalf("listing %d acquisitions took %s; a per-row lookup has crept in",
			total, elapsed)
	}
}

// commitContainersAs commits an inventory where each name carries a GIVEN id.
//
// What a recreation actually produces: the same name, a different Docker id.
// Names absent from the map are not in this inventory and become not-present.
func commitContainersAs(t *testing.T, db *store.DB, byName map[string]string) {
	t.Helper()

	records := make([]store.ContainerRecord, 0, len(byName))
	for name, id := range byName {
		records = append(records, store.ContainerRecord{
			Detail: domain.ContainerDetail{
				Overview: domain.ContainerSummary{
					HostID:    domain.LocalHostID,
					ID:        id,
					ShortID:   domain.ShortenID(id),
					Name:      name,
					Image:     domain.ParseImageRef(c3aImage),
					ImageID:   "sha256:image1",
					State:     domain.StateRunning,
					Health:    domain.HealthNone,
					CreatedAt: preferenceNow,
					Present:   true,
				},
				State:       domain.StateDetail{State: domain.StateRunning, RawState: "running"},
				Labels:      []domain.Label{},
				Environment: []domain.EnvVar{},
				Mounts:      []domain.Mount{},
				Networks:    []domain.NetworkAttachment{},
				Warnings:    []domain.InventoryWarning{},
			},
			RawJSON: []byte(`{"Id":"` + id + `"}`),
		})
	}

	if _, err := db.Inventory.CommitRefresh(context.Background(), store.RefreshCommit{
		Host:       domain.Host{ID: domain.LocalHostID, Name: "local", Runtime: domain.RuntimeDocker},
		Containers: records,
		Record: domain.RefreshRecord{
			Trigger:          domain.TriggerManual,
			StartedAt:        time.Now().UTC(),
			ContainersListed: len(records),
			Checksum:         time.Now().UTC().Format(time.RFC3339Nano),
		},
		Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CommitRefresh: %v", err)
	}
}

// acquisitionForName builds an acquisition naming a container and its id.
func acquisitionForName(name, containerID string) domain.Acquisition {
	now := time.Now().UTC()
	return domain.Acquisition{
		AcquisitionID: domain.NewAcquisitionID(),
		PlanID:        "plan_00112233445566778899",
		ContainerID:   containerID,
		ContainerName: name,
		Target: domain.AcquisitionTarget{
			Registry:   "docker.io",
			Repository: "library/nginx",
			Digest:     "sha256:" + strings.Repeat("b", 64),
			Reference:  "nginx:1.27.1",
			Platform:   domain.Platform{OS: "linux", Architecture: "amd64"},
		},
		State:       domain.AcquisitionQueued,
		RequestedAt: now,
		ExpiresAt:   now.Add(time.Hour),
		PlanDigest:  strings.Repeat("f", 64),
	}
}
