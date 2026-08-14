package domain

import "testing"

// The dependency states inside the attention model.
//
// # What these are protecting
//
// Two decisions, and both are about NOT crying wolf:
//
//  1. **Waiting is not attention.** A container waiting for its dependency is
//     the system working. Giving it a state would put a routine condition on
//     the same list as a failed reattachment, and a list that reports routine
//     conditions stops being read.
//  2. **Silence when nothing was established.** A deployment with no dependency
//     subsystem must produce exactly the verdicts it produced before this
//     existed -- not a fleet of containers claiming their dependencies are fine.
//
// Everything else here is the precedence, asserted pair by pair rather than as
// one list, because the pairs are the decisions and the list is the consequence.

// evidenceWith returns baseline evidence a caller can bend.
//
// Deliberately a container with an ordinary patch available: it produces
// AttentionUpdateAvailable on its own, so any test below that sees something
// else is seeing the thing it changed.
func evidenceWith(mutate func(*ContainerEvidence)) ContainerEvidence {
	evidence := ContainerEvidence{
		Health:         HealthHealthy,
		State:          StateRunning,
		Present:        true,
		PlanKnown:      true,
		UpdateType:     UpdatePatch,
		Recommendation: RecommendProceed,
		LineageKnown:   true,
		Tracked:        true,
	}
	if mutate != nil {
		mutate(&evidence)
	}
	return evidence
}

func TestTheBaselineEvidenceIsAnOrdinaryUpdate(t *testing.T) {
	t.Parallel()

	// Without this every test below could pass by accident, on a baseline that
	// already produced the state being asserted.
	if got := AssessContainer(evidenceWith(nil)).State; got != AttentionUpdateAvailable {
		t.Fatalf("the baseline assesses as %q, want %q", got, AttentionUpdateAvailable)
	}
}

// ---------------------------------------------------------------- silence --

// A deployment without dependency tracking asserts nothing.
func TestUnknownDependencyStateChangesNothing(t *testing.T) {
	t.Parallel()

	// Every dependency state, with DependencyKnown false. None may reach a
	// verdict: the subsystem did not answer, so there is nothing to report.
	for _, state := range DependencyStates {
		evidence := evidenceWith(func(e *ContainerEvidence) {
			e.DependencyKnown = false
			e.DependencyState = state
		})
		if got := AssessContainer(evidence).State; got != AttentionUpdateAvailable {
			t.Errorf("an unanswered %q produced %q; a subsystem that did not "+
				"answer must not change the verdict", state, got)
		}
	}
}

// And the projection does not carry a state it was not told.
func TestAnUnansweredDependencyIsNotRenderedAsSatisfied(t *testing.T) {
	t.Parallel()

	attention := AssessContainer(evidenceWith(func(e *ContainerEvidence) {
		e.DependencyKnown = false
		e.DependencyState = DependencySatisfied
		e.DependencyBlockedBy = "postgres"
	}))
	if attention.DependencyKnown {
		t.Error("the projection claims the dependency subsystem answered")
	}
	if attention.DependencyState != "" {
		t.Errorf("DependencyState = %q, want empty", attention.DependencyState)
	}
	if attention.DependencyBlockedBy != "" {
		t.Errorf("DependencyBlockedBy = %q, want empty", attention.DependencyBlockedBy)
	}
}

// ---------------------------------------------------------------- waiting --

// Waiting is not a state, and there is no state for it to be.
func TestWaitingForADependencyIsNotAnAttentionState(t *testing.T) {
	t.Parallel()

	evidence := evidenceWith(func(e *ContainerEvidence) {
		e.DependencyKnown = true
		e.DependencyState = DependencyWaiting
		e.DependencyBlockedBy = "postgres"
	})
	attention := AssessContainer(evidence)

	if attention.State != AttentionUpdateAvailable {
		t.Fatalf("waiting produced %q; waiting for a dependency is the system "+
			"working and must not change what a row says about the container",
			attention.State)
	}
	// The fact is still CARRIED, so a detail page can explain the delay. It is
	// the VERDICT that must not change.
	if attention.DependencyState != DependencyWaiting {
		t.Error("the waiting state was not carried through for the detail view")
	}
	if attention.DependencyBlockedBy != "postgres" {
		t.Error("the container being waited on was not carried through")
	}

	// And no attention state is spelled like it.
	for _, state := range AttentionOrder {
		if string(state) == string(DependencyWaiting) {
			t.Fatalf("%q exists as an attention state; waiting must not be one", state)
		}
	}
}

// A satisfied dependency is not news either.
func TestASatisfiedDependencyChangesNothing(t *testing.T) {
	t.Parallel()

	evidence := evidenceWith(func(e *ContainerEvidence) {
		e.DependencyKnown = true
		e.DependencyState = DependencySatisfied
	})
	if got := AssessContainer(evidence).State; got != AttentionUpdateAvailable {
		t.Fatalf("a satisfied dependency produced %q", got)
	}
}

