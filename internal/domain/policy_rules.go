package domain

import (
	"sort"
	"strconv"
	"strings"
)

// The rule catalogue and the evaluator.
//
// This file is the ENTIRE semantics of the policy engine. Every rule's
// behaviour is fixed here at compile time; an administrator selects a rule type
// from the closed vocabulary below and supplies a bounded parameter list.
// Nothing an administrator writes is ever interpreted, compiled, or executed.
//
// Every function here is PURE: it takes a rule and a target and returns a
// verdict. There is no I/O, no clock, no database, and no Docker. That is what
// makes the rules exhaustively testable, and the tests do test them
// exhaustively -- every rule type against a compliant and a non-compliant
// target, plus the boundary cases each rule has.

// PolicyRuleType names one check. A closed vocabulary.
type PolicyRuleType string

// Policy rule types.
const (
	// RulePrivilegedForbidden requires privileged == false.
	RulePrivilegedForbidden PolicyRuleType = "privilegedForbidden"
	// RuleReadOnlyRootFilesystem requires readOnlyRootFilesystem == true.
	RuleReadOnlyRootFilesystem PolicyRuleType = "readOnlyRootFilesystemRequired"
	// RuleImageAllowlist requires the image reference to match a pattern.
	RuleImageAllowlist PolicyRuleType = "imageAllowlist"
	// RuleImageDenylist forbids the image reference from matching a pattern.
	RuleImageDenylist PolicyRuleType = "imageDenylist"
	// RuleRequiredLabels requires a label key matching each pattern.
	RuleRequiredLabels PolicyRuleType = "requiredLabels"
	// RuleForbiddenLabels forbids any label key matching a pattern.
	RuleForbiddenLabels PolicyRuleType = "forbiddenLabels"
	// RuleRequiredEnv requires an environment variable NAME matching each
	// pattern. Presence only; values are never read.
	RuleRequiredEnv PolicyRuleType = "requiredEnv"
	// RuleForbiddenEnv forbids any environment variable NAME matching a
	// pattern. Presence only; values are never read.
	RuleForbiddenEnv PolicyRuleType = "forbiddenEnv"
	// RuleRequiredCapabilities requires each capability to be granted.
	RuleRequiredCapabilities PolicyRuleType = "requiredCapabilities"
	// RuleForbiddenCapabilities forbids each capability from being granted.
	RuleForbiddenCapabilities PolicyRuleType = "forbiddenCapabilities"
	// RuleMemoryLimitRequired requires a memory limit.
	RuleMemoryLimitRequired PolicyRuleType = "memoryLimitRequired"
	// RuleCPULimitRequired requires a CPU limit.
	RuleCPULimitRequired PolicyRuleType = "cpuLimitRequired"
	// RuleRestartPolicyAllowlist requires the restart policy to be listed.
	RuleRestartPolicyAllowlist PolicyRuleType = "restartPolicyAllowlist"
	// RuleNetworkAllowlist requires every attached network to be listed.
	//
	// This is HarborMaster's realisation of a network-mode allowlist. Docker
	// surfaces the network mode as the container's attached networks -- a
	// host-mode container is attached to "host", an isolated one to "none",
	// the default to "bridge", and a Compose stack to its project network --
	// so matching the attachments is how the mode becomes observable. See
	// PolicyTarget.Networks for the one mode this cannot distinguish.
	RuleNetworkAllowlist PolicyRuleType = "networkAllowlist"
	// RuleUserNotRoot requires a non-root user.
	RuleUserNotRoot PolicyRuleType = "userNotRoot"
	// RuleHealthCheckRequired requires a configured, enabled health check.
	RuleHealthCheckRequired PolicyRuleType = "healthCheckRequired"
)

