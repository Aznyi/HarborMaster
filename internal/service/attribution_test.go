package service_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Attribution of the two operations that change the Docker host.
//
// # What the audit found
//
// Phase 9.5 attributed every state-changing REQUEST. It did not attribute the
// OUTCOME: a recreation that stopped a container and replaced it produced no
// security-audit record at all, so the log could not answer the question an
// administrator asks after an incident -- who caused this container to be
// replaced, and did it work?
//
// The request row alone cannot answer it. A request can be refused by the
// second preflight, cancelled before the first mutation, expire in the queue,
// or fail partway leaving two containers behind. "Requested" and "happened"
// are different facts, and only one of them is about the host.

// attributionHarness wires a real audit recorder over a real database around
// the execution service.
//
// A real store rather than a double: the outcome is read BACK from the record
// after the pipeline finishes, so a test that faked the read would not exercise
// the thing that could be wrong.
type attributionHarness struct {
	*execHarness
	db    *store.DB
	audit *service.AuditRecorder
}

func newAttributionHarness(t *testing.T) *attributionHarness {
	t.Helper()

	base := newExecHarness(t)
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

	base.service = service.NewExecutionService(service.ExecutionOptions{
		Store:    base.store,
		Evidence: base.evidence,
		Runtime:  base.runtime,
		Capturer: base.mutator,
		Mutator:  base.mutator,
		Hasher:   service.NewHasher(key),
		Audit:    recorder,
		Config: config.Execution{
			Enabled:               true,
			RequireSnapshot:       true,
			StartupTimeout:        2 * time.Second,
			StabilityPeriod:       10 * time.Millisecond,
			HealthPollInterval:    time.Millisecond,
			StopTimeout:           time.Second,
			MaxConcurrent:         1,
			RequestTTL:            15 * time.Minute,
			AcquisitionFreshness:  24 * time.Hour,
			InventoryFreshness:    15 * time.Minute,
			PolicyFreshness:       24 * time.Hour,
			MaxEventsPerExecution: 200,
		},
		Logger: discardLogger(),
		Now:    base.now,
	})

	return &attributionHarness{execHarness: base, db: db, audit: recorder}
}

// auditEvents returns everything recorded, newest first.
func (h *attributionHarness) auditEvents(t *testing.T) []domain.AuditEvent {
	t.Helper()

	events, _, err := h.db.Audit.List(context.Background(),
		store.AuditFilter{Page: store.Page{Limit: 200}})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	return events
}

// findAudit returns the one event with an action, failing if there is not
// exactly one.
func findAudit(t *testing.T, events []domain.AuditEvent, action domain.AuditAction) domain.AuditEvent {
	t.Helper()

	var found []domain.AuditEvent
	for _, event := range events {
		if event.Action == action {
			found = append(found, event)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %q events, want exactly 1 (recorded: %v)",
			len(found), action, actionsOf(events))
	}
	return found[0]
}

func actionsOf(events []domain.AuditEvent) []domain.AuditAction {
	out := make([]domain.AuditAction, 0, len(events))
	for _, event := range events {
		out = append(out, event.Action)
	}
	return out
}

// theOperator is the account these tests attribute work to.
var theOperator = domain.Requester{UserID: "usr_operator000000000000", Username: "opsy"}

// TestASuccessfulRecreationIsAuditedAgainstItsRequester is the property the
// audit found missing.
func TestASuccessfulRecreationIsAuditedAgainstItsRequester(t *testing.T) {
	harness := newAttributionHarness(t)
	ctx := context.Background()

	requested, err := harness.service.Request(ctx, service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
		RequestedBy:   theOperator,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if requested.RequestedBy != theOperator {
		t.Fatalf("the record carries %+v, want %+v", requested.RequestedBy, theOperator)
	}

	final := harness.runOnce(t, requested)
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q, want succeeded (%s)", final.State, final.Message)
	}

	event := findAudit(t, harness.auditEvents(t), domain.AuditExecutionCompleted)

	if event.Outcome != domain.AuditSucceeded {
		t.Errorf("outcome = %q, want succeeded", event.Outcome)
	}
	if event.ActorUserID != theOperator.UserID || event.ActorUsername != theOperator.Username {
		t.Errorf("attributed to %q/%q, want %q/%q",
			event.ActorUserID, event.ActorUsername,
			theOperator.UserID, theOperator.Username)
	}
	if event.TargetType != domain.AuditTargetExecution {
		t.Errorf("target type = %q, want %q", event.TargetType, domain.AuditTargetExecution)
	}
	if event.TargetID != final.ExecutionID {
		t.Errorf("target id = %q, want the execution id", event.TargetID)
	}
	if event.TargetName != final.ContainerName {
		t.Errorf("target name = %q, want the container name", event.TargetName)
	}
	if !strings.Contains(event.Reason, "replaced") {
		t.Errorf("the reason does not say the container was replaced: %q", event.Reason)
	}

	// The completion is the PRIVILEGED action, not the request. A summary that
	// counted requests would over-report host changes.
	if !domain.AuditExecutionCompleted.Privileged() {
		t.Error("a completed recreation is not counted as a host change")
	}
	if domain.AuditExecutionRequested.Privileged() {
		t.Error("a recreation REQUEST is counted as a host change; a request can " +
			"still be refused, cancelled, or expire")
	}
}