// ------------------------------------------------------------- the states --

func TestEachDependencyStateReachesItsVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mutate   func(*ContainerEvidence)
		want     AttentionState
		operator bool
	}{
		{
			name: "a loop",
			mutate: func(e *ContainerEvidence) {
				e.DependencyKnown = true
				e.DependencyState = DependencyCycle
			},
			want:     AttentionDependencyCycle,
			operator: true,
		},
		{
			name: "a dependency that could not be identified",
			mutate: func(e *ContainerEvidence) {
				e.DependencyKnown = true
				e.DependencyState = DependencyMissing
			},
			want:     AttentionDependencyUnresolved,
			operator: true,
		},
		{
			name: "a dependency that could not be updated safely",
			mutate: func(e *ContainerEvidence) {
				e.DependencyKnown = true
				e.DependencyState = DependencyBlocked
				e.DependencyBlockedBy = "postgres"
			},
			want: AttentionDependencyBlocked,
		},
		{
			name: "a dependency the rules do not permit to update",
			mutate: func(e *ContainerEvidence) {
				e.DependencyKnown = true
				e.DependencyState = DependencyIneligible
			},
			want: AttentionDependencyBlocked,
		},
		{
			name: "a reattachment that failed",
			mutate: func(e *ContainerEvidence) {
				e.RebindFailed = true
				e.RebindProvider = "gluetun"
			},
			want:     AttentionDependencyFailed,
			operator: true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := AssessContainer(evidenceWith(test.mutate)).State
			if got != test.want {
				t.Fatalf("state = %q, want %q", got, test.want)
			}
			if got.NeedsOperator() != test.operator {
				t.Errorf("%q.NeedsOperator() = %v, want %v",
					got, got.NeedsOperator(), test.operator)
			}
		})
	}
}

// A reattachment in progress is work, not a condition.
func TestAPendingRebindDoesNotChangeTheVerdict(t *testing.T) {
	t.Parallel()

	attention := AssessContainer(evidenceWith(func(e *ContainerEvidence) {
		e.RebindPending = true
		e.RebindProvider = "gluetun"
	}))
	if attention.State != AttentionUpdateAvailable {
		t.Fatalf("a pending rebind produced %q", attention.State)
	}
	if !attention.RebindPending || attention.RebindProvider != "gluetun" {
		t.Error("the pending rebind was not carried through for the detail view")
	}
}

// -------------------------------------------------------------- the pairs --

