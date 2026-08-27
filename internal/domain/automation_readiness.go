package domain

import "time"

// Automation readiness: the estate's answer to "what could this policy actually
// do right now, and why not the rest".
//
// # This is a PROJECTION, not a decision
//
// Nothing in this file decides anything. Every value below is derived from
// AutomationDecision records that DecideAutomation and the dependency gate
// already produced -- the same records a real pass writes. There is no second
// rule here, no approximation of eligibility, and no vocabulary that does not
// already exist: a group is keyed by AutomationReason and explained with
// AutomationReason.Explain().
//
// That is the whole design. A readiness surface that re-derived eligibility
// would be a second automation engine that drifts from the first, and the
// operator would be reading a number that no pass will honour.
//
// # Why the counts are attributed to one policy
//
// An estate can carry several policies, and SelectUpdatePolicy gives each
// container to exactly one of them. "How many containers would THIS policy
// update" is therefore not "how many decisions say update" -- it is "how many
// say update AND were decided under this policy". Counting the former would
// credit a policy with containers another policy governs.

// AutomationReadinessCandidatePolicyID identifies a policy being previewed but not saved.
//
// Deliberately not a valid generated identifier: ValidUpdatePolicyID refuses
// it, so it cannot be persisted, matched against a stored row, or mistaken for
// one. The `z` run also puts it last in the identifier tie-break inside
// moreSpecific -- real identifiers are lowercase hex, so every one of them
// sorts before this.
//
// That direction is deliberate. When a candidate ties an existing policy on
// both priority and breadth, the EXISTING policy wins the preview and the
// candidate is not credited with the container. The preview therefore
// understates rather than overstates what a new policy would take over, and
// which one actually wins is settled when the operator saves and a real
// identifier is generated.
const AutomationReadinessCandidatePolicyID = UpdatePolicyIDPrefix +
	"zzzzzzzzzzzzzzzzzzzz"

// AutomationReadinessGroup is one reason, with the containers that carry it.
type AutomationReadinessGroup struct {
	// Reason is the closed-vocabulary value. The UI maps it to a label; it is
	// never rendered raw.
	Reason AutomationReason `json:"reason"`
	// Explanation is HarborMaster's own sentence for the reason, so a caller
	// that has no mapping still renders something true rather than an enum.
	Explanation string `json:"explanation"`
	Count       int    `json:"count"`
	// Containers names the containers, bounded. A readiness answer is a summary;
	// an operator who wants the full list has the automation pages.
	Containers []string `json:"containers,omitempty"`
}

// MaxAutomationReadinessGroupContainers bounds the names carried per group.
//
// The response is otherwise proportional to the estate, and an estate is not
// something HarborMaster chose. Past the bound the count still tells the truth
// and only the sample is cut.
const MaxAutomationReadinessGroupContainers = 10

// AutomationReadinessReport answers the policy-preview question.
type AutomationReadinessReport struct {
	// EvaluatedAt marks the state this describes. A readiness answer is
	// advisory and may be stale the moment it is returned; the timestamp is
	// what lets a reader say how stale.
	EvaluatedAt time.Time `json:"evaluatedAt"`
	// Truncated reports that the estate was cut at the target bound, so every
	// count below describes a prefix of it rather than all of it.
	Truncated bool `json:"truncated"`

	// Considered is every container evaluated.
	Considered int `json:"considered"`
	// Governed is how many this policy is the chosen policy for. The
	// denominator for every count below.
	Governed int `json:"governed"`

	// Eligible is the headline: containers this policy would update now.
	Eligible int `json:"eligible"`
	// Observing counts containers a non-acting mode would have updated. Kept
	// apart from Eligible because "would, but the mode forbids it" and "will"
	// are different answers to the operator's question.
	Observing int `json:"observing"`
	// AwaitingApproval counts containers held for a person by mode or by the
	// deployment-wide major-version rule.
	AwaitingApproval int `json:"awaitingApproval"`

	// Groups explains everything this policy governs but would not update, most
	// common first. Sorted by count then reason, so the order is stable.
	Groups []AutomationReadinessGroup `json:"groups"`
}

