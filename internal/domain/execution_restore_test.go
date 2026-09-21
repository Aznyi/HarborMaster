package domain_test

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Automatic restoration of a failed update, and the vocabulary around it.

func TestRestoreStatesAreAClosedVocabulary(t *testing.T) {
	for _, state := range domain.ExecutionRestoreStates {
		if !domain.ValidExecutionRestoreState(string(state)) {
			t.Errorf("%q is listed but not valid", state)
		}
	}
	if !domain.ValidExecutionRestoreState("") {
		t.Error("the empty state (no restore applies) is not valid")
	}
	for _, bad := range []string{"done", "Restored", "rolledBack"} {
		if domain.ValidExecutionRestoreState(bad) {
			t.Errorf("%q was accepted as a restore state", bad)
		}
	}
	// The five outcomes an operator must be able to tell apart.
	want := []domain.ExecutionRestoreState{
		domain.RestoreRequested, domain.RestoreRestored, domain.RestoreFailed,
		domain.RestoreRefused, domain.RestoreUnavailable,
	}
	for _, state := range want {
		found := false
		for _, listed := range domain.ExecutionRestoreStates {
			if listed == state {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not in ExecutionRestoreStates", state)
		}
	}
}

func TestRestoreStatesSayWhetherTheServiceIsBack(t *testing.T) {
	cases := map[domain.ExecutionRestoreState]bool{
		"":                        false,
		domain.RestoreRequested:   false,
		domain.RestoreRestored:    true,
		domain.RestoreFailed:      false,
		domain.RestoreRefused:     false,
		domain.RestoreUnavailable: false,
	}
	for state, want := range cases {
		if got := state.ServiceRestored(); got != want {
			t.Errorf("%q.ServiceRestored() = %v, want %v", state, got, want)
		}
	}
	// Settled means the restore reached a conclusion, good or bad.
	for _, settled := range []domain.ExecutionRestoreState{
		domain.RestoreRestored, domain.RestoreFailed, domain.RestoreRefused, domain.RestoreUnavailable,
	} {
		if !settled.Settled() {
			t.Errorf("%q is not settled", settled)
		}
	}
	if domain.RestoreRequested.Settled() || domain.ExecutionRestoreState("").Settled() {
		t.Error("a pending or absent restore reads as settled")
	}
}

func TestAManualRestoreRequestKeyIsRecognised(t *testing.T) {
	key := domain.ManualRestoreRequestKey("exec_00112233445566778899")
	if key != "manual-restore:exec_00112233445566778899" {
		t.Errorf("key = %q", key)
	}
	if !domain.ManualRestoreRequest(key) {
		t.Error("the key HarborMaster built is not recognised")
	}
	if domain.ManualRestoreRequest(domain.AutomationRequestKeyPrefix + "rollback:exec_x") {
		t.Error("an automation key was read as a manual restore")
	}
	if domain.AutomaticRequest(key) {
		t.Error("a manual restore key was read as an automation request")
	}

	rollback := domain.Rollback{RequestKey: key}
	if !rollback.ManualRestore() || rollback.Automatic() {
		t.Error("a rollback carrying the restore key does not report itself as one")
	}
	if !rollback.Unattended() {
		t.Error("a manual restore is unattended: nobody pressed the button")
	}
	if (domain.Rollback{}).Unattended() {
		t.Error("an operator-requested rollback reads as unattended")
	}
}

func TestTheRestartPolicyFailuresAreInBothVocabularies(t *testing.T) {
	var execListed, rollbackListed bool
	for _, failure := range domain.ExecutionFailures {
		if failure == domain.ExecutionFailureRestartPolicy {
			execListed = true
		}
	}
	for _, failure := range domain.RollbackFailures {
		if failure == domain.RollbackFailureRestartPolicy {
			rollbackListed = true
		}
	}
	if !execListed || !rollbackListed {
		t.Fatalf("restartPolicy listed: execution=%v rollback=%v", execListed, rollbackListed)
	}
	if !domain.ValidExecutionFailure("restartPolicy") || !domain.ValidRollbackFailure("restartPolicy") {
		t.Error("restartPolicy is not a valid failure name")
	}
	if !domain.ExecutionFailureRestartPolicy.NeedsOperator() {
		t.Error("a suspension failure after the park does not need an operator")
	}
	for _, text := range []string{
		domain.ExecutionFailureRestartPolicy.Explain(),
		domain.RollbackFailureRestartPolicy.Explain(),
	} {
		if !strings.Contains(strings.ToLower(text), "restart") {
			t.Errorf("the explanation does not mention the restart policy: %q", text)
		}
		if strings.Contains(strings.ToLower(text), "did not succeed") {
			t.Errorf("the explanation is the generic fallback: %q", text)
		}
	}
}
