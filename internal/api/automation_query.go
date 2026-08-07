package api

import (
	"net/url"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Automation query parsing.
//
// Every parameter is validated against a CLOSED VOCABULARY defined in the
// domain package, or is a bounded integer, or is an identifier matching a fixed
// shape. Nothing a caller sends becomes SQL text: a verdict, a state, or a
// trigger is matched against the domain's allowlist and then travels as a bound
// parameter, and the sort column selects a compile-time constant from a map.
//
// Rejected rather than clamped, matching the rest of the API: silently serving
// something other than what was asked hides a bug in the caller.

// maxAutomationFilterValues bounds a repeated query parameter.
//
// A filter with a hundred verdicts is not a filter; it is a way to make one
// request build a hundred-placeholder statement. The vocabularies themselves
// are short, so any value above their size is a caller error.
const maxAutomationFilterValues = 16

// automationRunQuery is a parsed and validated run listing request.
type automationRunQuery struct {
	States    []domain.AutomationRunState
	Triggers  []domain.AutomationTrigger
	ActedOnly bool

	Page     int
	PageSize int
}

// parseAutomationRunQuery reads and validates the run list parameters.
func parseAutomationRunQuery(query url.Values) (automationRunQuery, error) {
	var parsed automationRunQuery

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	states, err := parseCSVAllowlist(query, "state", validAutomationRunState,
		"one of running, completed, failed, interrupted")
	if err != nil {
		return parsed, err
	}
	for _, state := range states {
		parsed.States = append(parsed.States, domain.AutomationRunState(state))
	}

	triggers, err := parseCSVAllowlist(query, "trigger", domain.ValidAutomationTrigger,
		"one of schedule, manual, dryRun, startup")
	if err != nil {
		return parsed, err
	}
	for _, trigger := range triggers {
		parsed.Triggers = append(parsed.Triggers, domain.AutomationTrigger(trigger))
	}

	if raw := strings.TrimSpace(query.Get("acted")); raw != "" {
		switch raw {
		case "true":
			parsed.ActedOnly = true
		case "false":
			parsed.ActedOnly = false
		default:
			return parsed, invalidParam("acted", "true or false")
		}
	}

	return parsed, nil
}

// filter converts a validated query into a repository filter.
func (q automationRunQuery) filter() store.AutomationRunFilter {
	return store.AutomationRunFilter{
		States:    q.States,
		Triggers:  q.Triggers,
		ActedOnly: q.ActedOnly,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}

// updatePolicyQuery is a parsed and validated policy listing request.
type updatePolicyQuery struct {
	EnabledOnly     bool
	IncludeArchived bool
	Modes           []domain.AutomationMode
	Search          string

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// maxUpdatePolicySearchBytes bounds the free-text search term.
//
// The term becomes a bound LIKE pattern with its metacharacters escaped, so it
// cannot alter the statement; the bound exists because a very long pattern
// makes the match itself expensive.
const maxUpdatePolicySearchBytes = 200

// parseUpdatePolicyQuery reads and validates the policy list parameters.
func parseUpdatePolicyQuery(query url.Values) (updatePolicyQuery, error) {
	// Descending by priority: the rule that wins a tie is the one an operator
	// most wants at the top, and priority order is how the scheduler reads
	// them.
	parsed := updatePolicyQuery{Sort: "priority"}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidUpdatePolicySortField(raw) {
			return parsed, invalidParam("sort",
				"one of name, priority, createdAt, updatedAt, mode, strategy, id")
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

	modes, err := parseCSVAllowlist(query, "mode", domain.ValidAutomationMode,
		"one of observe, dryRun, approvalRequired, automatic")
	if err != nil {
		return parsed, err
	}
	for _, mode := range modes {
		parsed.Modes = append(parsed.Modes, domain.AutomationMode(mode))
	}

	if raw := strings.TrimSpace(query.Get("search")); raw != "" {
		if len(raw) > maxUpdatePolicySearchBytes {
			return parsed, invalidParam("search", "at most 200 characters")
		}
		parsed.Search = raw
	}

	return parsed, nil
}

// filter converts a validated query into a repository filter.
func (q updatePolicyQuery) filter() store.UpdatePolicyFilter {
	return store.UpdatePolicyFilter{
		EnabledOnly:     q.EnabledOnly,
		IncludeArchived: q.IncludeArchived,
		Modes:           q.Modes,
		Search:          q.Search,
		Sort:            q.Sort,
		Ascending:       q.Ascending,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}

// parseCSVAllowlist reads a repeated-or-comma-separated parameter and checks
// every value against an allowlist.
//
// Bounded in COUNT as well as in vocabulary. The count bound is what stops a
// caller building an arbitrarily long IN clause; the vocabulary bound is what
// stops any of its values being anything but a compile-time constant.
func parseCSVAllowlist(
	query url.Values,
	name string,
	valid func(string) bool,
	expectation string,
) ([]string, error) {
	raw := query[name]
	if len(raw) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, 4)
	values := make([]string, 0, 4)
	for _, entry := range raw {
		for _, candidate := range strings.Split(entry, ",") {
			trimmed := strings.TrimSpace(candidate)
			if trimmed == "" {
				continue
			}
			if !valid(trimmed) {
				return nil, invalidParam(name, expectation)
			}
			if _, duplicate := seen[trimmed]; duplicate {
				continue
			}
			if len(values) >= maxAutomationFilterValues {
				return nil, invalidParam(name, "at most 16 values")
			}
			seen[trimmed] = struct{}{}
			values = append(values, trimmed)
		}
	}
	return values, nil
}

// validAutomationRunState reports whether a value names a run state.
func validAutomationRunState(value string) bool {
	switch domain.AutomationRunState(value) {
	case domain.RunRunning, domain.RunCompleted, domain.RunFailed, domain.RunInterrupted:
		return true
	default:
		return false
	}
}
