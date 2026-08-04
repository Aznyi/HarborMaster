package api

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// maxChecksumLength bounds the checksum filter. A checksum is 64 hex
// characters; the bound exists so a caller cannot send a megabyte of text to be
// bound into a query.
const maxChecksumLength = 64

// maxContainerRefLength bounds a container reference. Docker IDs are 64
// characters and names are far shorter.
const maxContainerRefLength = 255

// parseSnapshotFilter builds a repository filter from the query string.
//
// Same discipline as parseContainerFilter and parseEventFilter: every
// enumerated value is checked against the domain's closed vocabulary and the
// sort field against the repository's allowlist, so nothing caller-controlled
// reaches the SQL builder as an identifier. Everything else travels as a bound
// parameter.
func parseSnapshotFilter(query url.Values) (store.SnapshotFilter, error) {
	page, pageSize, err := parsePage(query)
	if err != nil {
		return store.SnapshotFilter{}, err
	}

	filter := store.SnapshotFilter{
		ContainerID: strings.TrimSpace(query.Get("containerId")),
		Checksum:    strings.TrimSpace(query.Get("checksum")),
		// Newest first: snapshot history is read from the top, and the newest
		// capture is the one an operator opened the page for.
		Sort:      "createdAt",
		Direction: store.SortDesc,
		Page:      store.Page{Limit: pageSize, Offset: (page - 1) * pageSize},
	}

	if len(filter.ContainerID) > maxContainerRefLength {
		return store.SnapshotFilter{}, invalidParam("containerId",
			fmt.Sprintf("at most %d characters", maxContainerRefLength))
	}
	if filter.Checksum != "" {
		if len(filter.Checksum) > maxChecksumLength || !isHexString(filter.Checksum) {
			return store.SnapshotFilter{}, invalidParam("checksum", "a hex-encoded digest")
		}
	}

	for _, raw := range multiValue(query, "trigger") {
		if !domain.ValidSnapshotTrigger(raw) {
			return store.SnapshotFilter{}, invalidParam("trigger", "a known snapshot trigger")
		}
		filter.Triggers = append(filter.Triggers, domain.SnapshotTrigger(raw))
	}

	for _, raw := range multiValue(query, "readiness") {
		if !domain.ValidReadinessStatus(raw) {
			return store.SnapshotFilter{}, invalidParam("readiness", "a known readiness status")
		}
		filter.Readiness = append(filter.Readiness, domain.ReadinessStatus(raw))
	}

	if raw := strings.TrimSpace(query.Get("sort")); raw != "" {
		if !store.ValidSnapshotSortField(raw) {
			return store.SnapshotFilter{}, invalidParam("sort",
				"one of "+strings.Join(store.SnapshotSortFields(), ", "))
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
			return store.SnapshotFilter{}, invalidParam("direction", "asc or desc")
		}
	}

	if filter.Since, err = parseTimeParam(query, "since"); err != nil {
		return store.SnapshotFilter{}, err
	}
	if filter.Until, err = parseTimeParam(query, "until"); err != nil {
		return store.SnapshotFilter{}, err
	}

	// An inverted range matches nothing, which reads to a caller as "there were
	// no snapshots" rather than "your range is backwards".
	if filter.Since != nil && filter.Until != nil && filter.Until.Before(*filter.Since) {
		return store.SnapshotFilter{}, queryError{message: "until must not be earlier than since"}
	}

	return filter, nil
}

// snapshotDiffQuery is a validated diff request.
type snapshotDiffQuery struct {
	// AgainstSnapshotID is zero when the comparison target is the container's
	// current configuration.
	AgainstSnapshotID int64
	AgainstCurrent    bool
	Groups            []domain.DiffGroupName
	IncludeUnchanged  bool
}

// parseDiffQuery validates a diff request.
//
// The engine accepts exactly two inputs. There is no expression, selector, or
// pattern parameter here and there must never be one: an interpreter reachable
// from an unauthenticated endpoint is both a denial-of-service surface and an
// injection surface. Narrowing is done by naming groups from a closed
// vocabulary.
func parseDiffQuery(query url.Values) (snapshotDiffQuery, error) {
	parsed := snapshotDiffQuery{AgainstCurrent: true}

	switch raw := strings.TrimSpace(query.Get("against")); raw {
	case "", "current":
		parsed.AgainstCurrent = true
	default:
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return snapshotDiffQuery{}, invalidParam("against", "\"current\" or a snapshot id")
		}
		parsed.AgainstSnapshotID = id
		parsed.AgainstCurrent = false
	}

	for _, raw := range multiValue(query, "group") {
		if !domain.ValidDiffGroup(raw) {
			return snapshotDiffQuery{}, invalidParam("group", "a known diff group")
		}
		parsed.Groups = append(parsed.Groups, domain.DiffGroupName(raw))
	}

	if raw := strings.TrimSpace(query.Get("includeUnchanged")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return snapshotDiffQuery{}, invalidParam("includeUnchanged", "true or false")
		}
		parsed.IncludeUnchanged = value
	}

	return parsed, nil
}

// parseSnapshotID validates a path ID.
func parseSnapshotID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id < 1 {
		return 0, invalidParam("id", "a positive integer")
	}
	return id, nil
}

// isHexString reports whether s is entirely hexadecimal.
func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