// PolicyRuleTypes lists every rule type, in catalogue order: roughly most to
// least security-relevant, which is the order a policy editor should offer
// them in.
var PolicyRuleTypes = []PolicyRuleType{
	RulePrivilegedForbidden,
	RuleForbiddenCapabilities,
	RuleUserNotRoot,
	RuleReadOnlyRootFilesystem,
	RuleImageAllowlist,
	RuleImageDenylist,
	RuleNetworkAllowlist,
	RuleForbiddenEnv,
	RuleRequiredCapabilities,
	RuleMemoryLimitRequired,
	RuleCPULimitRequired,
	RuleHealthCheckRequired,
	RuleRestartPolicyAllowlist,
	RuleRequiredLabels,
	RuleForbiddenLabels,
	RuleRequiredEnv,
}

// ValidPolicyRuleType reports whether name is a known rule type.
func ValidPolicyRuleType(name string) bool {
	_, ok := policyRuleCatalogue[PolicyRuleType(name)]
	return ok
}

// PolicyValueKind describes what a rule's Values mean, so a UI can label the
// input and the validator can apply the right checks.
type PolicyValueKind string

// Policy value kinds.
const (
	// ValueKindNone means the rule takes no parameters.
	ValueKindNone PolicyValueKind = "none"
	// ValueKindImagePattern, ValueKindLabelPattern and ValueKindEnvPattern are
	// all glob patterns; they are distinguished so a UI can say what is being
	// matched.
	ValueKindImagePattern PolicyValueKind = "imagePattern"
	ValueKindLabelPattern PolicyValueKind = "labelKeyPattern"
	ValueKindEnvPattern   PolicyValueKind = "envNamePattern"
	// ValueKindCapability is a Linux capability name, matched exactly after
	// normalisation rather than as a pattern: a wildcard over capability names
	// reads as precision it does not have.
	ValueKindCapability PolicyValueKind = "capability"
	// ValueKindRestartPolicy and ValueKindNetwork are exact names from small
	// vocabularies.
	ValueKindRestartPolicy PolicyValueKind = "restartPolicy"
	ValueKindNetwork       PolicyValueKind = "networkName"
)

// PolicyRuleSpec describes one rule type for validation and for the UI.
type PolicyRuleSpec struct {
	Type PolicyRuleType `json:"type"`
	// Label is a short human name, and Description states exactly what the
	// rule checks -- including where HarborMaster's view is narrower than the
	// daemon's, which is a thing the editor must say rather than imply.
	Label       string `json:"label"`
	Description string `json:"description"`

	ValueKind PolicyValueKind `json:"valueKind"`
	// RequiresValues is true when the rule is meaningless without parameters.
	// A rule of such a type carrying none is refused at write time.
	RequiresValues bool `json:"requiresValues"`
}

