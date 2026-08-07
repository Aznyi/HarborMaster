package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The audit vocabulary lives in two places, and they must agree.
//
// # Why this test exists, and why it is worth its length
//
// `domain.AuditTargetTypes` is the Go vocabulary. The `target_type` CHECK on
// `audit_events` is the database's. They are ONE vocabulary written twice, and
// adding to either without the other produces a silent hole:
//
//   - The recorder never fails an operation because its audit row could not be
//     written. That is correct — an audit write must not be able to stop the
//     thing it describes — and it means a rejected row logs an error and
//     vanishes.
//   - Nothing in the interface says so. The audit page simply has no record of
//     the action, which is indistinguishable from the action never happening.
//
// This has now shipped THREE times: rollback (fixed by migration 0014),
// automation (0017), and notifications (0021). Each time the Go side was
// complete, every unit test passed — the API tests use a stub recorder, which
// has no constraint to violate — and the hole was found by somebody exercising
// a real database.
//
// So the test writes one row per vocabulary entry against a real database. A
// fourth omission fails the build.

func TestEveryAuditTargetTypeIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.AuditTargetTypes) < 15 {
		t.Fatalf("found %d target types; the vocabulary is not where this test "+
			"thinks it is", len(domain.AuditTargetTypes))
	}

	for _, targetType := range domain.AuditTargetTypes {
		event := domain.AuditEvent{
			EventID:    domain.NewAuditEventID(),
			Action:     domain.AuditInventoryRefreshed,
			Outcome:    domain.AuditSucceeded,
			TargetType: targetType,
			TargetID:   "vocabulary-check",
			OccurredAt: now,
		}
		if err := db.Audit.Record(ctx, event, now); err != nil {
			t.Errorf("the schema refuses target type %q: %v\n\n"+
				"domain.AuditTargetTypes and the target_type CHECK on audit_events "+
				"are one vocabulary in two places. A refused audit row does not fail "+
				"the operation it describes -- by design -- so this ships as a "+
				"capability with no audit trail and nothing saying so. Widen the "+
				"CHECK in a new migration; see 0021_notification_audit_target.sql.",
				targetType, err)
		}
	}
}

// Every audit ACTION the vocabulary declares is writable too.
//
// The action column has no enumerated CHECK — only a non-empty one — so this
// cannot fail the same way. It is here because the pair of vocabularies is the
// thing being pinned, and an action that could not be written for some other
// reason (a length bound, a future constraint) would be the same silent hole.
func TestEveryAuditActionIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, action := range domain.AuditActions {
		event := domain.AuditEvent{
			EventID:    domain.NewAuditEventID(),
			Action:     action,
			Outcome:    domain.AuditSucceeded,
			OccurredAt: now,
		}
		if err := db.Audit.Record(ctx, event, now); err != nil {
			t.Errorf("the schema refuses action %q: %v", action, err)
		}
	}

	// And they all came back, so none was silently dropped.
	_, total, err := db.Audit.List(ctx, store.AuditFilter{Page: store.Page{Limit: 500}})
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	if total != len(domain.AuditActions) {
		t.Fatalf("wrote %d actions and the log holds %d",
			len(domain.AuditActions), total)
	}
}
