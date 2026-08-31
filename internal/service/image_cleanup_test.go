package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The cleanup pass.
//
// The domain tests prove the DECISION is right. These prove the pass around it
// is: that it re-checks before acting, that it stops when it cannot read its
// evidence, that a daemon refusal is a safety stop rather than a failure, and
// that it never removes more than it was allowed to.
//
// Every case is written so that the failure message says what an operator would
// have lost. A test here that goes green wrongly is an image nobody can get
// back.

// ---------------------------------------------------------------- fakes --

// fakeRetentionStore is a scriptable evidence source.
type fakeRetentionStore struct {
	candidates []store.ImageCleanupCandidate
	references map[string]store.ImageReferences
	targets    map[string]struct{}

	candidatesErr error
	referencesErr error
	targetsErr    error

	// gathers counts reference lookups, so a test can assert that the evidence
	// behind a removal was re-read immediately before it.
	gathers int
}

func (f *fakeRetentionStore) ImageCleanupCandidates(
	context.Context,
) ([]store.ImageCleanupCandidate, error) {
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}
	return append([]store.ImageCleanupCandidate(nil), f.candidates...), nil
}

func (f *fakeRetentionStore) ImageReferencesFor(
	_ context.Context, imageIDs []string,
) (map[string]store.ImageReferences, error) {
	if f.referencesErr != nil {
		return nil, f.referencesErr
	}

	f.gathers++

	out := make(map[string]store.ImageReferences, len(imageIDs))
	for _, id := range imageIDs {
		out[id] = f.references[id]
	}
	return out, nil
}

func (f *fakeRetentionStore) PlanTargetImages(
	context.Context,
) (map[string]struct{}, error) {
	if f.targetsErr != nil {
		return nil, f.targetsErr
	}
	if f.targets == nil {
		return map[string]struct{}{}, nil
	}
	return f.targets, nil
}

// lateContainerRuntime models a container created BETWEEN the pass gathering
// evidence and the removal.
//
// It reports the host as empty on the first listing and occupied on every one
// after it, which is exactly the window the re-check exists to close. Wrapping
// the read-only fake rather than replacing it keeps every other Runtime method
// behaving normally.
type lateContainerRuntime struct {
	*docker.Fake
	appears []domain.ContainerSummary
	calls   int
}

func (r *lateContainerRuntime) ListContainers(
	ctx context.Context,
) ([]domain.ContainerSummary, error) {
	r.calls++
	if r.calls == 1 {
		return r.Fake.ListContainers(ctx)
	}
	return r.appears, nil
}

type fakeSelf struct{ identity domain.SelfIdentity }

func (f fakeSelf) Identity() domain.SelfIdentity { return f.identity }

func cleanupImageID(marker string) string {
	return "sha256:" + strings.Repeat(marker, 64)
}

var (
	cleanupOld  = cleanupImageID("a")
	cleanupSelf = cleanupImageID("b")
)

// cleanupSettledAt is well past any minimum age the tests configure.
var cleanupSettledAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func cleanupNow() time.Time { return cleanupSettledAt.Add(365 * 24 * time.Hour) }

func cleanupConfig() config.ImageCleanup {
	return config.ImageCleanup{
		Enabled:         true,
		MinAge:          14 * 24 * time.Hour,
		KeepGenerations: 1,
		Interval:        12 * time.Hour,
		MaxPerPass:      10,
	}
}

// cleanupRig assembles a service whose default state removes exactly one image.
//
// Deliberately eligible by default: a rig that removed nothing would make every
// safety test below pass without proving anything, so each test turns ONE thing
// on and asserts that the removal stops.
type cleanupRig struct {
	store   *fakeRetentionStore
	runtime *docker.Fake
	pruner  *docker.FakeImagePruner
	service *service.ImageCleanupService
}

