package outbox

import (
	"encoding/json"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/outbox/internal/postgres"
)

// Message is the enqueued message.
type Message struct {
	ID            uuid.UUID
	AggregateID   string
	AggregateType string
	Payload       json.RawMessage
	Type          string
	CreatedAt     time.Time
	FailureReason string
	RetryAt       *time.Time
	RetryCount    int
	RunAt         *time.Time
}

func newMessage(row *postgres.DbtxOutbox) *Message {
	var retryAt, runAt *time.Time
	if row.RetryAt.Valid {
		retryAt = new(row.RetryAt.Time)
	}
	if row.RunAt.Valid {
		runAt = new(row.RunAt.Time)
	}
	return &Message{
		ID:            row.ID,
		AggregateID:   row.AggregateID,
		AggregateType: row.AggregateType,
		Type:          row.Type,
		Payload:       row.Payload,
		CreatedAt:     row.CreatedAt,
		FailureReason: row.FailureReason,
		RetryAt:       retryAt,
		RetryCount:    int(row.RetryCount),
		RunAt:         runAt,
	}
}
