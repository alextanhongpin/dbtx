// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.27.0

package postgres

import (
	"encoding/json"
	"time"
)

type Outbox struct {
	ID            int64
	AggregateID   string
	AggregateType string
	Type          string
	Payload       json.RawMessage
	CreatedAt     time.Time
}
