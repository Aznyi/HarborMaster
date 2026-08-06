package service_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Attribution of a manual rollback.
//
// The same design as the recreation's, and for the same reason: the rollback
// record is an operational history an operator reads to understand ONE
// rollback, while the audit log is the security record that answers "what has
// been done to this host, and by whom".
//
// A request row alone cannot answer it. A request can be refused by the second
// preflight, cancelled before the first mutation, or expire in the queue.
// "Requested" and "happened" are different facts, and only one of them is about
// the host.

// rollbackAttributionHarness wires a real audit recorder over a real database
// around the rollback service.
//
// A real store rather than a double: the outcome is read BACK from the record
// after the pipeline finishes, so a test that faked the read would not exercise
// the thing that could be wrong.
type rollbackAttributionHarness struct {
	*rbHarness
	db *store.DB
}

func newRollbackAttributionHarness(
	t *testing.T,
	tune ...func(*rbHarness),
) *rollbackAttributionHarness {
	t.Helper()

	base := newRollbackHarness(t, tune...)
	db := openDB(t)

	recorder := service.NewAuditRecorder(db.Audit, config.Auth{
		AuditSummaryWindow: 24 * time.Hour,
	}, nil, base.now)

	key, err := service.LoadSecretKey(service.SecretKeyOptions{
		GeneratePath: filepath.Join(t.TempDir(), "secret.key"),
	})
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}

	base.service = service.NewRollbackService(service.RollbackOptions{
		Store:      base.store,
		Evidence:   base.evidence,
		Runtime:    base.host,
		Rollbacker: base.host,
		Hasher:     service.NewHasher(key),
		Audit:      recorder,
		Config: config.Rollback{
			Enabled:              true,
			MaxConcurrent:        1,
			RequestTTL:           10 * time.Minute,
			StartupTimeout:       2 * time.Second,
			StabilityPeriod:      10 * time.Millisecond,
			HealthPollInterval:   time.Millisecond,
			StopTimeout:          time.Second,
			InventoryFreshness:   15 * time.Minute,
			SweepInterval:        time.Hour,
			PruneInterval:        time.Hour,
			MaxEventsPerRollback: 200,
		},
		Logger: discardLogger(),
		Now:    base.now,
	})

	return &rollbackAttributionHarness{rbHarness: base, db: db}
}

// auditEvents returns everything recorded, newest first.
func (h *rollbackAttributionHarness) auditEvents(t *testing.T) []domain.AuditEvent {
	t.Helper()

	events, _, err := h.db.Audit.List(context.Background(),
		store.AuditFilter{Page: store.Page{Limit: 200}})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	return events
}

// theRollbackOperator is the account these tests attribute work to.
var theRollbackOperator = domain.Requester{
	UserID: "usr_rollback00000000000", Username: "opsy",
}