func newCleanupRig(t *testing.T, adjust ...func(*cleanupRig, *config.ImageCleanup)) *cleanupRig {
	t.Helper()

	rig := &cleanupRig{
		store: &fakeRetentionStore{
			candidates: []store.ImageCleanupCandidate{{
				ImageID:          cleanupOld,
				ContainerName:    "web",
				SettledAt:        cleanupSettledAt,
				NewerGenerations: 3,
			}},
			references: map[string]store.ImageReferences{},
		},
		runtime: docker.NewFake(),
		pruner:  docker.NewFakeImagePruner(),
	}
	rig.pruner.Add(cleanupOld)

	cfg := cleanupConfig()
	for _, apply := range adjust {
		apply(rig, &cfg)
	}

	rig.service = service.NewImageCleanupService(service.ImageCleanupOptions{
		Store:   rig.store,
		Runtime: rig.runtime,
		Pruner:  rig.pruner,
		Self:    fakeSelf{identity: domain.SelfIdentity{ImageID: cleanupSelf}},
		Config:  cfg,
		Now:     cleanupNow,
	})
	return rig
}

func (r *cleanupRig) run(t *testing.T) service.ImageCleanupPass {
	t.Helper()
	return r.service.RunPass(context.Background())
}

// ------------------------------------------------------- the happy path --

func TestAnEligibleImageIsRemoved(t *testing.T) {
	// The non-vacuity control for this whole file. If cleanup never removes
	// anything, every "it did not remove" assertion below is meaningless.
	rig := newCleanupRig(t)

	pass := rig.run(t)

	if pass.Removed != 1 {
		t.Fatalf("Removed = %d, want 1: nothing was removed on the path that "+
			"is supposed to remove", pass.Removed)
	}
	if got := rig.pruner.RemovedIDs(); len(got) != 1 || got[0] != cleanupOld {
		t.Fatalf("removed %v, want exactly [%s]", got, cleanupOld[:14])
	}
	if rig.pruner.Holds(cleanupOld) {
		t.Error("the image is still in the modelled store")
	}
}

// ------------------------------------------------ every retaining reason --

func TestEveryRetainingReferenceStopsTheRemoval(t *testing.T) {
	cases := []struct {
		name  string
		what  string
		apply func(*cleanupRig)
	}{{
		name: "a container on the host still uses it",
		what: "removing it would pull the artefact out from under a live workload",
		apply: func(r *cleanupRig) {
			r.runtime.Containers = []domain.ContainerSummary{
				{ID: "c1", Name: "web", ImageID: cleanupOld},
			}
		},
	}, {
		name: "HarborMaster's own inventory still lists a container on it",
		what: "the records and the host are independent checks and either one retains",
		apply: func(r *cleanupRig) {
			r.store.references[cleanupOld] = store.ImageReferences{PresentContainers: 1}
		},
	}, {
		name: "a parked original still exists",
		what: "the parked container IS the rollback",
		apply: func(r *cleanupRig) {
			r.store.references[cleanupOld] = store.ImageReferences{PreservedContainers: 1}
		},
	}, {
		name: "an acquisition is in flight",
		what: "a download that has not finished has not decided anything",
		apply: func(r *cleanupRig) {
			r.store.references[cleanupOld] = store.ImageReferences{ActiveAcquisitions: 1}
		},
	}, {
		name: "a recreation is in flight",
		what: "the update is mid-apply and needs both of its images",
		apply: func(r *cleanupRig) {
			r.store.references[cleanupOld] = store.ImageReferences{ActiveExecutions: 1}
		},
	}, {
		name: "a rollback is in flight",
		what: "a container is about to be created from exactly this image",
		apply: func(r *cleanupRig) {
			r.store.references[cleanupOld] = store.ImageReferences{ActiveRollbacks: 1}
		},
	}, {
		name: "an update failed and has not been settled",
		what: "this image is what puts the workload back",
		apply: func(r *cleanupRig) {
			r.store.references[cleanupOld] = store.ImageReferences{UnsettledFailures: 1}
		},
	}, {
		name: "a recovery note is outstanding",
		what: "an operator has been told what to do and has not done it yet",
		apply: func(r *cleanupRig) {
			r.store.references[cleanupOld] = store.ImageReferences{OutstandingRecoveries: 1}
		},
	}, {
		name: "the update pipeline still targets it",
		what: "an approved plan pointing at it turns into a failure the moment it runs",
		apply: func(r *cleanupRig) {
			r.store.targets = map[string]struct{}{cleanupOld: {}}
		},
	}, {
		name: "it is the image HarborMaster is running",
		what: "a process that removes its own image cannot be restarted",
		apply: func(r *cleanupRig) {
			r.store.candidates[0].ImageID = cleanupSelf
			r.pruner.Add(cleanupSelf)
		},
	}}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rig := newCleanupRig(t)
			testCase.apply(rig)

			pass := rig.run(t)

			if pass.Removed != 0 || len(rig.pruner.RemovedIDs()) != 0 {
				t.Errorf("an image was removed although %s.\n\n%s.\n\n"+
					"removed=%v", testCase.name, testCase.what,
					rig.pruner.RemovedIDs())
			}
			if pass.Retained != 1 {
				t.Errorf("Retained = %d, want 1: the image was neither removed "+
					"nor accounted for as kept", pass.Retained)
			}
		})
	}
}

