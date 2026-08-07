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

// SelectionTarget is what a selector is matched against.
//
// A projection rather than the whole container: a selector must not be able to
// reach configuration it has no business reading, and a narrow input makes the
// matching function pure and cheap to test.
type SelectionTarget struct {
	Name   string
	Image  string
	Labels map[string]string
}

// Matches reports whether a selector governs one container.
func (s UpdateSelector) Matches(target SelectionTarget) bool {
	// Exclusion first, and final.
	for _, name := range s.Exclude {
		if strings.EqualFold(strings.TrimSpace(name), target.Name) {
			return false
		}
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
func (p UpdatePolicy) Governs(target SelectionTarget) bool {
	if !p.Enabled || p.Archived {
		return false
	}
	return p.Selector.Matches(target)
}

// SelectUpdatePolicy picks the one policy that governs a container.
//
// Highest priority wins; a tie is broken by policy id, so two policies with the
// same priority always resolve to the same winner rather than to whichever the
// database happened to return first. A scheduler that could pick differently on
// two passes would be one nobody could reason about.
func SelectUpdatePolicy(policies []UpdatePolicy, target SelectionTarget) (UpdatePolicy, bool) {
	var (
		best  UpdatePolicy
		found bool
	)
	for _, policy := range policies {
		if !policy.Governs(target) {
			continue
		}
		switch {
		case !found:
			best, found = policy, true
		case policy.Priority > best.Priority:
			best = policy
		case policy.Priority == best.Priority && policy.PolicyID < best.PolicyID:
			best = policy
		}
	}
	return best, found
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