// policyRuleCatalogue is the closed vocabulary, keyed by type.
var policyRuleCatalogue = map[PolicyRuleType]PolicyRuleSpec{
	RulePrivilegedForbidden: {
		Type:        RulePrivilegedForbidden,
		Label:       "Privileged mode forbidden",
		Description: "The container must not run privileged. A privileged container has effectively lost the containment boundary.",
		ValueKind:   ValueKindNone,
	},
	RuleReadOnlyRootFilesystem: {
		Type:        RuleReadOnlyRootFilesystem,
		Label:       "Read-only root filesystem required",
		Description: "The container's root filesystem must be mounted read-only.",
		ValueKind:   ValueKindNone,
	},
	RuleImageAllowlist: {
		Type:           RuleImageAllowlist,
		Label:          "Image allowlist",
		Description:    "The image reference must match at least one pattern. Matched against the reference as declared, so a policy naming a registry only holds if references are written with it.",
		ValueKind:      ValueKindImagePattern,
		RequiresValues: true,
	},
	RuleImageDenylist: {
		Type:           RuleImageDenylist,
		Label:          "Image denylist",
		Description:    "The image reference must match no pattern.",
		ValueKind:      ValueKindImagePattern,
		RequiresValues: true,
	},
	RuleRequiredLabels: {
		Type:           RuleRequiredLabels,
		Label:          "Required labels",
		Description:    "Every pattern must be matched by at least one label key. Keys only: a label's value is not examined.",
		ValueKind:      ValueKindLabelPattern,
		RequiresValues: true,
	},
	RuleForbiddenLabels: {
		Type:           RuleForbiddenLabels,
		Label:          "Forbidden labels",
		Description:    "No label key may match any pattern.",
		ValueKind:      ValueKindLabelPattern,
		RequiresValues: true,
	},
	RuleRequiredEnv: {
		Type:           RuleRequiredEnv,
		Label:          "Required environment variables",
		Description:    "Every pattern must be matched by at least one environment variable NAME. Presence only -- HarborMaster never reads or stores a variable's value.",
		ValueKind:      ValueKindEnvPattern,
		RequiresValues: true,
	},
	RuleForbiddenEnv: {
		Type:           RuleForbiddenEnv,
		Label:          "Forbidden environment variables",
		Description:    "No environment variable NAME may match any pattern. Presence only -- HarborMaster never reads or stores a variable's value.",
		ValueKind:      ValueKindEnvPattern,
		RequiresValues: true,
	},
	RuleRequiredCapabilities: {
		Type:           RuleRequiredCapabilities,
		Label:          "Required capabilities",
		Description:    "Every capability must be explicitly granted through capAdd. HarborMaster reads the declared configuration and cannot see the daemon's default capability set, so a capability the daemon grants by default does not satisfy this rule.",
		ValueKind:      ValueKindCapability,
		RequiresValues: true,
	},
	RuleForbiddenCapabilities: {
		Type:           RuleForbiddenCapabilities,
		Label:          "Forbidden capabilities",
		Description:    "No capability may be granted through capAdd. HarborMaster reads the declared configuration; a capability the daemon grants by default is not visible to it.",
		ValueKind:      ValueKindCapability,
		RequiresValues: true,
	},
	RuleMemoryLimitRequired: {
		Type:        RuleMemoryLimitRequired,
		Label:       "Memory limit required",
		Description: "The container must declare a memory limit. A reservation is not a limit and does not satisfy this rule.",
		ValueKind:   ValueKindNone,
	},
	RuleCPULimitRequired: {
		Type:        RuleCPULimitRequired,
		Label:       "CPU limit required",
		Description: "The container must declare a CPU limit through nanoCpus or a cpu quota. CPU shares are a relative weight rather than a limit and do not satisfy this rule.",
		ValueKind:   ValueKindNone,
	},
	RuleRestartPolicyAllowlist: {
		Type:           RuleRestartPolicyAllowlist,
		Label:          "Restart policy allowlist",
		Description:    "The restart policy must be one of the listed names. A container with no policy configured is treated as \"no\".",
		ValueKind:      ValueKindRestartPolicy,
		RequiresValues: true,
	},
	RuleNetworkAllowlist: {
		Type:           RuleNetworkAllowlist,
		Label:          "Network allowlist",
		Description:    "Every attached network must be listed. This is how a network-mode allowlist is expressed: host mode appears as \"host\" and isolation as \"none\". A container attached to no network shares another container's namespace and never satisfies this rule.",
		ValueKind:      ValueKindNetwork,
		RequiresValues: true,
	},
	RuleUserNotRoot: {
		Type:        RuleUserNotRoot,
		Label:       "User must not be root",
		Description: "The container must declare a non-root user. A container that declares no user inherits the image's default, which is frequently root, so an unset user fails this rule.",
		ValueKind:   ValueKindNone,
	},
	RuleHealthCheckRequired: {
		Type:        RuleHealthCheckRequired,
		Label:       "Health check required",
		Description: "The container must have a health check that is configured and not disabled.",
		ValueKind:   ValueKindNone,
	},
}

