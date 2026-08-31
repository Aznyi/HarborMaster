package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The image-intelligence half of the attention projection, state by state.
//
// # What C3G changed, and what it must not have
//
// C3C read the FULL image-intelligence record and needed a second statement to
// map container to canonical reference first. C3G folded those into one join
// through containers.image_canonical, projecting only the three fields the
// decision actually reads.
//
// That is a query change and must be a NO-OP for output. Every registry state a
// container can be in is walked here and asserted on the evidence the
// projection produces, so a difference shows up as a state that stopped
// matching rather than as a count that moved.

// intelState is one registry situation and the evidence it must produce.
type intelState struct {
	name    string
	status  domain.CheckStatus
	update  domain.UpdateType
	success bool

	wantSettled       bool
	wantChecked       domain.UpdateType
	wantNotComparable bool
	wantStatus        domain.CheckStatus
}

func intelStates() []intelState {
	return []intelState{
		{
			name: "registry pending", status: domain.CheckPending,
			update: domain.UpdateNone, success: false,
		},
		{
			name: "failed with no prior success", status: domain.CheckFailed,
			update: domain.UpdateNone, success: false,
		},
		{
			name: "unauthorized with no prior success", status: domain.CheckUnauthorized,
			update: domain.UpdateNone, success: false,
		},
		{
			// Permanent: never queued, so no later pass changes the answer.
			name: "unsupported", status: domain.CheckUnsupported,
			update: domain.UpdateNone, success: false,
			wantNotComparable: true,
		},
		{
			name: "settled current", status: domain.CheckOK,
			update: domain.UpdateNone, success: true,
			wantSettled: true, wantChecked: domain.UpdateNone, wantStatus: domain.CheckOK,
		},
		{
			name: "settled with an update", status: domain.CheckOK,
			update: domain.UpdatePatch, success: true,
			wantSettled: true, wantChecked: domain.UpdatePatch, wantStatus: domain.CheckOK,
		},
		{
			// B1.1: a real earlier comparison the latest attempt could not
			// reconfirm. The verdict is PRESERVED and the row can say the
			// latest attempt did not answer.
			name: "prior success, latest attempt failed", status: domain.CheckFailed,
			update: domain.UpdateNone, success: true,
			wantSettled: true, wantChecked: domain.UpdateNone, wantStatus: domain.CheckFailed,
		},
	}
}

func TestEveryRegistryStateProducesTheSameEvidence(t *testing.T) {
	for _, tc := range intelStates() {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := preferenceRepo(t)
			commitContainersWithImage(t, db, currencyImage, "svc-a")

			// A prior success then the current attempt, so "settled but the
			// latest attempt failed" is reached the way the real scheduler
			// reaches it rather than by writing an impossible row.
			if tc.success {
				seedIntel(t, db, currencyImage, domain.CheckOK, tc.update, true)
			}
			seedIntel(t, db, currencyImage, tc.status, tc.update, tc.success && tc.status == domain.CheckOK)

			evidence, err := db.Containers.Attention(ctx,
				[]store.ContainerKey{{ID: "svc-a-id", Name: "svc-a"}})
			if err != nil {
				t.Fatalf("Attention: %v", err)
			}
			row := evidence["svc-a-id"]

			if row.CheckSettled != tc.wantSettled {
				t.Errorf("checkSettled = %v, want %v", row.CheckSettled, tc.wantSettled)
			}
			if row.CheckedUpdate != tc.wantChecked {
				t.Errorf("checkedUpdate = %q, want %q", row.CheckedUpdate, tc.wantChecked)
			}
			if row.CheckNotComparable != tc.wantNotComparable {
				t.Errorf("checkNotComparable = %v, want %v",
					row.CheckNotComparable, tc.wantNotComparable)
			}
			if row.CheckStatus != tc.wantStatus {
				t.Errorf("checkStatus = %q, want %q", row.CheckStatus, tc.wantStatus)
			}
			if tc.wantSettled && row.LastSuccessAt == nil {
				t.Error("a settled comparison carried no successful-check time")
			}
			if !tc.wantSettled && row.LastSuccessAt != nil {
				t.Error("an unsettled comparison carried a successful-check time")
			}
		})
	}
}

func TestAContainerWithNoIntelRecordAssertsNothing(t *testing.T) {
	// Never checked: no image_intel row at all, so the join matches nothing and
	// the evidence stays at its zero value.
	db, ctx := preferenceRepo(t)
	commitContainersWithImage(t, db, currencyImage, "svc-a")

	evidence, err := db.Containers.Attention(ctx,
		[]store.ContainerKey{{ID: "svc-a-id", Name: "svc-a"}})
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	row := evidence["svc-a-id"]
	if row.CheckSettled || row.CheckNotComparable ||
		row.CheckStatus != "" || row.LastSuccessAt != nil {
		t.Fatalf("a container nothing has checked carried evidence: %+v", row)
	}
	if got := domain.AssessContainer(row); got.State != domain.AttentionNotChecked {
		t.Errorf("assessed %q, want notChecked", got.State)
	}
}

