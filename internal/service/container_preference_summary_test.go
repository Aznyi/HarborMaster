package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The saved-behaviour summary (C2.2).
//
// # What this summary is, and what it must never become
//
// It answers one question for the automation workspace: which containers has an
// operator given an explicit update behaviour? That is a READ of what was
// saved. It is deliberately NOT an answer to "what will happen to each of
// these", because a preference may only make automation safer -- a policy can
// still hold a container this reports as `automatic` -- and answering the
// second question truthfully costs one engine evaluation per container.
//
// So these tests pin two things: that the counts describe present containers
// honestly, and that a preference outliving its container is reported as the
// inert row it is rather than counted among what is configured.

// fakePreferenceStore returns fixed rows and records what was asked of it.
type fakePreferenceStore struct {
	rows []store.ContainerPreferenceRow
	err  error
	// calls counts reads, so a summary that grew a per-row lookup is visible.
	calls int
}

func (f *fakePreferenceStore) SetContainerPreference(context.Context,
	domain.ContainerUpdatePreference, string, string, time.Time,
) (domain.ContainerUpdatePreference, error) {
	return domain.ContainerUpdatePreference{}, errors.New("the summary must not write")
}

func (f *fakePreferenceStore) ClearContainerPreference(context.Context, string) error {
	return errors.New("the summary must not clear a preference")
}

func (f *fakePreferenceStore) ContainerPreference(context.Context, string,
) (domain.ContainerUpdatePreference, error) {
	return domain.ContainerUpdatePreference{}, errors.New("the summary must not read per container")
}

func (f *fakePreferenceStore) ListContainerPreferences(context.Context,
) ([]domain.ContainerUpdatePreference, error) {
	return nil, errors.New("the summary must use the presence-resolving read")
}

func (f *fakePreferenceStore) ListContainerPreferencesWithPresence(context.Context,
) ([]store.ContainerPreferenceRow, error) {
	f.calls++
	return f.rows, f.err
}

func row(name string, behavior domain.UpdateBehavior, present bool, id string) store.ContainerPreferenceRow {
	return store.ContainerPreferenceRow{
		ContainerUpdatePreference: domain.ContainerUpdatePreference{
			ContainerName: name,
			Behavior:      behavior,
		},
		Present:            present,
		CurrentContainerID: id,
	}
}

func summaryService(t *testing.T, fake *fakePreferenceStore) *service.ContainerPreferenceService {
	t.Helper()
	return service.NewContainerPreferenceService(service.ContainerPreferenceOptions{
		Store: fake,
	})
}

func TestTheSummaryCountsEachBehaviour(t *testing.T) {
	fake := &fakePreferenceStore{rows: []store.ContainerPreferenceRow{
		row("grafana", domain.BehaviorAutomatic, true, "grafana-id"),
		row("immich", domain.BehaviorAutomatic, true, "immich-id"),
		row("vaultwarden", domain.BehaviorReviewFirst, true, "vaultwarden-id"),
	}}

	summary, err := summaryService(t, fake).BehaviorSummary(context.Background())
	if err != nil {
		t.Fatalf("BehaviorSummary: %v", err)
	}
	if summary.Total != 3 {
		t.Errorf("total = %d, want 3", summary.Total)
	}
	if summary.Counts[domain.BehaviorAutomatic] != 2 {
		t.Errorf("automatic = %d, want 2", summary.Counts[domain.BehaviorAutomatic])
	}
	if summary.Counts[domain.BehaviorReviewFirst] != 1 {
		t.Errorf("reviewFirst = %d, want 1", summary.Counts[domain.BehaviorReviewFirst])
	}
	if summary.Stale != 0 {
		t.Errorf("stale = %d, want 0", summary.Stale)
	}
}

func TestEveryBehaviourGetsARealZero(t *testing.T) {
	// A missing key and a genuine zero are indistinguishable to a client, and
	// the difference matters: "no container is set to monitor only" is a fact,
	// while a gap is a question. Every behaviour therefore has a key.
	fake := &fakePreferenceStore{rows: []store.ContainerPreferenceRow{
		row("grafana", domain.BehaviorAutomatic, true, "grafana-id"),
	}}

	summary, err := summaryService(t, fake).BehaviorSummary(context.Background())
	if err != nil {
		t.Fatalf("BehaviorSummary: %v", err)
	}
	for _, behavior := range domain.UpdateBehaviors {
		if _, ok := summary.Counts[behavior]; !ok {
			t.Errorf("%q has no count; a client cannot tell zero from absent", behavior)
		}
	}
	if summary.Counts[domain.BehaviorMonitorOnly] != 0 {
		t.Errorf("monitorOnly = %d, want 0", summary.Counts[domain.BehaviorMonitorOnly])
	}
}