// PolicyRuleCatalogue returns every rule type in catalogue order.
//
// Served to the frontend so the policy editor is built from the same source of
// truth the validator uses. A hand-maintained list in the UI would eventually
// offer a rule the backend rejects.
func PolicyRuleCatalogue() []PolicyRuleSpec {
	specs := make([]PolicyRuleSpec, 0, len(PolicyRuleTypes))
	for _, ruleType := range PolicyRuleTypes {
		specs = append(specs, policyRuleCatalogue[ruleType])
	}
	return specs
}

// RuleSpec returns the catalogue entry for a rule type.
func RuleSpec(ruleType PolicyRuleType) (PolicyRuleSpec, bool) {
	spec, ok := policyRuleCatalogue[ruleType]
	return spec, ok
}

// ---------------------------------------------------------------- target --

// PolicyTarget is the container configuration a policy is evaluated against.
//
// # Why this exists rather than passing ContainerDetail
//
// ContainerDetail.Environment carries EnvVar.RawValue, which holds the real
// value of a secret-bearing variable. The surest way to guarantee that a rule
// cannot read one is to hand the rule engine a structure that HAS NO FIELD
// holding it. EnvNames below is a list of names and nothing else, so "policies
// evaluate names only" is a property of the type rather than a discipline every
// future rule author has to remember.
//
// The same reasoning applies to labels: only keys are carried.
type PolicyTarget struct {
	ContainerID   string
	ContainerName string

	// Image is the reference as declared, and ImageRepository the same without
	// tag or digest. Both are offered so a pattern can be written either way.
	Image           string
	ImageRepository string

	// EnvNames holds variable NAMES ONLY, sorted. No value, masked or
	// otherwise, reaches this struct.
	EnvNames []string
	// LabelKeys holds label keys only, sorted.
	LabelKeys []string

	// CapabilitiesAdded holds normalised capability names from capAdd, and
	// CapabilitiesAll records that capAdd contained "ALL".
	CapabilitiesAdded []string
	CapabilitiesAll   bool

	// Networks holds the attached network names, sorted.
	//
	// EMPTY has meaning: a container in "container:<id>" network mode shares
	// another container's namespace and reports no attachment of its own.
	// HarborMaster cannot distinguish that from a container with no networking
	// at all, so the network rule treats both as failing an allowlist -- which
	// is the fail-closed direction.
	Networks []string

	// RestartPolicy is the policy name, normalised so an unset policy reads as
	// "no" rather than the empty string.
	RestartPolicy string
	// User is the declared user, empty when the container inherits the image
	// default.
	User string

	MemoryBytes int64
	NanoCPUs    int64
	CPUQuota    int64

	Privileged     bool
	ReadonlyRootfs bool
	HasHealthCheck bool
}

