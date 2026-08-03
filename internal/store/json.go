package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// Helpers for the JSON-valued columns in migration 0002.
//
// Everything here fails soft on read. A column that cannot be decoded yields
// an empty value rather than an error: a malformed row should degrade one
// container's display, not make the whole inventory endpoint fail.

func marshalStrings(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func unmarshalStrings(encoded string) []string {
	if encoded == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(encoded), &values); err != nil {
		return nil
	}
	return values
}

func marshalStringMap(values map[string]string) string {
	if len(values) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func unmarshalStringMap(encoded string) map[string]string {
	if encoded == "" {
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(encoded), &values); err != nil {
		return nil
	}
	return values
}

func marshalJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// nullableTime renders an optional timestamp for storage.
func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return formatTime(*value)
}

// timeOrNil renders a timestamp that may be the zero value.
func timeOrNil(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatTime(value)
}

// scanOptionalTime converts a nullable timestamp column.
func scanOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil
	}
	return &parsed
}

// scanTime converts a nullable timestamp column to a zero-able time.
func scanTime(value sql.NullString) time.Time {
	if parsed := scanOptionalTime(value); parsed != nil {
		return *parsed
	}
	return time.Time{}
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func scanOptionalInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func nullableBool(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func scanOptionalBool(value sql.NullBool) *bool {
	if !value.Valid {
		return nil
	}
	converted := value.Bool
	return &converted
}
