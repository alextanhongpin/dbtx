// Package pg holds small PostgreSQL helpers shared by the durable packages.
package pg

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"
)

// SQLState returns the SQLSTATE code of a driver error, or "" when the
// driver does not expose one. lib/pq and pgx both implement SQLState().
func SQLState(err error) string {
	var s interface{ SQLState() string }
	if errors.As(err, &s) {
		return s.SQLState()
	}
	return ""
}

// Codes used by the durable packages.
const (
	CodeLockNotAvailable = "55P03"
	CodeQueryCanceled    = "57014"
	CodeNumericRange     = "22003"
	CodeUniqueViolation  = "23505"
)

// Micros converts a duration to whole microseconds for interval arithmetic.
func Micros(d time.Duration) int64 { return int64(d / time.Microsecond) }

// Timeout formats d for durable.set_local_timeouts.
func Timeout(d time.Duration) string { return fmt.Sprintf("%dms", max(int64(1), d.Milliseconds())) }

// NewID returns a time-ordered UUID.
func NewID() uuid.UUID { return uuid.NewV7() }

// JSON is a jsonb value that may be NULL (nil). The generated queries use it
// for every jsonb column, because json.RawMessage cannot scan NULL.
type JSON []byte

// Scan implements sql.Scanner.
func (j *JSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append(JSON(nil), v...)
	case string:
		*j = JSON(v)
	default:
		return fmt.Errorf("pg: cannot scan %T into JSON", src)
	}
	return nil
}

// Value implements driver.Valuer: nil is NULL, anything else is JSON text.
func (j JSON) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return string(j), nil
}

// Raw returns the value as json.RawMessage (nil for NULL).
func (j JSON) Raw() json.RawMessage {
	if j == nil {
		return nil
	}
	return json.RawMessage(j)
}

// ValidJSON validates raw as JSON, mapping empty input to fallback.
func ValidJSON(raw []byte, fallback string) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(fallback), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("pg: invalid JSON")
	}
	return raw, nil
}

// Marshal encodes v as JSON; json.RawMessage and []byte pass through
// unchanged after validation.
func Marshal(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return []byte("null"), nil
	case json.RawMessage:
		return ValidJSON(x, "null")
	case []byte:
		return ValidJSON(x, "null")
	}
	return json.Marshal(v)
}
