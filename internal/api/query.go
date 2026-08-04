package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Pagination bounds. MaxPageSize caps how much work one request can ask the
// database and the JSON encoder to do.
const (
	DefaultPageSize = 25
	MaxPageSize     = 200
)

// queryError is a validation failure with a client-safe message.
//
// It carries only the parameter name and what was expected -- never the
// offending value, which could be anything a caller sent.
type queryError struct {
	message string
}

func (e queryError) Error() string { return e.message }

func invalidParam(name, expected string) error {
	return queryError{message: fmt.Sprintf("%s must be %s", name, expected)}
}

// parsePage reads page and pageSize.
//
// Both are rejected rather than clamped when out of range: silently serving
// page 1 to a client that asked for page -3 hides a bug in the caller.
func parsePage(query url.Values) (page int, pageSize int, err error) {
	page = 1
	pageSize = DefaultPageSize

	if raw := strings.TrimSpace(query.Get("page")); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 1 {
			return 0, 0, invalidParam("page", "a positive integer")
		}
		page = parsed
	}

	if raw := strings.TrimSpace(query.Get("pageSize")); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 1 {
			return 0, 0, invalidParam("pageSize", "a positive integer")
		}
		if parsed > MaxPageSize {
			return 0, 0, invalidParam("pageSize", fmt.Sprintf("at most %d", MaxPageSize))
		}
		pageSize = parsed
	}
	return page, pageSize, nil
}

// parseContainerFilter builds a repository filter from the query string.
//
// Every enumerated value is validated against the domain's closed vocabulary,
// and the sort field against the repository's allowlist, so nothing
// caller-controlled can reach the SQL builder as an identifier.
func parseContainerFilter(query url.Values) (store.ContainerFilter, error) {
	page, pageSize, err := parsePage(query)
	if err != nil {
		return store.ContainerFilter{}, err
	}

	filter := store.ContainerFilter{
		Search:         strings.TrimSpace(query.Get("search")),
		ComposeProject: strings.TrimSpace(query.Get("project")),
		ComposeService: strings.TrimSpace(query.Get("service")),
		Image:          strings.TrimSpace(query.Get("image")),
		RestartPolicy:  strings.TrimSpace(query.Get("restartPolicy")),
		LabelKey:       strings.TrimSpace(query.Get("labelKey")),
		LabelValue:     strings.TrimSpace(query.Get("labelValue")),
		Sort:           "name",
		Direction:      store.SortAsc,
		Page:           store.Page{Limit: pageSize, Offset: (page - 1) * pageSize},
	}

	for _, raw := range multiValue(query, "state") {
		if !domain.ValidContainerState(raw) {
			return store.ContainerFilter{}, invalidParam("state", "a known container state")
		}
		filter.States = append(filter.States, domain.ContainerState(raw))
	}

	for _, raw := range multiValue(query, "health") {
		if !domain.ValidHealthState(raw) {
			return store.ContainerFilter{}, invalidParam("health", "a known health state")
		}
		filter.Health = append(filter.Health, domain.HealthState(raw))
	}

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidSortField(raw) {
			return store.ContainerFilter{}, invalidParam("sort",
				"one of "+strings.Join(store.SortFields(), ", "))
		}
		filter.Sort = raw
	}

	if raw := strings.TrimSpace(query.Get("direction")); raw != "" {
		switch strings.ToLower(raw) {
		case "asc":
			filter.Direction = store.SortAsc
		case "desc":
			filter.Direction = store.SortDesc
		default:
			return store.ContainerFilter{}, invalidParam("direction", "asc or desc")
		}
	}

	if raw := strings.TrimSpace(query.Get("harbormasterEnabled")); raw != "" {
		parsed, convErr := strconv.ParseBool(raw)
		if convErr != nil {
			return store.ContainerFilter{}, invalidParam("harbormasterEnabled", "true or false")
		}
		filter.HarborMasterEnabled = &parsed
	}

	if raw := strings.TrimSpace(query.Get("includeAbsent")); raw != "" {
		parsed, convErr := strconv.ParseBool(raw)
		if convErr != nil {
			return store.ContainerFilter{}, invalidParam("includeAbsent", "true or false")
		}
		filter.IncludeAbsent = parsed
	}

	// A label value without a key would silently match nothing, which reads as
	// "no results" rather than "your query was wrong".
	if filter.LabelValue != "" && filter.LabelKey == "" {
		return store.ContainerFilter{}, queryError{message: "labelValue requires labelKey"}
	}

	return filter, nil
}

