package cache

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/zeebo/xxh3"
)

type dto struct {
	Key       string
	Value     jsontext.Value
	Digest    string
	ExpiresAt sql.NullTime
}

func newDto(key string, value any, ttl time.Duration) (*dto, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	// Unmarshal to map[string]any.
	var a any
	err = json.Unmarshal(b, &a)
	if err != nil {
		return nil, err
	}

	// Marshal with deterministic ordering for map.
	b, err = json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return nil, err
	}

	return &dto{
		Key:    key,
		Value:  b,
		Digest: fmt.Sprint(xxh3.Hash(b)),
		ExpiresAt: sql.NullTime{
			Time:  time.Now().Add(ttl),
			Valid: ttl > 0,
		},
	}, nil
}

func hash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}

	// Unmarshal to map[string]any.
	var a any
	err = json.Unmarshal(b, &a)
	if err != nil {
		return "", err
	}

	// Marshal with deterministic ordering for map.
	b, err = json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return "", err
	}

	return fmt.Sprint(xxh3.Hash(b)), nil
}