// --------------------------------------------------------- failing closed --

func TestEvidenceThatCouldNotBeReadRemovesNothing(t *testing.T) {
	unreadable := errors.New("unreadable")

	cases := []struct {
		name  string
		apply func(*fakeRetentionStore)
	}{
		{"the candidate list", func(f *fakeRetentionStore) { f.candidatesErr = unreadable }},
		{"the image references", func(f *fakeRetentionStore) { f.referencesErr = unreadable }},
		{"the plan targets", func(f *fakeRetentionStore) { f.targetsErr = unreadable }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rig := newCleanupRig(t)
			testCase.apply(rig.store)

			pass := rig.run(t)

			if pass.Removed != 0 || len(rig.pruner.RemovedIDs()) != 0 {
				t.Errorf("an image was removed although %s could not be read.\n\n"+
					"A check that could not be PERFORMED establishes nothing. "+
					"Reading it as 'no reference found' is how a cleanup pass "+
					"turns a database hiccup into a deleted image.", testCase.name)
			}
		})
	}
}

func TestADaemonThatCannotBeListedRemovesNothing(t *testing.T) {
	rig := newCleanupRig(t)
	rig.runtime.ListErr = docker.ErrUnreachable

	pass := rig.run(t)

	if pass.Removed != 0 || len(rig.pruner.RemovedIDs()) != 0 {
		t.Error("an image was removed although the live container list was unreadable.\n\n" +
			"The host listing is the check that sees a container created since " +
			"the last inventory refresh. Without it, cleanup is deciding from " +
			"a snapshot of unknown age.")
	}
}

func TestAServiceWithNoRuntimeRemovesNothing(t *testing.T) {
	pruner := docker.NewFakeImagePruner()
	pruner.Add(cleanupOld)

	cleanup := service.NewImageCleanupService(service.ImageCleanupOptions{
		Store: &fakeRetentionStore{
			candidates: []store.ImageCleanupCandidate{{
				ImageID: cleanupOld, ContainerName: "web", SettledAt: cleanupSettledAt,
			}},
			references: map[string]store.ImageReferences{},
		},
		Pruner: pruner,
		Config: cleanupConfig(),
		Now:    cleanupNow,
	})

	if pass := cleanup.RunPass(context.Background()); pass.Removed != 0 {
		t.Error("an image was removed with no read-only runtime wired.\n\n" +
			"No runtime is not 'no containers'. It is a check that could not " +
			"be performed at all.")
	}
}

// ------------------------------------------------------- configuration --

func TestDisabledCleanupRemovesNothing(t *testing.T) {
	rig := newCleanupRig(t, func(_ *cleanupRig, cfg *config.ImageCleanup) {
		cfg.Enabled = false
	})

	pass := rig.run(t)

	if pass.Removed != 0 || len(rig.pruner.RequestedIDs()) != 0 {
		t.Error("cleanup that is switched off still asked the daemon about an image.\n\n" +
			"Disabled means the daemon is never contacted, not that the answer " +
			"is discarded afterwards.")
	}
}

func TestAZeroMinimumAgeRemovesNothing(t *testing.T) {
	// Fail closed on an unusable policy. A zero MinAge is not "no waiting
	// period"; it is a configuration that never established one.
	rig := newCleanupRig(t, func(_ *cleanupRig, cfg *config.ImageCleanup) {
		cfg.MinAge = 0
	})

	if pass := rig.run(t); pass.Removed != 0 {
		t.Error("an image was removed under a zero minimum age.\n\n" +
			"Removing an image the moment it is superseded destroys exactly " +
			"the artefact a same-day rollback needs.")
	}
}