// NewPolicyTarget projects a container's current configuration into the
// evaluation input.
//
// The projection is where secret values are dropped, and it is the only place
// they could be carried across. Nothing below reads EnvVar.Value or
// EnvVar.RawValue.
func NewPolicyTarget(detail ContainerDetail) PolicyTarget {
	target := PolicyTarget{
		ContainerID:     detail.Overview.ID,
		ContainerName:   detail.Overview.Name,
		Image:           detail.Overview.Image.Raw,
		ImageRepository: detail.Overview.Image.Repository,
		RestartPolicy:   normaliseRestartPolicy(detail.Overview.RestartPolicy.Name),
		User:            strings.TrimSpace(detail.Process.User),
		MemoryBytes:     detail.Resources.MemoryBytes,
		NanoCPUs:        detail.Resources.NanoCPUs,
		CPUQuota:        detail.Resources.CPUQuota,
		Privileged:      detail.Security.Privileged,
		ReadonlyRootfs:  detail.Security.ReadonlyRootfs,
	}

	target.EnvNames = make([]string, 0, len(detail.Environment))
	for _, variable := range detail.Environment {
		// The NAME only. Deliberate, and the reason this loop does not simply
		// copy the slice.
		target.EnvNames = append(target.EnvNames, variable.Name)
	}
	sort.Strings(target.EnvNames)

	target.LabelKeys = make([]string, 0, len(detail.Labels))
	for _, label := range detail.Labels {
		target.LabelKeys = append(target.LabelKeys, label.Key)
	}
	sort.Strings(target.LabelKeys)

	target.CapabilitiesAdded = make([]string, 0, len(detail.Security.CapAdd))
	for _, capability := range detail.Security.CapAdd {
		normalised := NormaliseCapability(capability)
		if normalised == "ALL" {
			target.CapabilitiesAll = true
			continue
		}
		if normalised != "" {
			target.CapabilitiesAdded = append(target.CapabilitiesAdded, normalised)
		}
	}
	sort.Strings(target.CapabilitiesAdded)

	target.Networks = make([]string, 0, len(detail.Networks))
	for _, attachment := range detail.Networks {
		if attachment.NetworkName != "" {
			target.Networks = append(target.Networks, attachment.NetworkName)
		}
	}
	sort.Strings(target.Networks)

	// A health check exists when it is configured, not disabled, and actually
	// carries a test. Docker represents "the image has one but this container
	// turned it off" as a test of exactly ["NONE"], which is not a health
	// check however it is spelled.
	if check := detail.HealthCheck; check != nil {
		target.HasHealthCheck = !check.Disabled &&
			len(check.Test) > 0 &&
			(len(check.Test) != 1 || !strings.EqualFold(check.Test[0], "NONE"))
	}

	return target
}

// NormaliseCapability puts a capability name into comparable form.
//
// Docker accepts both "CAP_SYS_ADMIN" and "SYS_ADMIN" and is case-insensitive,
// so a policy naming one form must match a container declaring the other.
// Normalising both sides is the only way that holds.
func NormaliseCapability(name string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(name))
	return strings.TrimPrefix(trimmed, "CAP_")
}

// Grants reports whether the container's DECLARED configuration grants a
// capability.
//
// Declared, not effective. HarborMaster reads what the container asked for; the
// daemon's default capability set is not part of any inspect response, so a
// capability granted by default is invisible here. That is stated in the rule
// descriptions and in the violation's reason rather than papered over with a
// hardcoded default list, which would be a claim about a daemon HarborMaster
// has not asked.
//
// capAdd wins over capDrop, matching how the daemon composes the two.
func (t PolicyTarget) Grants(capability string) bool {
	if t.CapabilitiesAll {
		return true
	}
	wanted := NormaliseCapability(capability)
	for _, granted := range t.CapabilitiesAdded {
		if granted == wanted {
			return true
		}
	}
	return false
}

// normaliseRestartPolicy renders an unset policy as Docker's default.
func normaliseRestartPolicy(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "no"
	}
	return trimmed
}

// IsRootUser reports whether a declared user string names root.
//
// An EMPTY user counts as root. The container inherits the image's default,
// which is root unless the image says otherwise, and HarborMaster cannot see
// what the image says. Reporting "unknown, assume fine" would make the rule
// pass for exactly the containers most likely to fail it.
func IsRootUser(user string) bool {
	trimmed := strings.TrimSpace(user)
	if trimmed == "" {
		return true
	}
	// "user:group" -- only the user half decides.
	if colon := strings.Index(trimmed, ":"); colon >= 0 {
		trimmed = trimmed[:colon]
	}
	return strings.EqualFold(trimmed, "root") || trimmed == "0"
}

// ------------------------------------------------------------- evaluation --

// PolicyRuleResult is one rule's verdict against one container.
type PolicyRuleResult struct {
	Compliant bool
	// Observed renders what the container actually has. NEVER an environment
	// variable value: the env rules render names, because names are all the
	// target carries.
	Observed string
	// Expected renders what the rule required, from the rule's own parameters.
	Expected string
	// Reason is prose from the fixed set below, explaining the failure.
	Reason string
}