// parseEventFilter builds a Docker event filter from the query string.
//
// Same discipline as parseContainerFilter: enumerated values are checked
// against the domain's closed vocabulary and the sort field against the
// repository's allowlist, so nothing caller-controlled reaches the SQL builder
// as an identifier.
//
// Action is the one filter validated only for length rather than against a
// vocabulary. Docker's action set varies by daemon version, so an allowlist
// here would reject a legitimate filter for an action the host really emits.
// It travels as a bound parameter regardless, so it cannot reach the SQL text.
func parseEventFilter(query url.Values) (store.DockerEventFilter, error) {
	page, pageSize, err := parsePage(query)
	if err != nil {
		return store.DockerEventFilter{}, err
	}

	filter := store.DockerEventFilter{
		ActorID:        strings.TrimSpace(query.Get("actorId")),
		ComposeProject: strings.TrimSpace(query.Get("project")),
		ComposeService: strings.TrimSpace(query.Get("service")),
		Search:         strings.TrimSpace(query.Get("search")),
		// Newest first: an event log is read from the top, and the newest
		// event is the one an operator opened the page for.
		Sort:      "sequence",
		Direction: store.SortDesc,
		Page:      store.Page{Limit: pageSize, Offset: (page - 1) * pageSize},
	}

	for _, raw := range multiValue(query, "type") {
		if !domain.ValidDockerEventType(raw) {
			return store.DockerEventFilter{}, invalidParam("type", "a known event type")
		}
		filter.Types = append(filter.Types, domain.DockerEventType(raw))
	}

	for _, raw := range multiValue(query, "result") {
		if !domain.ValidEventResult(raw) {
			return store.DockerEventFilter{}, invalidParam("result", "a known processing result")
		}
		filter.Results = append(filter.Results, domain.EventProcessingResult(raw))
	}

	for _, raw := range multiValue(query, "action") {
		if len(raw) > maxActionLength {
			return store.DockerEventFilter{}, invalidParam("action",
				fmt.Sprintf("at most %d characters", maxActionLength))
		}
		filter.Actions = append(filter.Actions, strings.ToLower(raw))
	}

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidEventSortField(raw) {
			return store.DockerEventFilter{}, invalidParam("sort",
				"one of "+strings.Join(store.EventSortFields(), ", "))
		}
		filter.Sort = raw
	}

	if raw := strings.TrimSpace(query.Get("direction")); raw != "" {
		switch strings.ToLower(raw) {
		case "asc":
			filter.Direction = store.SortAsc
		case "desc":
			filter.Direction = store.SortDesc
		default:
			return store.DockerEventFilter{}, invalidParam("direction", "asc or desc")
		}
	}

	if filter.Since, err = parseTimeParam(query, "since"); err != nil {
		return store.DockerEventFilter{}, err
	}
	if filter.Until, err = parseTimeParam(query, "until"); err != nil {
		return store.DockerEventFilter{}, err
	}

	// An inverted range matches nothing, which reads to a caller as "there were
	// no events" rather than "your range is backwards".
	if filter.Since != nil && filter.Until != nil && filter.Until.Before(*filter.Since) {
		return store.DockerEventFilter{}, queryError{message: "until must not be earlier than since"}
	}

	return filter, nil
}

// maxActionLength bounds the action filter. Docker's longest action is well
// under this; the limit exists so a caller cannot send a megabyte of text to be
// bound into a query.
const maxActionLength = 64

// parseTimeParam reads an RFC 3339 timestamp parameter.
//
// RFC 3339 only, deliberately: accepting several formats means guessing which
// one was meant, and a time filter that silently interprets a value differently
// from what the caller intended is worse than one that rejects it.
func parseTimeParam(query url.Values, name string) (*time.Time, error) {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return nil, nil
	}

	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// The offending value is deliberately omitted from the error.
		return nil, invalidParam(name, "an RFC 3339 timestamp such as 2026-08-03T09:20:11Z")
	}
	utc := parsed.UTC()
	return &utc, nil
}

// multiValue reads a parameter that may repeat or hold a comma-separated list,
// so both ?state=running&state=paused and ?state=running,paused work.
func multiValue(query url.Values, name string) []string {
	values := make([]string, 0)
	for _, raw := range query[name] {
		for _, part := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				values = append(values, trimmed)
			}
		}
	}
	return values
}

// pageFromFilter recovers the 1-based page number from an offset filter.
func pageFromFilter(filter store.ContainerFilter) int {
	if filter.Page.Limit <= 0 {
		return 1
	}
	return filter.Page.Offset/filter.Page.Limit + 1
}

// writeQueryError renders a validation failure as a 400.
func (s *Server) writeQueryError(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, s.logger, http.StatusBadRequest, CodeInvalidRequest, err.Error())
}