func TestAnImageInsideTheRetentionWindowIsKept(t *testing.T) {
	rig := newCleanupRig(t)
	// Settled yesterday against a fourteen-day window.
	rig.store.candidates[0].SettledAt = cleanupNow().Add(-24 * time.Hour)

	if pass := rig.run(t); pass.Removed != 0 {
		t.Error("an image settled yesterday was removed under a fourteen-day window.\n\n" +
			"The clock runs from SETTLEMENT. If it ran from anything else, the " +
			"operator's window to change their mind would not be the window " +
			"they configured.")
	}
}

func TestTheMostRecentGenerationIsKept(t *testing.T) {
	rig := newCleanupRig(t, func(_ *cleanupRig, cfg *config.ImageCleanup) {
		cfg.KeepGenerations = 2
	})
	// The immediately previous image for this workload.
	rig.store.candidates[0].NewerGenerations = 1

	if pass := rig.run(t); pass.Removed != 0 {
		t.Error("the second-newest superseded image was removed with two " +
			"generations configured.\n\n" +
			"Generations are the rollback contract: keeping two means an " +
			"operator can go back past the update that broke things to the " +
			"one before it.")
	}
}

func TestAPassNeverRemovesMoreThanItsLimit(t *testing.T) {
	rig := newCleanupRig(t, func(r *cleanupRig, cfg *config.ImageCleanup) {
		cfg.MaxPerPass = 2

		r.store.candidates = nil
		for _, marker := range []string{"1", "2", "3", "4", "5"} {
			id := cleanupImageID(marker)
			r.store.candidates = append(r.store.candidates, store.ImageCleanupCandidate{
				ImageID:          id,
				ContainerName:    "web-" + marker,
				SettledAt:        cleanupSettledAt,
				NewerGenerations: 3,
			})
			r.pruner.Add(id)
		}
	})

	pass := rig.run(t)

	if pass.Removed != 2 {
		t.Errorf("Removed = %d, want 2\n\n"+
			"The per-pass limit is the blast radius. It is what keeps a defect "+
			"in the eligibility rules from emptying an image store between one "+
			"look and the next.", pass.Removed)
	}
	if got := len(rig.pruner.RemovedIDs()); got != 2 {
		t.Errorf("the daemon removed %d images, want 2", got)
	}
}

// ---------------------------------------------------------- the re-check --

func TestAContainerThatAppearsMidPassStopsTheRemoval(t *testing.T) {
	// The TOCTOU case. The pass-level gather saw nothing; by the time this
	// image's turn came, a container had been created from it.
	late := &lateContainerRuntime{
		Fake: docker.NewFake(),
		appears: []domain.ContainerSummary{
			{ID: "c1", Name: "web", ImageID: cleanupOld},
		},
	}

	pruner := docker.NewFakeImagePruner()
	pruner.Add(cleanupOld)

	cleanup := service.NewImageCleanupService(service.ImageCleanupOptions{
		Store: &fakeRetentionStore{
			candidates: []store.ImageCleanupCandidate{{
				ImageID:          cleanupOld,
				ContainerName:    "web",
				SettledAt:        cleanupSettledAt,
				NewerGenerations: 3,
			}},
			references: map[string]store.ImageReferences{},
		},
		Runtime: late,
		Pruner:  pruner,
		Self:    fakeSelf{identity: domain.SelfIdentity{ImageID: cleanupSelf}},
		Config:  cleanupConfig(),
		Now:     cleanupNow,
	})

	pass := cleanup.RunPass(context.Background())

	if pass.Removed != 0 || len(pruner.RemovedIDs()) != 0 {
		t.Error("an image was removed although a container appeared between the " +
			"pass gathering evidence and the removal.\n\n" +
			"Gathering the evidence a second time immediately before acting is " +
			"the entire reason the re-check exists. Without it, cleanup acts on " +
			"a picture of the host that is as old as the pass.")
	}
}