// EvaluatePolicyRule applies one rule to one container.
//
// Pure and total: every rule type in the catalogue is handled, and an unknown
// type -- which validation makes unreachable -- returns COMPLIANT rather than
// failing. That direction is deliberate. A rule the engine does not understand
// has established nothing, and manufacturing a violation from ignorance would
// put an unfixable row on an operator's dashboard.
func EvaluatePolicyRule(rule PolicyRule, target PolicyTarget) PolicyRuleResult {
	switch rule.Type {
	case RulePrivilegedForbidden:
		return boolRule(!target.Privileged, "privileged", target.Privileged,
			"the container runs privileged, which removes the containment boundary")

	case RuleReadOnlyRootFilesystem:
		return boolRule(target.ReadonlyRootfs, "readOnlyRootFilesystem", target.ReadonlyRootfs,
			"the container's root filesystem is writable")

	case RuleImageAllowlist:
		return evaluateImageAllowlist(rule, target)
	case RuleImageDenylist:
		return evaluateImageDenylist(rule, target)

	case RuleRequiredLabels:
		return evaluateRequiredPatterns(rule.Values, target.LabelKeys,
			"no label key matches")
	case RuleForbiddenLabels:
		return evaluateForbiddenPatterns(rule.Values, target.LabelKeys,
			"label key")

	case RuleRequiredEnv:
		return evaluateRequiredPatterns(rule.Values, target.EnvNames,
			"no environment variable is named")
	case RuleForbiddenEnv:
		return evaluateForbiddenPatterns(rule.Values, target.EnvNames,
			"environment variable")

	case RuleRequiredCapabilities:
		return evaluateRequiredCapabilities(rule, target)
	case RuleForbiddenCapabilities:
		return evaluateForbiddenCapabilities(rule, target)

	case RuleMemoryLimitRequired:
		if target.MemoryBytes > 0 {
			return PolicyRuleResult{Compliant: true}
		}
		return PolicyRuleResult{
			Observed: "no memory limit",
			Expected: "a memory limit",
			Reason:   "the container declares no memory limit, so it can consume the host's memory",
		}

	case RuleCPULimitRequired:
		if target.NanoCPUs > 0 || target.CPUQuota > 0 {
			return PolicyRuleResult{Compliant: true}
		}
		return PolicyRuleResult{
			Observed: "no cpu limit",
			Expected: "nanoCpus or a cpu quota",
			Reason:   "the container declares no cpu limit; cpu shares are a relative weight rather than a cap",
		}

	case RuleRestartPolicyAllowlist:
		return evaluateExactAllowlist(rule.Values, []string{target.RestartPolicy},
			"restart policy")

	case RuleNetworkAllowlist:
		return evaluateNetworkAllowlist(rule, target)

	case RuleUserNotRoot:
		return evaluateUserNotRoot(target)

	case RuleHealthCheckRequired:
		if target.HasHealthCheck {
			return PolicyRuleResult{Compliant: true}
		}
		return PolicyRuleResult{
			Observed: "no health check",
			Expected: "a configured health check",
			Reason:   "the container has no health check, or its health check is disabled",
		}
	}

	return PolicyRuleResult{Compliant: true}
}

// boolRule renders the verdict for the three parameterless boolean rules.
func boolRule(compliant bool, field string, observed bool, reason string) PolicyRuleResult {
	if compliant {
		return PolicyRuleResult{Compliant: true}
	}
	return PolicyRuleResult{
		Observed: field + "=" + strconv.FormatBool(observed),
		Expected: field + "=" + strconv.FormatBool(!observed),
		Reason:   reason,
	}
}