// The precedence decisions, stated one pair at a time.
//
// Each row is a judgement somebody made, not a consequence of the list order:
// the list is generated FROM these, so a change to it that contradicts one of
// them fails here rather than silently reordering what an operator reads.
func TestDependencyPrecedenceAgainstEveryOtherState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*ContainerEvidence)
		want   AttentionState
		why    string
	}{
		{
			name: "unhealthy beats a failed rebind",
			mutate: func(e *ContainerEvidence) {
				e.Health = HealthUnhealthy
				e.RebindFailed = true
			},
			want: AttentionUnhealthy,
			why:  "the healthcheck is the direct symptom; the rebind is the cause",
		},
		{
			name: "a failed rebind beats an approval",
			mutate: func(e *ContainerEvidence) {
				e.RebindFailed = true
				e.AwaitingApproval = true
			},
			want: AttentionDependencyFailed,
			why:  "an unfinished change to the host beats a decision waiting on somebody",
		},
		{
			name: "a failed rebind beats an available update",
			mutate: func(e *ContainerEvidence) {
				e.RebindFailed = true
			},
			want: AttentionDependencyFailed,
			why:  "the container may be detached from its provider's namespace",
		},
		{
			name: "preserved beats a failed rebind",
			mutate: func(e *ContainerEvidence) {
				e.Preserved = PreservedOriginal
				e.RebindFailed = true
			},
			want: AttentionPreserved,
			why:  "a parked container is not a workload, so no verdict about one applies",
		},
		{
			name: "an approval beats a loop",
			mutate: func(e *ContainerEvidence) {
				e.AwaitingApproval = true
				e.DependencyKnown = true
				e.DependencyState = DependencyCycle
			},
			want: AttentionApprovalRequired,
			why:  "an approval is waiting on this person; a loop is waiting on a change",
		},
		{
			name: "a pause beats a loop",
			mutate: func(e *ContainerEvidence) {
				e.AutomationPaused = true
				e.DependencyKnown = true
				e.DependencyState = DependencyCycle
			},
			want: AttentionPaused,
			why:  "both stop automation; a pause is HarborMaster having tried and given up",
		},
		{
			name: "a loop beats an unresolvable dependency",
			mutate: func(e *ContainerEvidence) {
				e.DependencyKnown = true
				e.DependencyState = DependencyCycle
			},
			want: AttentionDependencyCycle,
		},
		{
			name: "a loop beats not having been checked",
			mutate: func(e *ContainerEvidence) {
				e.PlanKnown = false
				e.LineageKnown = false
				e.DependencyKnown = true
				e.DependencyState = DependencyCycle
			},
			want: AttentionDependencyCycle,
			why:  "a loop is an established fact; not-checked is the absence of one",
		},
		{
			name: "an unresolvable dependency beats not having been checked",
			mutate: func(e *ContainerEvidence) {
				e.PlanKnown = false
				e.LineageKnown = false
				e.DependencyKnown = true
				e.DependencyState = DependencyMissing
			},
			want: AttentionDependencyUnresolved,
		},
		{
			name: "an unresolvable dependency beats an available update",
			mutate: func(e *ContainerEvidence) {
				e.DependencyKnown = true
				e.DependencyState = DependencyMissing
			},
			want: AttentionDependencyUnresolved,
			why:  "HarborMaster will refuse the update while it holds, so offering it would mislead",
		},
		{
			name: "a review request beats a block",
			mutate: func(e *ContainerEvidence) {
				e.Recommendation = RecommendManualReview
				e.DependencyKnown = true
				e.DependencyState = DependencyBlocked
			},
			want: AttentionNeedsReview,
			why:  "work addressed to the operator beats a consequence that may clear itself",
		},
		{
			name: "a block beats an available update",
			mutate: func(e *ContainerEvidence) {
				e.DependencyKnown = true
				e.DependencyState = DependencyBlocked
			},
			want: AttentionDependencyBlocked,
			why:  "the block is WHY the available update did not happen",
		},
		{
			name: "a block beats an unjudgeable update",
			mutate: func(e *ContainerEvidence) {
				e.Recommendation = RecommendUnknown
				e.DependencyKnown = true
				e.DependencyState = DependencyBlocked
			},
			want: AttentionDependencyBlocked,
		},
		{
			name: "an untracked image beats a block",
			mutate: func(e *ContainerEvidence) {
				e.PlanKnown = false
				e.Tracked = false
				e.DependencyKnown = true
				e.DependencyState = DependencyBlocked
			},
			want: AttentionNotTracked,
			why:  "no update will ever be found, so what blocked one is beside the point",
		},
		{
			name: "up to date beats a block",
			mutate: func(e *ContainerEvidence) {
				e.UpdateType = UpdateNone
				e.DependencyKnown = true
				e.DependencyState = DependencyBlocked
			},
			want: AttentionUpToDate,
			why:  "nothing wanted updating, so nothing was blocked in any sense that matters",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := AssessContainer(evidenceWith(test.mutate)).State
			if got != test.want {
				t.Fatalf("state = %q, want %q\n\t%s", got, test.want, test.why)
			}
		})
	}
}

// ------------------------------------------------------------- the vocabulary --

// Every dependency attention state has a place in the order.
//
// AttentionRank returns len(order) for anything it does not know, so a state
// added to the constants and forgotten in the order would silently rank LAST --
// which for a failed rebind would mean an update-available row outranking it.
func TestEveryDependencyAttentionStateIsRanked(t *testing.T) {
	t.Parallel()

	for _, state := range []AttentionState{
		AttentionDependencyFailed,
		AttentionDependencyCycle,
		AttentionDependencyUnresolved,
		AttentionDependencyBlocked,
	} {
		if AttentionRank(state) >= len(AttentionOrder) {
			t.Errorf("%q is not in AttentionOrder, so it ranks below every real "+
				"state", state)
		}
	}
}

// The order agrees with itself: every constant appears exactly once.
func TestAttentionOrderListsEveryStateOnce(t *testing.T) {
	t.Parallel()

	seen := make(map[AttentionState]int, len(AttentionOrder))
	for _, state := range AttentionOrder {
		seen[state]++
	}
	for state, count := range seen {
		if count != 1 {
			t.Errorf("%q appears %d times in AttentionOrder", state, count)
		}
	}

	// And the four new ones sit where the comments claim.
	rank := func(state AttentionState) int { return AttentionRank(state) }
	if rank(AttentionUnhealthy) >= rank(AttentionDependencyFailed) ||
		rank(AttentionDependencyFailed) >= rank(AttentionApprovalRequired) {
		t.Error("a failed rebind is not between unhealthy and approval")
	}
	if rank(AttentionPaused) >= rank(AttentionDependencyCycle) ||
		rank(AttentionDependencyCycle) >= rank(AttentionDependencyUnresolved) {
		t.Error("a loop is not between paused and unresolvable")
	}
	if rank(AttentionNeedsReview) >= rank(AttentionDependencyBlocked) ||
		rank(AttentionDependencyBlocked) >= rank(AttentionCannotAdvise) {
		t.Error("a block is not between needs-review and cannot-advise")
	}
}