// TestARefusedRecreationIsAuditedAsFailedAndSaysNothingChanged.
//
// The distinction this record carries is the one that decides whether somebody
// has to go and look at the host.
func TestARefusedRecreationIsAuditedAsFailedAndSaysNothingChanged(t *testing.T) {
	harness := newAttributionHarness(t)
	ctx := context.Background()

	requested, err := harness.service.Request(ctx, service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
		RequestedBy:   theOperator,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	// Make the SECOND preflight refuse. The daemon has gone away between the
	// request being accepted and the worker reaching it, which is exactly the
	// window the second preflight exists to cover -- and it refuses before the
	// first mutation, so the host is untouched.
	harness.runtime.SetPingErr(errDaemonGone)

	final := harness.runOnce(t, requested)
	if final.State != domain.ExecutionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}

	event := findAudit(t, harness.auditEvents(t), domain.AuditExecutionFailed)
	if event.Outcome != domain.AuditFailed {
		t.Errorf("outcome = %q, want failed", event.Outcome)
	}
	if event.ActorUsername != theOperator.Username {
		t.Errorf("attributed to %q, want %q", event.ActorUsername, theOperator.Username)
	}
	if !strings.Contains(event.Reason, "before anything on this host was changed") {
		t.Errorf("the reason does not state that the host is untouched: %q", event.Reason)
	}
	if domain.AuditExecutionFailed.Privileged() {
		t.Error("a failed recreation is counted as a host change")
	}
}

// TestAnUnattributedRecreationIsStillAudited.
//
// Work requested before HarborMaster recorded requesters, or by a path with no
// account behind it, must still produce an outcome record. "Not recorded" and
// "did not happen" are different, and only one of them is true here.
func TestAnUnattributedRecreationIsStillAudited(t *testing.T) {
	harness := newAttributionHarness(t)
	ctx := context.Background()

	requested, err := harness.service.Request(ctx, service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if requested.RequestedBy.Known() {
		t.Fatal("an unattributed request reported a requester")
	}

	final := harness.runOnce(t, requested)
	if final.State != domain.ExecutionSucceeded {
		t.Fatalf("state = %q, want succeeded (%s)", final.State, final.Message)
	}

	event := findAudit(t, harness.auditEvents(t), domain.AuditExecutionCompleted)
	if event.ActorUsername != "" || event.ActorUserID != "" {
		t.Errorf("an unattributed recreation was attributed to %q/%q",
			event.ActorUserID, event.ActorUsername)
	}
	if event.TargetID != final.ExecutionID {
		t.Error("the record does not name the execution, so it cannot be joined " +
			"to the request that started it")
	}
}

// TestTheOutcomeRecordCarriesNoSecretAndNoDockerText.
//
// The reason field reaches a page an administrator reads and a column that must
// stay bounded. It is built from HarborMaster's own words and a
// closed-vocabulary failure name -- never the operator-facing failure message,
// which can carry a Docker error, and never anything derived from container
// configuration.
func TestTheOutcomeRecordCarriesNoSecretAndNoDockerText(t *testing.T) {
	harness := newAttributionHarness(t)
	ctx := context.Background()

	// A Docker error carrying something that must not be repeated.
	const dockerNoise = "SECRET-FROM-THE-DAEMON"
	harness.mutator.StartErr = &dockerError{message: "start failed: " + dockerNoise}

	requested, err := harness.service.Request(ctx, service.ExecutionRequest{
		AcquisitionID: execAcquisitionID,
		RequestedBy:   theOperator,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	final := harness.runOnce(t, requested)
	if final.State != domain.ExecutionFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}

	for _, event := range harness.auditEvents(t) {
		haystack := strings.Join([]string{
			string(event.Action), string(event.Outcome), event.ActorUserID,
			event.ActorUsername, string(event.ActorRole), event.ActorSessionID,
			string(event.TargetType), event.TargetID, event.TargetName,
			event.RequestID, event.ClientAddr, event.Reason,
		}, "\x00")

		if strings.Contains(haystack, dockerNoise) {
			t.Errorf("audit event %q repeats a Docker error: %q", event.Action, event.Reason)
		}
		if len(event.Reason) > domain.MaxAuditReasonBytes {
			t.Errorf("audit event %q has an unbounded reason (%d bytes)",
				event.Action, len(event.Reason))
		}
	}

	// It DOES say the host was changed, because that is what decides whether
	// somebody has to act.
	event := findAudit(t, harness.auditEvents(t), domain.AuditExecutionFailed)
	if !strings.Contains(event.Reason, "needs attention") {
		t.Errorf("a recreation that failed after changing the host does not say so: %q",
			event.Reason)
	}
}

// dockerError is a minimal error for injecting into the fake mutator.
type dockerError struct{ message string }

func (e *dockerError) Error() string { return e.message }

// errDaemonGone stands in for the daemon becoming unreachable between the
// request and the worker.
var errDaemonGone = &dockerError{message: "docker engine unreachable"}
