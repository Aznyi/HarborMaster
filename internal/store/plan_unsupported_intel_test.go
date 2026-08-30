package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The KEY CONTRACT between image intelligence and the planner.
//
// An image reference domain.NormalizeImageRef refuses has no canonical form, so
// the record for it is stored under the RAW reference. Two different pieces of
// code depend on that key: the inventory sync WRITES the record under it, and
// the planner READS the record back under it.
//
// They used to derive it separately -- and the planner's half was missing
// entirely, so the lookup was made under the empty canonical form, always
// missed, and the container was omitted from planning rather than reported as
// unassessable. domain.UnsupportedReferenceKey is now the single derivation,
// and this test is the round trip that holds the two halves together.

// unsupportedSeed is what the inventory sync writes for a refused reference:
// the raw string as identity, no registry, no tag, and not supported.
func unsupportedSeed(raw string) store.ImageReferenceSeed {
	return store.ImageReferenceSeed{
		Reference: domain.UnsupportedReferenceKey(raw),
		Familiar:  domain.UnsupportedReferenceKey(raw),
		Kind:      domain.RegistryUnknown,
		ImageID:   "sha256:" + repeat("2", 64),
		Supported: false,
		Detail:    "the image reference cannot be looked up: it names no public registry",
	}
}

func TestAnUnsupportedRecordIsReadableUnderThePlannerLookupKey(t *testing.T) {
	const raw = "registry.internal:5000/app:1.2.3"

	// The premise. If this ever normalises the test proves nothing.
	if _, err := domain.NormalizeImageRef(raw); err == nil {
		t.Fatalf("%q normalised; it is no longer an unsupported reference", raw)
	}

	db := openTestDB(t)
	ctx := context.Background()

	commitOf(t, db, records(
		buildContainer("container-u", "internal-app", withImage(raw, "sha256:image9")),
	))

	if _, err := db.ImageIntel.SyncReferences(ctx,
		[]store.ImageReferenceSeed{unsupportedSeed(raw)}, time.Now().UTC()); err != nil {
		t.Fatalf("SyncReferences: %v", err)
	}

	// The planner's key, derived exactly as the planner derives it.
	key := domain.UnsupportedReferenceKey(raw)
	if key == "" {
		t.Fatal("the lookup key is empty, which names no record")
	}

	inputs, err := db.Plans.GatherInputs(ctx, []string{"container-u"}, []string{key})
	if err != nil {
		t.Fatalf("GatherInputs: %v", err)
	}

	record, ok := inputs.Intel[key]
	if !ok {
		t.Fatalf("no record under %q; the planner cannot assess this container", key)
	}
	if record.Status != domain.CheckUnsupported {
		t.Errorf("status = %q, want %q", record.Status, domain.CheckUnsupported)
	}
	// Nothing was invented from a string the domain refused to parse.
	if record.RemoteDigest != "" || record.LatestTag != "" || record.LatestDigest != "" {
		t.Errorf("the record carries registry intelligence it could never have obtained: %+v", record)
	}
	if record.Update != domain.UpdateNone {
		t.Errorf("update type = %q, want none", record.Update)
	}

	// And the empty canonical form -- what the planner used to ask for -- names
	// nothing at all. This is the regression guard.
	if _, found := inputs.Intel[""]; found {
		t.Error("a record is reachable under the empty key, which several containers would share")
	}
}
