package api

import (
	"net/url"
	"strings"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// Notification query parsing.
//
// Every parameter is validated against a CLOSED VOCABULARY defined in the
// domain package, or is a bounded integer, or is an identifier matching a fixed
// shape. Nothing a caller sends becomes SQL text: a result or an event is
// matched against the domain's allowlist and then travels as a bound parameter.
//
// Rejected rather than clamped, matching the rest of the API: silently serving
// something other than what was asked hides a bug in the caller.

// notificationConfigQuery is a parsed destination or rule listing request.
type notificationConfigQuery struct {
	IncludeArchived bool
	Page            int
	PageSize        int
}

// Offset is where the page starts.
func (q notificationConfigQuery) Offset() int { return (q.Page - 1) * q.PageSize }

// parseNotificationConfigQuery reads the destination and rule list parameters.
func parseNotificationConfigQuery(query url.Values) (notificationConfigQuery, error) {
	var parsed notificationConfigQuery

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	// Archived rows are excluded by default. An archived destination is
	// history; a list dominated by history buries the ones in force.
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

	return parsed, nil
}

// deliveryQuery is a parsed and validated delivery history request.
type deliveryQuery struct {
	DestinationID string
	ContainerName string
	Results       []domain.DeliveryResult
	Events        []domain.NotificationEvent
	FailedOnly    bool

	Page     int
	PageSize int
}

// parseDeliveryQuery reads and validates the delivery list parameters.
func parseDeliveryQuery(query url.Values) (deliveryQuery, error) {
	var parsed deliveryQuery

	page, pageSize, err := parsePage(query)
	if err != nil {
		return parsed, err
	}
	parsed.Page, parsed.PageSize = page, pageSize

	if raw := strings.TrimSpace(query.Get("destinationId")); raw != "" {
		if !domain.ValidNotificationDestinationID(raw) {
			return parsed, invalidParam("destinationId", "a notification destination identifier")
		}
		parsed.DestinationID = raw
	}

	if raw := strings.TrimSpace(query.Get("container")); raw != "" {
		if len(raw) > domain.MaxContainerNameBytes {
			return parsed, invalidParam("container", "a container name")
		}
		parsed.ContainerName = raw
	}

	// Shared with the drift and policy listings: the same bound of 32 values,
	// the same refusal of an unrecognised one, and the same rule that the
	// offending value is never echoed back.
	results, err := parseVocabulary(query, "result",
		domain.ValidDeliveryResult, "one of pending, retrying, succeeded, failed, suppressed, dropped")
	if err != nil {
		return parsed, err
	}
	for _, value := range results {
		parsed.Results = append(parsed.Results, domain.DeliveryResult(value))
	}

	events, err := parseVocabulary(query, "event",
		domain.ValidNotificationEvent, "a notification event")
	if err != nil {
		return parsed, err
	}
	for _, value := range events {
		parsed.Events = append(parsed.Events, domain.NotificationEvent(value))
	}

	// The filter an operator asking "what did I not get told about" needs.
	if raw := strings.TrimSpace(query.Get("failed")); raw != "" {
		switch raw {
		case "true":
			parsed.FailedOnly = true
		case "false":
			parsed.FailedOnly = false
		default:
			return parsed, invalidParam("failed", "true or false")
		}
	}

	return parsed, nil
}

// filter converts a validated query into a repository filter.
func (q deliveryQuery) filter() store.DeliveryFilter {
	return store.DeliveryFilter{
		DestinationID: q.DestinationID,
		ContainerName: q.ContainerName,
		Results:       q.Results,
		Events:        q.Events,
		FailedOnly:    q.FailedOnly,
		Page: store.Page{
			Limit:  q.PageSize,
			Offset: (q.Page - 1) * q.PageSize,
		},
	}
}