func TestAnEmptyEstateIsAnEmptySummaryNotAnError(t *testing.T) {
	// The state every installation starts in. It must render, and it must still
	// carry the full set of zeroed counts.
	summary, err := summaryService(t, &fakePreferenceStore{}).BehaviorSummary(context.Background())
	if err != nil {
		t.Fatalf("no overrides must not be an error: %v", err)
	}
	if summary.Total != 0 || summary.Stale != 0 {
		t.Errorf("summary = %+v, want zeros", summary)
	}
	if summary.Items == nil {
		t.Error("items is nil; the contract promises a list, and a client that " +
			"iterates it must not have to guard for null")
	}
	if len(summary.Counts) != len(domain.UpdateBehaviors) {
		t.Errorf("counts has %d keys, want %d", len(summary.Counts), len(domain.UpdateBehaviors))
	}
}

func TestAStalePreferenceIsReportedSeparatelyAndNotCounted(t *testing.T) {
	// A preference is keyed by NAME so it survives the recreation it
	// authorises, which means one can outlive its container. Counting that row
	// among configured containers would overstate what is set up, and hiding it
	// would leave an operator unable to explain a name they no longer recognise.
	fake := &fakePreferenceStore{rows: []store.ContainerPreferenceRow{
		row("grafana", domain.BehaviorAutomatic, true, "grafana-id"),
		row("old-thing", domain.BehaviorAutomatic, false, ""),
	}}

	summary, err := summaryService(t, fake).BehaviorSummary(context.Background())
	if err != nil {
		t.Fatalf("BehaviorSummary: %v", err)
	}
	if summary.Total != 1 {
		t.Errorf("total = %d, want 1 -- a stale row is not a configured container", summary.Total)
	}
	if summary.Counts[domain.BehaviorAutomatic] != 1 {
		t.Errorf("automatic = %d, want 1", summary.Counts[domain.BehaviorAutomatic])
	}
	if summary.Stale != 1 {
		t.Errorf("stale = %d, want 1", summary.Stale)
	}
	// And it is still listed, so it can be recognised.
	if len(summary.Items) != 2 {
		t.Fatalf("items = %d, want 2 -- a stale row is reported, not dropped", len(summary.Items))
	}
	for _, item := range summary.Items {
		if item.ContainerName == "old-thing" && item.Present {
			t.Error("a stale row was marked present")
		}
	}
}

func TestTheSummaryDoesNotShowABehaviourOutsideTheVocabulary(t *testing.T) {
	// The column has a CHECK constraint, so this cannot arise from
	// HarborMaster. A value that reached the table another way must not become
	// a category on an operator's screen or a key in the counts.
	fake := &fakePreferenceStore{rows: []store.ContainerPreferenceRow{
		row("grafana", domain.BehaviorAutomatic, true, "grafana-id"),
		row("odd", domain.UpdateBehavior("whatever-it-wants"), true, "odd-id"),
	}}

	summary, err := summaryService(t, fake).BehaviorSummary(context.Background())
	if err != nil {
		t.Fatalf("BehaviorSummary: %v", err)
	}
	if len(summary.Items) != 1 || summary.Items[0].ContainerName != "grafana" {
		t.Fatalf("items = %+v, want only grafana", summary.Items)
	}
	if summary.Total != 1 {
		t.Errorf("total = %d, want 1", summary.Total)
	}
	if _, ok := summary.Counts[domain.UpdateBehavior("whatever-it-wants")]; ok {
		t.Error("an unknown behaviour became a count key")
	}
}

func TestTheSummaryIsOneRead(t *testing.T) {
	// The request-explosion guard at the service layer: however many
	// preferences exist, the summary reads once.
	rows := make([]store.ContainerPreferenceRow, 0, 50)
	for i := 0; i < 50; i++ {
		rows = append(rows, row(string(rune('a'+i%26))+"-svc", domain.BehaviorReviewFirst, true, "id"))
	}
	fake := &fakePreferenceStore{rows: rows}

	if _, err := summaryService(t, fake).BehaviorSummary(context.Background()); err != nil {
		t.Fatalf("BehaviorSummary: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("the summary made %d reads for %d preferences, want 1", fake.calls, len(rows))
	}
}

func TestASummaryFailureIsAFailureNotAnEmptyEstate(t *testing.T) {
	// Fail closed. A read that could not be performed establishes nothing, and
	// reporting it as "no container has an override" would be a claim
	// HarborMaster has no basis for.
	fake := &fakePreferenceStore{err: errors.New("database is gone")}

	if _, err := summaryService(t, fake).BehaviorSummary(context.Background()); err == nil {
		t.Fatal("a failed read was reported as a successful empty summary")
	}
}