func TestTheEvidenceIsGatheredAgainBeforeEachRemoval(t *testing.T) {
	rig := newCleanupRig(t)

	rig.run(t)

	// Once for the pass, once for the candidate about to be removed.
	if rig.store.gathers != 2 {
		t.Errorf("the store was asked for references %d times, want 2\n\n"+
			"One gather for the pass and one immediately before acting. A "+
			"single gather means the evidence behind a removal is as old as "+
			"the pass that started it.", rig.store.gathers)
	}
}

// ----------------------------------------------- what the daemon answers --

func TestADaemonRefusalIsASafetyStopAndNotAFailure(t *testing.T) {
	rig := newCleanupRig(t)
	rig.pruner.MarkInUse(cleanupOld)

	pass := rig.run(t)

	if pass.Refused != 1 {
		t.Errorf("Refused = %d, want 1\n\n"+
			"A daemon that says the image is still referenced has independently "+
			"contradicted every check above it. That is worth counting and "+
			"reporting, not swallowing.", pass.Refused)
	}
	if pass.Failed != 0 {
		t.Errorf("Failed = %d, want 0: a refusal is an answer, not a fault", pass.Failed)
	}
	if len(rig.pruner.RemovedIDs()) != 0 {
		t.Error("the image was removed despite the refusal")
	}
	if got := len(rig.pruner.RequestedIDs()); got != 1 {
		t.Errorf("the daemon was asked %d times, want exactly 1\n\n"+
			"A refusal is never retried. There is no force to escalate to, and "+
			"asking again would only produce the same answer.", got)
	}
}

func TestAnImageAlreadyGoneSettlesRatherThanFailing(t *testing.T) {
	rig := newCleanupRig(t)
	// Present to HarborMaster's records, absent from the modelled image store:
	// a second pass over the same candidate, or an operator who got there first.
	rig.pruner.Present = map[string]bool{}

	pass := rig.run(t)

	if pass.Failed != 0 {
		t.Errorf("Failed = %d, want 0\n\n"+
			"Cleanup is idempotent by necessity: it runs forever on a schedule "+
			"and its candidate set is historical. An image that is already gone "+
			"is the desired end state.", pass.Failed)
	}
	if pass.Removed != 0 {
		t.Errorf("Removed = %d, want 0: nothing was actually removed", pass.Removed)
	}
}

func TestAGenuineRemovalFailureIsReportedAndNotRetried(t *testing.T) {
	rig := newCleanupRig(t)
	rig.pruner.Err = docker.ErrUnreachable

	pass := rig.run(t)

	if pass.Failed != 1 {
		t.Errorf("Failed = %d, want 1", pass.Failed)
	}
	if got := len(rig.pruner.RequestedIDs()); got != 1 {
		t.Errorf("the daemon was asked %d times, want 1: a failed removal is "+
			"left for the next pass rather than retried inside this one", got)
	}
}

// --------------------------------------------------------- restart shape --

func TestAPassIsSafeToRepeat(t *testing.T) {
	// Restart behaviour, expressed as the property that actually matters:
	// running the same pass twice removes the image once and reports no fault.
	rig := newCleanupRig(t)

	first := rig.run(t)
	second := rig.run(t)

	if first.Removed != 1 {
		t.Fatalf("the first pass removed %d, want 1", first.Removed)
	}
	if second.Removed != 0 || second.Failed != 0 {
		t.Errorf("the second pass removed %d and failed %d, want 0 and 0\n\n"+
			"Cleanup keeps no durable 'already removed' state -- its candidate "+
			"set is historical evidence that does not change when an image "+
			"goes. Repeating a pass must therefore be free.",
			second.Removed, second.Failed)
	}
	if got := len(rig.pruner.RemovedIDs()); got != 1 {
		t.Errorf("the daemon removed %d images across two passes, want 1", got)
	}
}

func TestTheLastPassSummaryIsReported(t *testing.T) {
	rig := newCleanupRig(t)
	rig.run(t)

	last := rig.service.LastPass()
	if last.Removed != 1 || last.Considered != 1 {
		t.Errorf("LastPass = %+v, want one considered and one removed\n\n"+
			"An operator who turned cleanup on needs to be able to see what it "+
			"did without reading the audit log.", last)
	}
	if last.RanAt.IsZero() {
		t.Error("LastPass has no timestamp")
	}
}

// ----------------------------------------------------------------- audit --