func evaluateImageAllowlist(rule PolicyRule, target PolicyTarget) PolicyRuleResult {
	// Either form may match, so a policy can name a repository without having
	// to enumerate every tag, or pin an exact reference when it wants to.
	for _, subject := range []string{target.Image, target.ImageRepository} {
		if subject == "" {
			continue
		}
		if _, ok := MatchAnyGlob(rule.Values, subject); ok {
			return PolicyRuleResult{Compliant: true}
		}
	}
	return PolicyRuleResult{
		Observed: renderObserved([]string{target.Image}),
		Expected: renderExpected(rule.Values),
		Reason:   "the image reference matches no allowed pattern",
	}
}

func evaluateImageDenylist(rule PolicyRule, target PolicyTarget) PolicyRuleResult {
	for _, subject := range []string{target.Image, target.ImageRepository} {
		if subject == "" {
			continue
		}
		if _, ok := MatchAnyGlob(rule.Values, subject); ok {
			return PolicyRuleResult{
				Observed: renderObserved([]string{target.Image}),
				Expected: "no match against " + renderExpected(rule.Values),
				Reason:   "the image reference matches a denied pattern",
			}
		}
	}
	return PolicyRuleResult{Compliant: true}
}

// evaluateRequiredPatterns requires EVERY pattern to be matched by at least one
// subject, and names the patterns that were not.
func evaluateRequiredPatterns(patterns, subjects []string, phrase string) PolicyRuleResult {
	missing := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		matched := false
		for _, subject := range subjects {
			if MatchGlob(pattern, subject) {
				matched = true
				break
			}
		}
		if !matched {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		return PolicyRuleResult{Compliant: true}
	}
	return PolicyRuleResult{
		Observed: "missing " + renderObserved(missing),
		Expected: renderExpected(patterns),
		Reason:   phrase + " " + renderObserved(missing),
	}
}

// evaluateForbiddenPatterns fails when ANY subject matches ANY pattern, and
// names the offending SUBJECTS.
//
// For the environment rule those subjects are variable names, which is the
// whole point: an operator needs to know which variable is the problem, and a
// name is safe to report where a value would never be.
func evaluateForbiddenPatterns(patterns, subjects []string, noun string) PolicyRuleResult {
	offending := make([]string, 0, 4)
	for _, subject := range subjects {
		if _, ok := MatchAnyGlob(patterns, subject); ok {
			offending = append(offending, subject)
		}
	}
	if len(offending) == 0 {
		return PolicyRuleResult{Compliant: true}
	}
	return PolicyRuleResult{
		Observed: renderObserved(offending),
		Expected: "no " + noun + " matching " + renderExpected(patterns),
		Reason:   "a forbidden " + noun + " is present: " + renderObserved(offending),
	}
}

// evaluateExactAllowlist requires every subject to appear in the allowlist,
// compared exactly rather than as a pattern.
func evaluateExactAllowlist(allowed, subjects []string, noun string) PolicyRuleResult {
	offending := make([]string, 0, 4)
	for _, subject := range subjects {
		if !containsFold(allowed, subject) {
			offending = append(offending, subject)
		}
	}
	if len(offending) == 0 {
		return PolicyRuleResult{Compliant: true}
	}
	return PolicyRuleResult{
		Observed: renderObserved(offending),
		Expected: renderExpected(allowed),
		Reason:   "the " + noun + " is not allowed: " + renderObserved(offending),
	}
}

func evaluateNetworkAllowlist(rule PolicyRule, target PolicyTarget) PolicyRuleResult {
	// A container with no attachment shares another container's network
	// namespace, or has none at all. Neither is on an allowlist, and treating
	// "nothing to check" as compliant would let the one mode HarborMaster
	// cannot see through be the one that always passes.
	if len(target.Networks) == 0 {
		return PolicyRuleResult{
			Observed: "no attached network",
			Expected: renderExpected(rule.Values),
			Reason: "the container is attached to no network of its own, which means " +
				"it shares another container's network namespace",
		}
	}
	return evaluateExactAllowlist(rule.Values, target.Networks, "network")
}