func TestAnEmptyCanonicalReferenceJoinsToNothing(t *testing.T) {
	// A reference domain.NormalizeImageRef refuses stores the empty string, and
	// the join excludes it explicitly rather than matching every other row that
	// also has none.
	db, ctx := preferenceRepo(t)
	commitContainersWithImage(t, db, "not a valid @@ reference", "svc-a")
	// Intelligence exists for a DIFFERENT image; the empty canonical must not
	// match it.
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	evidence, err := db.Containers.Attention(ctx,
		[]store.ContainerKey{{ID: "svc-a-id", Name: "svc-a"}})
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	if row := evidence["svc-a-id"]; row.CheckSettled {
		t.Fatalf("a container with no canonical identity picked up another "+
			"image's evidence: %+v", row)
	}
}

func TestOneImageAnswersForEveryContainerRunningIt(t *testing.T) {
	// The join returns one row per container, and each must get the same
	// verdict from the single intelligence record.
	db, ctx := preferenceRepo(t)

	names := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("svc-%03d", i))
	}
	commitContainersWithImage(t, db, currencyImage, names...)
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	keys := make([]store.ContainerKey, 0, len(names))
	for _, name := range names {
		keys = append(keys, store.ContainerKey{ID: name + "-id", Name: name})
	}
	evidence, err := db.Containers.Attention(ctx, keys)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	for _, name := range names {
		row := evidence[name+"-id"]
		if !row.CheckSettled || row.CheckedUpdate != domain.UpdateNone {
			t.Fatalf("%s did not receive the shared comparison: %+v", name, row)
		}
	}
}

func TestTwoImagesDoNotCrossContaminate(t *testing.T) {
	// The join is per container through its own canonical reference, so two
	// containers on different images must get their own verdicts.
	db, ctx := preferenceRepo(t)
	const other = "ghcr.io/acme/other:2.0.0"

	commitContainersWithImage(t, db, currencyImage, "svc-a")
	commitContainersWithImage(t, db, other, "svc-b")
	// commitContainersWithImage replaces the inventory, so put both back.
	commitContainersWithImage(t, db, currencyImage, "svc-a")

	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)
	seedIntel(t, db, other, domain.CheckOK, domain.UpdatePatch, true)

	evidence, err := db.Containers.Attention(ctx,
		[]store.ContainerKey{{ID: "svc-a-id", Name: "svc-a"}})
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}
	row := evidence["svc-a-id"]
	if row.CheckedUpdate != domain.UpdateNone {
		t.Fatalf("svc-a picked up the other image's verdict: %q", row.CheckedUpdate)
	}
}

func TestAttentionCostIsFlatFromOneContainerToOverAHundred(t *testing.T) {
	// The empirical half of the budget. A fixed number of statements means the
	// page cost does not grow with the page, so a hundred-and-fifty container
	// page must not cost proportionally more than a one-container page.
	db, ctx := preferenceRepo(t)

	names := make([]string, 0, 150)
	for i := 0; i < 150; i++ {
		names = append(names, fmt.Sprintf("svc-%03d", i))
	}
	commitContainersWithImage(t, db, currencyImage, names...)
	seedIntel(t, db, currencyImage, domain.CheckOK, domain.UpdateNone, true)

	measure := func(count int) time.Duration {
		keys := make([]store.ContainerKey, 0, count)
		for _, name := range names[:count] {
			keys = append(keys, store.ContainerKey{ID: name + "-id", Name: name})
		}
		start := time.Now()
		evidence, err := db.Containers.Attention(ctx, keys)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Attention(%d): %v", count, err)
		}
		if len(evidence) != count {
			t.Fatalf("Attention(%d) returned %d rows", count, len(evidence))
		}
		return elapsed
	}

	one := measure(1)
	twentyFive := measure(25)
	hundredFifty := measure(150)
	t.Logf("attention: 1=%s  25=%s  150=%s", one, twentyFive, hundredFifty)

	// Deliberately loose. This is not a benchmark and wall-clock on a shared CI
	// machine proves nothing about speed; it exists to catch a per-container
	// read, which shows up as growth of a completely different order.
	if hundredFifty > 3*time.Second {
		t.Fatalf("150 containers took %s; a per-container query has crept in",
			hundredFifty)
	}
}
