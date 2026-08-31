package outbox

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
	"uuid"
)

// Message is the enqueued message.
type Message struct {
	ID            uuid.UUID       `json:"id"`
	AggregateID   string          `json:"aggregate_id"`
	AggregateType string          `json:"aggregate_type"`
	Payload       json.RawMessage `json:"payload"`
	Type          string          `json:"type"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Scan implements the sql.Scanner interface to parse JSON data from Postgres
func (m *Message) Scan(src any) error {
	if src == nil {
		*m = Message{}
		return nil
	}

	var bytes []byte
	switch v := src.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return fmt.Errorf("unexpected type for MessageList: %T", src)
	}

	// Unmarshal the JSON array directly into the custom slice type
	return json.Unmarshal(bytes, m)
}

// Value implements the driver.Valuer interface to serialize data into Postgres
func (m Message) Value() (driver.Value, error) {
	bytes, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Message: %w", err)
	}

	return string(bytes), nil
}

// MessageList is the custom slice type that implements sql.Scanner and driver.Valuer
type MessageList []*Message

// Scan implements the sql.Scanner interface to parse JSON data from Postgres
func (m *MessageList) Scan(src any) error {
	if src == nil {
		*m = MessageList{}
		return nil
	}

	var bytes []byte
	switch v := src.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return fmt.Errorf("unexpected type for MessageList: %T", src)
	}

	// Unmarshal the JSON array directly into the custom slice type
	return json.Unmarshal(bytes, m)
}

// Value implements the driver.Valuer interface to serialize data into Postgres
func (m MessageList) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}

	bytes, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MessageList: %w", err)
	}

	return string(bytes), nil
}
