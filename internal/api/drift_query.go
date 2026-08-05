package api

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Drift query parsing.
//
// Every parameter is validated against a CLOSED VOCABULARY defined in the
// domain package, or is a bounded integer. Nothing a caller sends becomes SQL
// text: a category, severity or status is matched against the domain's
// allowlist and then travels as a bound parameter, and the sort field selects
// a compile-time column constant from a map.
//
// Rejected rather than clamped, matching the rest of the API: silently serving
// something other than what was asked hides a bug in the caller.

// maxDriftFilterValues bounds how many values one repeatable filter may carry.
//
// Without it a caller could send a thousand `severity=` parameters and make
// the IN clause -- and its placeholder run -- proportional to the request
// rather than to the vocabulary. The vocabularies are all far smaller than
// this, so the cap can never reject a legitimate query.
const maxDriftFilterValues = 32

// driftQuery is a parsed and validated drift listing request.
type driftQuery struct {
	ContainerID string
	SnapshotID  int64
	Categories  []domain.DriftCategory
	Severities  []domain.DriftSeverity
	Statuses    []domain.DriftStatus
	OpenOnly    bool

	Sort      string
	Ascending bool
	Page      int
	PageSize  int
}

// parseDriftQuery reads and validates the drift list parameters.
func parseDriftQuery(query url.Values) (driftQuery, error) {
	parsed := driftQuery{Sort: "detectedAt"}

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidDriftSortField(raw) {
			return parsed, invalidParam("sort",
				"one of detectedAt, lastSeenAt, id, container, category, field, status, severity")
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

	if raw := strings.TrimSpace(query.Get("snapshotId")); raw != "" {
		id, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil || id < 1 {
			return parsed, invalidParam("snapshotId", "a positive integer")
		}
		parsed.SnapshotID = id
	}

	// openOnly defaults to TRUE. A dashboard dominated by resolved history
	// would bury the differences that still stand, and "what is wrong now" is
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

	categories, err := parseVocabulary(query, "category", domain.ValidDriftCategory,
		"one of "+joinCategories())
	if err != nil {
		return parsed, err
	}
	for _, value := range categories {
		parsed.Categories = append(parsed.Categories, domain.DriftCategory(value))
	}

	severities, err := parseVocabulary(query, "severity", domain.ValidDriftSeverity,
		"one of critical, high, medium, low")
	if err != nil {
		return parsed, err
	}
	for _, value := range severities {
		parsed.Severities = append(parsed.Severities, domain.DriftSeverity(value))
	}

	statuses, err := parseVocabulary(query, "status", domain.ValidDriftStatus,
		"one of active, resolved, acknowledged, ignored, expected")
	if err != nil {
		return parsed, err
	}
	for _, value := range statuses {
		parsed.Statuses = append(parsed.Statuses, domain.DriftStatus(value))
	}

	// An explicit status filter overrides the openOnly default: asking for
	// resolved records and being handed none would be the API contradicting
	// itself.
	if len(parsed.Statuses) > 0 && strings.TrimSpace(query.Get("openOnly")) == "" {
		parsed.OpenOnly = false
	}

	return parsed, nil
}

// parseVocabulary reads a repeatable parameter whose values must all belong to
// a closed vocabulary.
//
// Accepts both repeated parameters (?severity=high&severity=low) and a
// comma-separated list (?severity=high,low), because both are natural to write
// and neither costs anything to support. Every value is validated; an
// unrecognised one is an error rather than being dropped, so a typo produces a
// message rather than a silently different result set.
func parseVocabulary(query url.Values, name string, valid func(string) bool, expected string) ([]string, error) {
	raw := query[name]
	if len(raw) == 0 {
		return nil, nil
	}

	values := make([]string, 0, len(raw))
	for _, entry := range raw {
		for _, part := range strings.Split(entry, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if len(values) >= maxDriftFilterValues {
				return nil, invalidParam(name, "at most 32 values")
			}
			if !valid(trimmed) {
				// The offending value is deliberately NOT echoed: an error
				// message is not the place to reflect caller input.
				return nil, invalidParam(name, expected)
			}
			values = append(values, trimmed)
		}
	}
	return values, nil
}

// joinCategories renders the category vocabulary for an error message.
func joinCategories() string {
	names := make([]string, 0, len(domain.DriftCategories))
	for _, category := range domain.DriftCategories {
		names = append(names, string(category))
	}
	return strings.Join(names, ", ")
}

// filter converts a validated query into a repository filter.
func (q driftQuery) filter() store.DriftFilter {
	return store.DriftFilter{
		ContainerID: q.ContainerID,
		SnapshotID:  q.SnapshotID,
		Categories:  q.Categories,
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