func evaluateRequiredCapabilities(rule PolicyRule, target PolicyTarget) PolicyRuleResult {
	missing := make([]string, 0, len(rule.Values))
	for _, capability := range rule.Values {
		if !target.Grants(capability) {
			missing = append(missing, NormaliseCapability(capability))
		}
	}
	if len(missing) == 0 {
		return PolicyRuleResult{Compliant: true}
	}
	return PolicyRuleResult{
		Observed: "missing " + renderObserved(missing),
		Expected: renderExpected(rule.Values),
		Reason: "the container does not explicitly grant " + renderObserved(missing) +
			"; HarborMaster reads capAdd and cannot see the daemon's default capability set",
	}
}

func evaluateForbiddenCapabilities(rule PolicyRule, target PolicyTarget) PolicyRuleResult {
	granted := make([]string, 0, len(rule.Values))
	for _, capability := range rule.Values {
		if target.Grants(capability) {
			granted = append(granted, NormaliseCapability(capability))
		}
	}
	if len(granted) == 0 {
		return PolicyRuleResult{Compliant: true}
	}

	reason := "the container grants " + renderObserved(granted)
	if target.CapabilitiesAll {
		reason = "the container grants ALL capabilities, which includes " + renderObserved(granted)
	}
	return PolicyRuleResult{
		Observed: renderObserved(granted),
		Expected: "none of " + renderExpected(rule.Values),
		Reason:   reason,
	}
}

func evaluateUserNotRoot(target PolicyTarget) PolicyRuleResult {
	if !IsRootUser(target.User) {
		return PolicyRuleResult{Compliant: true}
	}
	if target.User == "" {
		return PolicyRuleResult{
			Observed: "no user declared",
			Expected: "a non-root user",
			Reason: "the container declares no user, so it inherits the image's default, " +
				"which is root unless the image says otherwise",
		}
	}
	return PolicyRuleResult{
		Observed: renderObserved([]string{target.User}),
		Expected: "a non-root user",
		Reason:   "the container runs as root",
	}
}

// containsFold reports whether values contains subject, case-insensitively.
//
// Case-insensitive because restart policy and network names are compared for
// operator intent rather than byte identity, and "Always" meaning something
// different from "always" would be a trap rather than a feature.
func containsFold(values []string, subject string) bool {
	for _, value := range values {
		if strings.EqualFold(value, subject) {
			return true
		}
	}
	return false
}

// maxRenderedItems and maxRenderedBytes bound a rendered value list.
//
// A container with four hundred labels matching a forbidden pattern must not
// produce a four-hundred-item string that is then stored on every evaluation,
// served to every dashboard, and rendered into a table cell. The overflow is
// COUNTED rather than dropped silently, so the row still says how much it is
// not showing.
const (
	maxRenderedItems = 8
	maxRenderedBytes = 512
)

// renderObserved renders a bounded, comma-separated list for storage and
// display.
func renderObserved(values []string) string {
	if len(values) == 0 {
		return ""
	}

	shown := values
	overflow := 0
	if len(shown) > maxRenderedItems {
		overflow = len(shown) - maxRenderedItems
		shown = shown[:maxRenderedItems]
	}

	rendered := strings.Join(shown, ", ")
	if len(rendered) > maxRenderedBytes {
		// Cut on a rune boundary so the stored text stays valid UTF-8.
		cut := maxRenderedBytes
		for cut > 0 && !utf8RuneStart(rendered[cut]) {
			cut--
		}
		rendered = rendered[:cut] + "…"
	}
	if overflow > 0 {
		rendered += " (and " + strconv.Itoa(overflow) + " more)"
	}
	return rendered
}

// renderExpected renders a rule's parameters for display.
func renderExpected(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return "one of " + renderObserved(values)
}

// utf8RuneStart reports whether b begins a UTF-8 encoded rune.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
