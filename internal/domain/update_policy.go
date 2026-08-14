package domain

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
)

// Update policies: the rules that let HarborMaster update a container without
// being asked.
//
// # This is a deliberate reversal, and it is scoped
//
// Every phase before this one rested on a single premise: nothing changes the
// host unless a person asked. Phase 11 breaks that premise on purpose, because
// "a safer replacement for Watchtower" is not a thing you can be without it.
//
// What it does NOT break is everything the premise was protecting. The
// automation engine submits exactly the requests a human would submit, through
// exactly the same services, and every one of them re-runs its own preflight
// against the live host. Automation is a CALLER, not a bypass:
//
//   - It cannot acquire an image the planner did not recommend.
//   - It cannot recreate a container whose acquisition did not verify.
//   - It cannot skip health verification, preservation comparison, or the
//     checkpoint discipline.
//   - It holds no Docker capability of its own. It has no way to reach a
//     socket except by asking a service that already had one.
//
// # Why this is a separate subsystem from compliance policies
//
// A compliance policy answers "is this container configured acceptably" and
// produces a violation. An update policy answers "may HarborMaster change this
// container, when, and how far" and produces an action. Sharing a type would
// mean one edit could turn a reporting rule into a mutation rule, which is the
// last conflation this codebase should make.

// UpdatePolicyIDPrefix is the fixed prefix of a generated policy id.
const UpdatePolicyIDPrefix = "upd_"

// UpdatePolicyIDHexLength is how many hex characters follow the prefix.
const UpdatePolicyIDHexLength = 20

// NewUpdatePolicyID generates a policy identifier.
func NewUpdatePolicyID() string {
	var raw [UpdatePolicyIDHexLength / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("update policy id: " + err.Error())
	}
	return UpdatePolicyIDPrefix + hex.EncodeToString(raw[:])
}