// requestAs asks for a rollback on behalf of an account.
func (h *rollbackAttributionHarness) requestAs(
	t *testing.T,
	requester domain.Requester,
) domain.Rollback {
	t.Helper()

	rollback, err := h.service.Request(context.Background(), service.RollbackRequest{
		ExecutionID: rbExecutionID,
		RequestedBy: requester,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return rollback
}

// TestASuccessfulRollbackIsAuditedAgainstItsRequester.
func TestASuccessfulRollbackIsAuditedAgainstItsRequester(t *testing.T) {
	harness := newRollbackAttributionHarness(t)

	requested := harness.requestAs(t, theRollbackOperator)
	if requested.RequestedBy != theRollbackOperator {
		t.Fatalf("the record carries %+v, want %+v",
			requested.RequestedBy, theRollbackOperator)
	}

	final := harness.runOnce(t, requested)
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state = %q, want succeeded (%s)", final.State, final.Message)
	}

	event := findAudit(t, harness.auditEvents(t), domain.AuditRollbackCompleted)

	if event.Outcome != domain.AuditSucceeded {
		t.Errorf("outcome = %q, want succeeded", event.Outcome)
	}
	if event.ActorUserID != theRollbackOperator.UserID ||
		event.ActorUsername != theRollbackOperator.Username {
		t.Errorf("attributed to %q/%q, want %q/%q",
			event.ActorUserID, event.ActorUsername,
			theRollbackOperator.UserID, theRollbackOperator.Username)
	}
	if event.TargetType != domain.AuditTargetRollback {
		t.Errorf("target type = %q, want %q", event.TargetType, domain.AuditTargetRollback)
	}
	if event.TargetID != final.RollbackID {
		t.Errorf("target id = %q, want the rollback id", event.TargetID)
	}
	if event.TargetName != final.ContainerName {
		t.Errorf("target name = %q, want the container name", event.TargetName)
	}
	if event.Reason == "" {
		t.Error("the outcome record says nothing about what happened")
	}

	// The COMPLETION is the privileged action, not the request. A summary that
	// counted requests would over-report host changes, because a request can
	// still be refused, cancelled, or expire.
	if !domain.AuditRollbackCompleted.Privileged() {
		t.Error("a completed rollback is not counted as a host change")
	}
	if domain.AuditRollbackRequested.Privileged() {
		t.Error("a rollback REQUEST is counted as a host change")
	}
}

// TestARefusedRollbackIsAuditedAsFailedAndSaysNothingChanged.
//
// The distinction this record carries is the one that decides whether somebody
// has to go and look at the host.
func TestARefusedRollbackIsAuditedAsFailedAndSaysNothingChanged(t *testing.T) {
	harness := newRollbackAttributionHarness(t)

	requested := harness.requestAs(t, theRollbackOperator)

	// Make the SECOND preflight refuse. Somebody removed the preserved original
	// between the request being accepted and the worker reaching it, which is
	// exactly the window the second preflight exists to cover.
	harness.host.remove(rbOriginalID)

	final := harness.runOnce(t, requested)
	if final.State != domain.RollbackFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Refusal != domain.RollbackRefusalOriginalMissing {
		t.Fatalf("refusal = %q, want originalMissing", final.Refusal)
	}

	event := findAudit(t, harness.auditEvents(t), domain.AuditRollbackFailed)
	if event.Outcome != domain.AuditFailed {
		t.Errorf("outcome = %q, want failed", event.Outcome)
	}
	if event.ActorUsername != theRollbackOperator.Username {
		t.Errorf("attributed to %q, want %q",
			event.ActorUsername, theRollbackOperator.Username)
	}
	if !strings.Contains(event.Reason, "before anything on this host was changed") {
		t.Errorf("the reason does not state that the host is untouched: %q", event.Reason)
	}
	if domain.AuditRollbackFailed.Privileged() {
		t.Error("a failed rollback is counted as a host change")
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("the host was touched: %v", ops)
	}
}

// TestAFailureThatChangedTheHostSaysSoInTheAuditRecord.
//
// The opposite of the case above, and the one an administrator reading the log
// must be able to tell apart from it at a glance.
func TestAFailureThatChangedTheHostSaysSoInTheAuditRecord(t *testing.T) {
	harness := newRollbackAttributionHarness(t, func(h *rbHarness) {
		h.host.startErr = errRollbackDaemonGone
	})

	requested := harness.requestAs(t, theRollbackOperator)

	final := harness.runOnce(t, requested)
	if final.State != domain.RollbackFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if !final.Checkpoint.HostChanged() {
		t.Fatalf("checkpoint = %q, want one that changed the host", final.Checkpoint)
	}

	event := findAudit(t, harness.auditEvents(t), domain.AuditRollbackFailed)
	if strings.Contains(event.Reason, "before anything on this host was changed") {
		t.Errorf("a failure that moved containers claims the host is untouched: %q",
			event.Reason)
	}
	if event.TargetID != final.RollbackID {
		t.Error("the record does not name the rollback, so it cannot be joined " +
			"to the request that started it")
	}
}

// TestACancelledRollbackIsNotAuditedAsAHostChange.
func TestACancelledRollbackIsNotAuditedAsAHostChange(t *testing.T) {
	harness := newRollbackAttributionHarness(t)

	requested := harness.requestAs(t, theRollbackOperator)
	if _, err := harness.service.Cancel(context.Background(), requested.RollbackID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Drive the worker so any deferred outcome audit would land.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	harness.service.Run(ctx)

	for _, event := range harness.auditEvents(t) {
		if event.Action == domain.AuditRollbackCompleted {
			t.Error("a cancelled rollback was audited as a completed host change")
		}
	}
	if ops := harness.host.operations(); len(ops) != 0 {
		t.Errorf("a cancelled rollback touched the host: %v", ops)
	}
}

// TestAnUnattributedRollbackIsStillAudited.
//
// Work started by a path with no account behind it must still produce an
// outcome record. "Not recorded" and "did not happen" are different, and only
// one of them is true here.
func TestAnUnattributedRollbackIsStillAudited(t *testing.T) {
	harness := newRollbackAttributionHarness(t)

	requested := harness.requestAs(t, domain.Requester{})
	if requested.RequestedBy.Known() {
		t.Fatal("an unattributed request reported a requester")
	}

	final := harness.runOnce(t, requested)
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state = %q, want succeeded (%s)", final.State, final.Message)
	}

	event := findAudit(t, harness.auditEvents(t), domain.AuditRollbackCompleted)
	if event.ActorUsername != "" || event.ActorUserID != "" {
		t.Errorf("an unattributed rollback was attributed to %q/%q",
			event.ActorUserID, event.ActorUsername)
	}
	if event.TargetID != final.RollbackID {
		t.Error("the record does not name the rollback")
	}
}

// TestNoRollbackAuditRecordCarriesASecret.
func TestNoRollbackAuditRecordCarriesASecret(t *testing.T) {
	harness := newRollbackAttributionHarness(t)

	final := harness.runOnce(t, harness.requestAs(t, theRollbackOperator))
	if final.State != domain.RollbackSucceeded {
		t.Fatalf("state = %q, want succeeded", final.State)
	}

	encoded, err := json.Marshal(harness.auditEvents(t))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{
		"hunter2", "DB_PASSWORD", "/var/run/docker.sock",
		"Error response from daemon", "Authorization", "Bearer ",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("an audit record contains %q", forbidden)
		}
	}
}