// SummariseAutomationReadiness projects decisions onto the report, for one policy.
//
// Pure: no clock, no I/O, no map iteration order dependence. `policyID` selects
// which decisions are attributed; a decision made under another policy counts
// towards Considered and nothing else.
func SummariseAutomationReadiness(
	decisions []AutomationDecision,
	policyID string,
	evaluatedAt time.Time,
	truncated bool,
) AutomationReadinessReport {
	report := AutomationReadinessReport{
		EvaluatedAt: evaluatedAt.UTC(),
		Truncated:   truncated,
		Considered:  len(decisions),
	}

	counts := make(map[AutomationReason]int)
	names := make(map[AutomationReason][]string)
	order := make([]AutomationReason, 0, len(decisions))

	for _, decision := range decisions {
		attributed := decision.PolicyID == policyID
		if attributed {
			report.Governed++
		} else if !containerLevelRefusal(decision.Reason) {
			continue
		}

		switch decision.Verdict {
		case VerdictUpdate:
			report.Eligible++
			continue
		case VerdictWouldUpdate:
			report.Observing++
		case VerdictAwaitingApproval:
			report.AwaitingApproval++
		}

		// Everything that is not "would update now" is explained, including the
		// observe and approval verdicts: an operator asking why a container is
		// not being updated deserves the same answer whichever of those it is.
		if _, seen := counts[decision.Reason]; !seen {
			order = append(order, decision.Reason)
		}
		counts[decision.Reason]++
		if len(names[decision.Reason]) < MaxAutomationReadinessGroupContainers {
			names[decision.Reason] = append(names[decision.Reason], decision.ContainerName)
		}
	}

	// Built from `order`, which follows decision order, so the result does not
	// depend on map iteration. Sorted afterwards by count.
	report.Groups = make([]AutomationReadinessGroup, 0, len(order))
	for _, reason := range order {
		report.Groups = append(report.Groups, AutomationReadinessGroup{
			Reason:      reason,
			Explanation: reason.Explain(),
			Count:       counts[reason],
			Containers:  names[reason],
		})
	}
	sortAutomationReadinessGroups(report.Groups)

	return report
}

// containerLevelRefusal reports a refusal reached BEFORE a policy is selected.
//
// `DecideAutomation` refuses three things ahead of the policy lookup: it is
// HarborMaster's own container (step 0), automation is paused for it (step 1),
// or its owner opted out with a label (step 2). Those decisions carry no
// PolicyID, because no policy was consulted to reach them.
//
// # Why they are explained anyway
//
// Attributing strictly by PolicyID made them invisible. An operator previewing
// a policy over an estate containing a container paused after a failed update
// saw nothing about it at all -- not "paused", not even a count -- because the
// decision named no policy and the summary skipped it. That is the one question
// a readiness surface most needs to answer.
//
// So these are explained in the groups WITHOUT being counted as governed: no
// policy governs a container refused before selection, and inflating the
// denominator with containers this policy's selector may never have matched
// would be a different lie. The groups are an explanation of what stands in the
// way, not a partition of `Governed`, and the UI says so.
//
// The refusals meaning "this policy does not cover it" -- notSelected,
// noPolicy, notEligible -- are deliberately NOT here. Reporting every container
// another policy owns under this one's preview would make the answer useless.
func containerLevelRefusal(reason AutomationReason) bool {
	switch reason {
	case ReasonSelfUpdate, ReasonPaused, ReasonLabelOff, ReasonLabelPaused:
		return true
	default:
		return false
	}
}

// sortAutomationReadinessGroups orders by count descending, then by reason, so
// the order is total and stable rather than dependent on how the estate
// happened to be walked.
func sortAutomationReadinessGroups(groups []AutomationReadinessGroup) {
	for i := 1; i < len(groups); i++ {
		for j := i; j > 0; j-- {
			if !automationReadinessGroupBefore(groups[j], groups[j-1]) {
				break
			}
			groups[j], groups[j-1] = groups[j-1], groups[j]
		}
	}
}

func automationReadinessGroupBefore(a, b AutomationReadinessGroup) bool {
	if a.Count != b.Count {
		return a.Count > b.Count
	}
	return a.Reason < b.Reason
}