// ValidUpdatePolicyID reports whether id has the generated shape.
func ValidUpdatePolicyID(id string) bool {
	if len(id) != len(UpdatePolicyIDPrefix)+UpdatePolicyIDHexLength {
		return false
	}
	if id[:len(UpdatePolicyIDPrefix)] != UpdatePolicyIDPrefix {
		return false
	}
	for _, r := range id[len(UpdatePolicyIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- strategy --

// UpdateStrategy is how far an update may go.
//
// Ordered by blast radius. A policy naming `patch` permits a digest move and a
// patch bump and refuses everything above, because the point of the setting is
// a ceiling rather than a filter.
type UpdateStrategy string

const (
	// StrategyDigestOnly permits a republished tag and nothing else. The
	// safest automation there is: the operator chose the tag, and only its
	// content moved.
	StrategyDigestOnly UpdateStrategy = "digestOnly"
	// StrategyPatch permits digest moves and patch bumps.
	StrategyPatch UpdateStrategy = "patch"
	// StrategyMinor permits everything up to a minor bump.
	StrategyMinor UpdateStrategy = "minor"
	// StrategyMajor permits everything, including a major bump.
	//
	// Never a default. A major version is where publishers put breaking
	// changes, and automating one is a decision an operator has to make in as
	// many words.
	StrategyMajor UpdateStrategy = "major"
)

// UpdateStrategies lists every strategy, least permissive first.
var UpdateStrategies = []UpdateStrategy{
	StrategyDigestOnly, StrategyPatch, StrategyMinor, StrategyMajor,
}

// ValidUpdateStrategy reports whether value names a strategy.
func ValidUpdateStrategy(value string) bool {
	for _, s := range UpdateStrategies {
		if string(s) == value {
			return true
		}
	}
	return false
}

// Permits reports whether a strategy allows an observed update type.
//
// UNKNOWN is never permitted by any strategy, and neither is a pre-release.
// An unknown update is one HarborMaster could not size -- a tag listing that
// ran out of budget, a registry that did not answer -- and automating a change
// whose size is unknown is exactly the thing a ceiling is for.
func (s UpdateStrategy) Permits(update UpdateType) bool {
	switch update {
	case UpdateRebind:
		// Permitted by every strategy, because a strategy is a ceiling on how
		// far a VERSION may move and a rebind moves it nowhere: the proposed
		// digest is the one HarborMaster observed the container already
		// running.
		//
		// The alternative was worse in the direction that matters. A policy
		// with a `patch` ceiling that refused rebinds would leave its own
		// containers permanently attached to a namespace that no longer exists
		// -- broken by an update the same policy performed -- with no path to
		// repair short of an operator noticing.
		//
		// This widens nothing else. A rebind still needs a governing policy, an
		// open window, an automatic mode, the acquisition preflight, and the
		// execution preflight, exactly like any other change.
		return true
	case UpdateDigest:
		return true
	case UpdatePatch:
		return s == StrategyPatch || s == StrategyMinor || s == StrategyMajor
	case UpdateMinor:
		return s == StrategyMinor || s == StrategyMajor
	case UpdateMajor:
		return s == StrategyMajor
	default:
		// none, prerelease, unknown, and anything added later.
		return false
	}
}

// ------------------------------------------------------------ safety mode --

// AutomationMode is how far a policy is allowed to act.
type AutomationMode string

const (
	// ModeObserve evaluates everything and does nothing. Image intelligence,
	// planning, policy matching, window checks, and the decision are all
	// recorded; no image is pulled and no container is touched.
	//
	// The correct first setting on any real host.
	ModeObserve AutomationMode = "observe"
	// ModeDryRun is observe plus a rendered execution order, so an operator
	// can read what WOULD happen and in what sequence.
	ModeDryRun AutomationMode = "dryRun"
	// ModeApprove decides automatically and waits for a person to release each
	// decision. Automation chooses; a human commits.
	ModeApprove AutomationMode = "approvalRequired"
	// ModeAutomatic acquires and recreates without asking.
	ModeAutomatic AutomationMode = "automatic"
)

// AutomationModes lists every mode, least active first.
var AutomationModes = []AutomationMode{
	ModeObserve, ModeDryRun, ModeApprove, ModeAutomatic,
}

// ValidAutomationMode reports whether value names a mode.
func ValidAutomationMode(value string) bool {
	for _, m := range AutomationModes {
		if string(m) == value {
			return true
		}
	}
	return false
}

// Mutates reports whether the mode may change the host at all.
//
// The single check every acting path asks. Observe and dry run are read-only
// by construction rather than by remembering to skip a call.
func (m AutomationMode) Mutates() bool { return m == ModeAutomatic }

// NeedsApproval reports whether a decision waits for a person.
func (m AutomationMode) NeedsApproval() bool { return m == ModeApprove }

// ------------------------------------------------------------- selectors --

// UpdateSelector chooses which containers a policy governs.
//
// # Evaluation order, and why exclusion wins
//
// Exclusion is checked FIRST and cannot be overridden. An operator who names a
// container in Exclude has said "never automate this", and no label, no image
// pattern, and no other clause may reverse it. Everything else is inclusive:
// a container matches if ANY populated clause matches it.
//
// An entirely empty selector matches NOTHING. A policy that accidentally
// governs the whole estate is the failure mode worth designing against, so the
// empty case is the safe one rather than the convenient one.
//
// # Exclusion outranks the SCOPE too
//
// UpdateScope decides how wide a policy reaches; Exclude decides what it may
// never reach. The second wins. A policy in ScopeAllEligible still consults
// Excludes first, which is what makes "everything except the database" a thing
// an operator can express in one rule -- see UpdatePolicy.Governs.
type UpdateSelector struct {
	// Labels matches containers carrying every one of these label keys, with
	// the given value when a value is given. Keys only when the value is
	// empty.
	Labels map[string]string `json:"labels,omitempty"`
	// Images matches the container's image reference against glob patterns,
	// e.g. "ghcr.io/acme/*" or "nginx:1.27.*".
	Images []string `json:"images,omitempty"`
	// Include names containers explicitly, by NAME. Container ids change on
	// every recreation, so a policy pinned to one would stop governing the
	// container the moment it acted.
	Include []string `json:"include,omitempty"`
	// Exclude names containers that this policy must never touch. Checked
	// first, and final.
	Exclude []string `json:"exclude,omitempty"`
}

// Empty reports whether the selector can match anything at all.
func (s UpdateSelector) Empty() bool {
	return len(s.Labels) == 0 && len(s.Images) == 0 && len(s.Include) == 0
}

// Excludes reports whether a container is named in Exclude.
//
// Its own method because it is consulted from two places -- the selector's own
// match, and the scope check that runs before it -- and the rule that exclusion
// is checked first and is final must be implemented exactly once.
func (s UpdateSelector) Excludes(name string) bool {
	for _, excluded := range s.Exclude {
		if strings.EqualFold(strings.TrimSpace(excluded), name) {
			return true
		}
	}
	return false
}

// Matches reports whether a selector governs one container.
func (s UpdateSelector) Matches(target SelectionTarget) bool {
	// Exclusion first, and final.
	if s.Excludes(target.Name) {
		return false
	}
	if s.Empty() {
		return false
	}

	for _, name := range s.Include {
		if strings.EqualFold(strings.TrimSpace(name), target.Name) {
			return true
		}
	}
	for _, pattern := range s.Images {
		if matchGlob(pattern, target.Image) {
			return true
		}
	}
	if len(s.Labels) > 0 && matchesAllLabels(s.Labels, target.Labels) {
		return true
	}
	return false
}

// matchesAllLabels reports whether every required label is present.
func matchesAllLabels(required, actual map[string]string) bool {
	for key, want := range required {
		got, ok := actual[key]
		if !ok {
			return false
		}
		if want != "" && got != want {
			return false
		}
	}
	return true
}

// matchGlob matches a reference against a bounded glob pattern.
//
// `*` matches any run of characters. Deliberately NOT a regular expression:
// an operator-supplied regex is an unbounded backtracking risk on a path the
// scheduler walks for every container on every pass, and glob covers the cases
// a reference pattern actually needs.
func matchGlob(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	// A leading segment must be a prefix.
	if parts[0] != "" {
		if !strings.HasPrefix(value, parts[0]) {
			return false
		}
		value = value[len(parts[0]):]
	}
	// A trailing segment must be a suffix.
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(value, last) {
			return false
		}
		value = value[:len(value)-len(last)]
	}
	// Middle segments must appear in order.
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		index := strings.Index(value, part)
		if index < 0 {
			return false
		}
		value = value[index+len(part):]
	}
	return true
}

// -------------------------------------------------------------- policy --

// UpdateLimits bounds what one policy may do at once.
type UpdateLimits struct {
	// MaxConcurrent is how many updates this policy may have in flight.
	MaxConcurrent int `json:"maxConcurrent"`
	// MaxPerRegistry bounds simultaneous work against one registry, so a
	// policy cannot become a thundering herd against a single host.
	MaxPerRegistry int `json:"maxPerRegistry"`
	// MaxPerRun caps how many updates one scheduler pass may START. The
	// difference from MaxConcurrent matters on a large estate: concurrency
	// bounds parallelism, this bounds the blast radius of a single decision.
	MaxPerRun int `json:"maxPerRun"`

	// AcquisitionTimeout, RecreateTimeout and HealthTimeout are the budgets
	// handed to the existing services. Expressed in seconds so the stored form
	// is unambiguous.
	AcquisitionTimeoutSeconds int `json:"acquisitionTimeoutSeconds"`
	RecreateTimeoutSeconds    int `json:"recreateTimeoutSeconds"`
	HealthTimeoutSeconds      int `json:"healthTimeoutSeconds"`
}

// UpdateFailureHandling is what happens when an update goes wrong.
type UpdateFailureHandling struct {
	// AutoRollback permits automation to invoke the EXISTING rollback service
	// when a recreation fails verification.
	AutoRollback bool `json:"autoRollback"`
	// PauseAfterFailures is how many failures within PauseWindow pause this
	// container's automation. Zero disables pausing, which is not recommended
	// and is reported as such.
	PauseAfterFailures int `json:"pauseAfterFailures"`
	// PauseWindowHours is the span failures are counted over.
	PauseWindowHours int `json:"pauseWindowHours"`
	// CooldownHours is how long a pause lasts before automation may resume by
	// itself. Zero means it never resumes without acknowledgement, which is
	// the safer setting and the default.
	CooldownHours int `json:"cooldownHours"`
	// MaxRetries is how many times one container's update may be retried
	// before it is treated as a failure for pause purposes.
	MaxRetries int `json:"maxRetries"`
}

// UpdatePolicy is one administrator-defined automation rule.
type UpdatePolicy struct {
	// ID is the internal row id. Not part of the API contract.
	ID int64 `json:"-"`
	// PolicyID is the IMMUTABLE public identifier, generated server-side.
	PolicyID string `json:"policyId"`

	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Priority orders policies when several match one container. HIGHER wins,
	// and a tie is broken by the policy id so the winner is deterministic.
	Priority int `json:"priority"`

	// Scope is what this policy is POINTED AT, and it is a field of its own
	// rather than a value hidden in the selector. See update_scope.go.
	//
	// The zero value is not a scope. Normalise maps it to ScopeSelector, so a
	// policy stored before this field existed keeps exactly the breadth its
	// selector always had, and Governs refuses anything it does not recognise.
	Scope    UpdateScope    `json:"scope"`
	Selector UpdateSelector `json:"selector"`
	Strategy UpdateStrategy `json:"strategy"`
	// MinimumRecommendation is the planner verdict a change must carry.
	//
	// Constrained to proceed or proceedWithCaution by validation. `unknown`,
	// `manualReview`, and `notRecommended` can never be automated: the first
	// two mean a person has to look, and the third means the model argued
	// against it.
	MinimumRecommendation Recommendation `json:"minimumRecommendation"`

	Mode   AutomationMode    `json:"mode"`
	Window MaintenanceWindow `json:"window"`

	Limits  UpdateLimits          `json:"limits"`
	Failure UpdateFailureHandling `json:"failure"`

	// Archived hides a policy without deleting the history that references it.
	Archived bool `json:"archived"`

	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Governs reports whether this policy applies to a container at all.
//
// # The order here is the whole selection rule
//
//  1. A policy that is off or withdrawn governs nothing.
//  2. An exclusion is checked next, before the scope, and is FINAL. This is
//     what makes an exclusion mean the same thing in every scope: the operator
//     named a container and said never, and no breadth setting outranks that.
//  3. Only then does the scope decide, and only two values decide anything. A
//     scope this build does not recognise governs NOTHING -- a policy written
//     by a newer build and read by an older one is refused rather than
//     reinterpreted, which is the one direction that cannot cause a surprise
//     mutation.
//
// Self is consulted only by the broad scope. A selector that names a container
// explicitly is an operator pointing at it, and the self-update refusal that
// follows in DecideAutomation, the acquisition preflight, and the execution
// preflight is what stops that -- three refusals that this method does not
// remove and cannot weaken.
func (p UpdatePolicy) Governs(target SelectionTarget, self SelfIdentity) bool {
	if !p.Enabled || p.Archived {
		return false
	}
	if p.Selector.Excludes(target.Name) {
		return false
	}

	switch p.Scope {
	case ScopeSelector, "":
		// The empty scope is the NARROW one, matching Normalise. It means the
		// policy predates the field, which is a statement about when it was
		// written and not about what it reaches -- and reaching is then decided
		// by the selector it already carried.
		return p.Selector.Matches(target)
	case ScopeAllEligible:
		selectable, _ := target.BroadlySelectable(self)
		return selectable
	default:
		// A scope that was SUPPLIED and is not recognised. Distinct from empty:
		// this is a policy written by a build that knew a breadth this one does
		// not, and the only safe reading of a breadth you cannot evaluate is
		// that it governs nothing.
		return false
	}
}

// GovernsWithReason is Governs, and says why not when it declines.
//
// Separate so the hot path stays allocation-free and the decision path can
// still record HarborMaster's own sentence about a container it passed over.
func (p UpdatePolicy) GovernsWithReason(target SelectionTarget, self SelfIdentity) (bool, string) {
	if !p.Enabled || p.Archived {
		return false, "the policy is not in force"
	}
	if p.Selector.Excludes(target.Name) {
		return false, "the policy excludes this container by name"
	}
	switch p.Scope {
	case ScopeSelector, "":
		if p.Selector.Matches(target) {
			return true, ""
		}
		return false, "the policy's selector does not name this container"
	case ScopeAllEligible:
		return target.BroadlySelectable(self)
	default:
		return false, "the policy names a scope this build does not understand"
	}
}

// SelectUpdatePolicy picks the one policy that governs a container.
//
// # The order, and why the middle rule was added
//
//  1. Highest PRIORITY wins. An operator's explicit ordering, and the only one
//     of the three a policy author controls directly.
//  2. At equal priority, the NARROWER SCOPE wins. A policy that names this
//     container beats one that reaches it by being broad.
//  3. At equal priority and equal scope, the lower POLICY ID wins, so the
//     winner is the same on every pass rather than whichever the database
//     happened to return first. A scheduler that could pick differently on two
//     passes would be one nobody could reason about.
//
// Rule 2 exists because rule 3 alone would decide by generated id which of a
// specific rule and a catch-all rule governs a container -- effectively at
// random, and re-rolled for every new policy. That was tolerable while every
// policy had to name what it governed. It is not tolerable now that one of them
// can be "everything": adding a catch-all must not silently take containers
// away from the rules an operator wrote for them.
//
// It can only ever move a container from a broad rule to a specific one. It
// never widens anything, never overrides an explicit priority, and never makes
// a policy govern a container that Governs declined.
func SelectUpdatePolicy(
	policies []UpdatePolicy,
	target SelectionTarget,
	self SelfIdentity,
) (UpdatePolicy, bool) {
	var (
		best  UpdatePolicy
		found bool
	)
	for _, policy := range policies {
		if !policy.Governs(target, self) {
			continue
		}
		if !found || moreSpecific(policy, best) {
			best, found = policy, true
		}
	}
	return best, found
}

// moreSpecific reports whether candidate outranks current for one container.
//
// A total order over the three keys, written as an explicit cascade rather than
// as a chain of boolean operators so that adding a key cannot accidentally
// change the meaning of the ones above it.
func moreSpecific(candidate, current UpdatePolicy) bool {
	if candidate.Priority != current.Priority {
		return candidate.Priority > current.Priority
	}
	if candidate.Scope.Broad() != current.Scope.Broad() {
		// The narrow one wins, whichever way round they arrived.
		return !candidate.Scope.Broad()
	}
	return candidate.PolicyID < current.PolicyID
}

// SortUpdatePolicies orders policies for display: priority descending, then id.
func SortUpdatePolicies(policies []UpdatePolicy) {
	sort.SliceStable(policies, func(i, j int) bool {
		if policies[i].Priority != policies[j].Priority {
			return policies[i].Priority > policies[j].Priority
		}
		return policies[i].PolicyID < policies[j].PolicyID
	})
}