// TestARemovalIsRecordedInTheSecurityLog holds invariant 9 for the one write
// path that has no human behind it.
//
// A removal HarborMaster performed on a timer is the case where the audit
// record is the ONLY account of why an image disappeared. It is written against
// a real recorder over a real database, so a test that faked the read-back
// would not exercise the thing that could be wrong.
func TestARemovalIsRecordedInTheSecurityLog(t *testing.T) {
	db := openDB(t)
	recorder := service.NewAuditRecorder(db.Audit, config.Auth{
		AuditSummaryWindow: 24 * time.Hour,
	}, nil, cleanupNow)

	pruner := docker.NewFakeImagePruner()
	pruner.Add(cleanupOld)

	cleanup := service.NewImageCleanupService(service.ImageCleanupOptions{
		Store: &fakeRetentionStore{
			candidates: []store.ImageCleanupCandidate{{
				ImageID:          cleanupOld,
				ContainerName:    "web",
				SettledAt:        cleanupSettledAt,
				NewerGenerations: 3,
			}},
			references: map[string]store.ImageReferences{},
		},
		Runtime: docker.NewFake(),
		Pruner:  pruner,
		Audit:   recorder,
		Config:  cleanupConfig(),
		Now:     cleanupNow,
	})

	if pass := cleanup.RunPass(context.Background()); pass.Removed != 1 {
		t.Fatalf("nothing was removed, so there is nothing to have audited: %+v", pass)
	}

	events, _, err := db.Audit.List(context.Background(),
		store.AuditFilter{Page: store.Page{Limit: 200}})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	var removals []domain.AuditEvent
	for _, event := range events {
		if event.Action == domain.AuditImageRemoved {
			removals = append(removals, event)
		}
	}
	if len(removals) != 1 {
		t.Fatalf("%d image.removed events, want exactly 1\n\n"+
			"A removal on a timer has nobody who remembers making it. The audit "+
			"record is the only account of why the image is gone.", len(removals))
	}

	removal := removals[0]
	if removal.Outcome != domain.AuditSucceeded {
		t.Errorf("outcome = %q, want succeeded", removal.Outcome)
	}
	if removal.TargetType != domain.AuditTargetImage {
		t.Errorf("target type = %q, want %q", removal.TargetType, domain.AuditTargetImage)
	}
	if removal.TargetID != cleanupOld {
		t.Errorf("target id = %q, want the image id: without it the record does "+
			"not say WHICH image went", removal.TargetID)
	}
	if removal.TargetName != "web" {
		t.Errorf("target name = %q, want the workload name", removal.TargetName)
	}
}

// TestARetainedImageIsNotAudited keeps the log readable.
//
// A pass that kept ninety images and removed one would otherwise write
// ninety-one records every twelve hours, and the ninety would bury the one that
// matters. What was kept, and why, is a question the pass summary answers.
func TestARetainedImageIsNotAudited(t *testing.T) {
	db := openDB(t)
	recorder := service.NewAuditRecorder(db.Audit, config.Auth{
		AuditSummaryWindow: 24 * time.Hour,
	}, nil, cleanupNow)

	runtime := docker.NewFake()
	runtime.Containers = []domain.ContainerSummary{
		{ID: "c1", Name: "web", ImageID: cleanupOld},
	}

	cleanup := service.NewImageCleanupService(service.ImageCleanupOptions{
		Store: &fakeRetentionStore{
			candidates: []store.ImageCleanupCandidate{{
				ImageID:          cleanupOld,
				ContainerName:    "web",
				SettledAt:        cleanupSettledAt,
				NewerGenerations: 3,
			}},
			references: map[string]store.ImageReferences{},
		},
		Runtime: runtime,
		Pruner:  docker.NewFakeImagePruner(),
		Audit:   recorder,
		Config:  cleanupConfig(),
		Now:     cleanupNow,
	})

	cleanup.RunPass(context.Background())

	events, _, err := db.Audit.List(context.Background(),
		store.AuditFilter{Page: store.Page{Limit: 200}})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, event := range events {
		if event.Action == domain.AuditImageRemoved {
			t.Fatalf("a retained image produced an %q record", domain.AuditImageRemoved)
		}
	}
}
