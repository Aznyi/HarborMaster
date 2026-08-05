package api

import (
	"net/url"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Policy query parsing and identifier validation.
//
// Every parameter is validated against a CLOSED VOCABULARY defined in the
// domain package, or is a bounded integer, or is an identifier matching a fixed
// shape. Nothing a caller sends becomes SQL text: a rule type, severity or
// status is matched against the domain's allowlist and then travels as a bound
// parameter, and the sort field selects a compile-time column constant from a
// map.
//
// Rejected rather than clamped, matching the rest of the API: silently serving
// something other than what was asked hides a bug in the caller.

// policyIDPrefix and policyIDHexLength describe a generated policy identifier.
//
// Validated by SHAPE before it reaches a query. The id is server-generated from
// the system entropy source, so any well-formed one is either real or a miss --
// there is no reason to accept anything else, and refusing early keeps
// arbitrary caller text out of the database layer entirely.
const (
	policyIDPrefix     = "pol_"
	policyIDHexLength  = 20
	policyIDTotalBytes = len(policyIDPrefix) + policyIDHexLength
)

// validPolicyID reports whether id has the shape of a generated policy id.
func validPolicyID(id string) bool {
	if len(id) != policyIDTotalBytes || !strings.HasPrefix(id, policyIDPrefix) {
		return false
	}
	for _, r := range id[len(policyIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// policyQuery is a parsed and validated policy listing request.
type policyQuery struct {
	EnabledOnly     bool
	IncludeArchived bool
	Search          string

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// maxPolicySearchBytes bounds the free-text search term.
//
// The term becomes a bound LIKE pattern with its metacharacters escaped, so it
// cannot alter the statement; the bound exists because a very long pattern
// makes the match itself expensive.
const maxPolicySearchBytes = 200

// parsePolicyQuery reads and validates the policy list parameters.
func parsePolicyQuery(query url.Values) (policyQuery, error) {
	// Ascending by name: a policy list is a catalogue an operator reads
	// alphabetically, not a feed of recent events.
	parsed := policyQuery{Sort: "name", Ascending: true}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidPolicySortField(raw) {
			return parsed, invalidParam("sort",
				"one of name, severity, createdAt, updatedAt, rules, id")
		}
		parsed.Sort = raw
	}

	if raw := strings.TrimSpace(query.Get("order")); raw != "" {
		switch raw {
		case "asc":
			parsed.Ascending = true
		case "desc":
			parsed.Ascending = false
		default:
			return parsed, invalidParam("order", "asc or desc")
		}
	}

	if raw := strings.TrimSpace(query.Get("enabled")); raw != "" {
		switch raw {
		case "true":
			parsed.EnabledOnly = true
		case "false":
			parsed.EnabledOnly = false
		default:
			return parsed, invalidParam("enabled", "true or false")
		}
	}

	// Archived policies are excluded by default. An archived policy is
	// history; a list dominated by history buries the rules actually in force.
	if raw := strings.TrimSpace(query.Get("includeArchived")); raw != "" {
		switch raw {
		case "true":
			parsed.IncludeArchived = true
		case "false":
			parsed.IncludeArchived = false
		default:
			return parsed, invalidParam("includeArchived", "true or false")
		}
	}

	if raw := strings.TrimSpace(query.Get("search")); raw != "" {
		if len(raw) > maxPolicySearchBytes {
			return parsed, invalidParam("search", "at most 200 characters")
		}
		parsed.Search = raw
	}

	return parsed, nil
}

// filter converts a validated query into a repository filter.
func (q policyQuery) filter() store.PolicyFilter {
	return store.PolicyFilter{
		EnabledOnly:     q.EnabledOnly,
		IncludeArchived: q.IncludeArchived,
		Search:          q.Search,
		Sort:            q.Sort,
		Ascending:       q.Ascending,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}

// violationQuery is a parsed and validated violation listing request.
type violationQuery struct {
	ContainerID string
	PolicyID    string
	RuleTypes   []domain.PolicyRuleType
	Severities  []domain.PolicySeverity
	Statuses    []domain.PolicyViolationStatus
	OpenOnly    bool

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// parseViolationQuery reads and validates the violation list parameters.
func parseViolationQuery(query url.Values) (violationQuery, error) {
	parsed := violationQuery{Sort: "detectedAt"}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidPolicyViolationSortField(raw) {
			return parsed, invalidParam("sort",
				"one of detectedAt, lastSeenAt, id, container, policy, rule, status, severity")
		}
		parsed.Sort = raw
	}

	if raw := strings.TrimSpace(query.Get("order")); raw != "" {
		switch raw {
		case "asc":
			parsed.Ascending = true
		case "desc":
			parsed.Ascending = false
		default:
			return parsed, invalidParam("order", "asc or desc")
		}
	}

	// The container id is matched exactly against a stored value and travels
	// as a bound parameter. It is length-bounded so a caller cannot make the
	// comparison itself expensive.
	if raw := strings.TrimSpace(query.Get("containerId")); raw != "" {
		if len(raw) > 128 {
			return parsed, invalidParam("containerId", "at most 128 characters")
		}
		parsed.ContainerID = raw
	}

	if raw := strings.TrimSpace(query.Get("policyId")); raw != "" {
		if !validPolicyID(raw) {
			return parsed, invalidParam("policyId", "a policy identifier")
		}
		parsed.PolicyID = raw
	}

	// openOnly defaults to TRUE. A dashboard dominated by resolved history
	// would bury the failures that still stand, and "what is failing now" is
	// the question the endpoint exists to answer.
	parsed.OpenOnly = true
	if raw := strings.TrimSpace(query.Get("openOnly")); raw != "" {
		switch raw {
		case "true":
			parsed.OpenOnly = true
		case "false":
			parsed.OpenOnly = false
		default:
			return parsed, invalidParam("openOnly", "true or false")
		}
	}

	ruleTypes, err := parseVocabulary(query, "rule", domain.ValidPolicyRuleType,
		"a known policy rule type")
	if err != nil {
		return parsed, err
	}
	for _, value := range ruleTypes {
		parsed.RuleTypes = append(parsed.RuleTypes, domain.PolicyRuleType(value))
	}

	severities, err := parseVocabulary(query, "severity", domain.ValidPolicySeverity,
		"one of critical, high, medium, low")
	if err != nil {
		return parsed, err
	}
	for _, value := range severities {
		parsed.Severities = append(parsed.Severities, domain.PolicySeverity(value))
	}

	statuses, err := parseVocabulary(query, "status", domain.ValidPolicyViolationStatus,
		"one of active, resolved, acknowledged, exempted")
	if err != nil {
		return parsed, err
	}
	for _, value := range statuses {
		parsed.Statuses = append(parsed.Statuses, domain.PolicyViolationStatus(value))
	}

	// An explicit status filter overrides the openOnly default: asking for
	// resolved violations and being handed none would be the API contradicting
	// itself.
	if len(parsed.Statuses) > 0 && strings.TrimSpace(query.Get("openOnly")) == "" {
		parsed.OpenOnly = false
	}

	return parsed, nil
}

// filter converts a validated query into a repository filter.
func (q violationQuery) filter() store.PolicyViolationFilter {
	return store.PolicyViolationFilter{
		ContainerID: q.ContainerID,
		PolicyID:    q.PolicyID,
		RuleTypes:   q.RuleTypes,
		Severities:  q.Severities,
		Statuses:    q.Statuses,
		OpenOnly:    q.OpenOnly,
		Sort:        q.Sort,
		Ascending:   q.Ascending,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}
