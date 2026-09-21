package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Every rollback FAILURE is writable.
//
// The same guard the executions table has had since 0028: the Go vocabulary
// and the CHECK on the column are one vocabulary in two places, and a failure
// the schema refuses is a rollback whose record says nothing about why it
// stopped -- on the one record that always needs a person.
func TestEveryRollbackFailureIsAcceptedByTheSchema(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if len(domain.RollbackFailures) < 14 {
		t.Fatalf("found %d rollback failures; the vocabulary is not where this "+
			"test thinks it is", len(domain.RollbackFailures))
	}

	for index, failure := range domain.RollbackFailures {
		rollback := rollbackFor(domain.NewExecutionID(), fmt.Sprintf("web-%d", index))
		created, err := db.Rollbacks.Create(ctx, rollback, now)
		if err != nil {
			t.Fatalf("the fixture could not be stored: %v", err)
		}
		if _, err := db.Rollbacks.Advance(ctx, store.RollbackChange{
			RollbackID: created.RollbackID,
			To:         domain.RollbackFailed,
			Failure:    failure,
		}, now); err != nil {
			t.Errorf("the schema refuses rollback failure %q: %v", failure, err)
		}
	}
}
