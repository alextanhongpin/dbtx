package cache

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/cache/internal/postgres"
	"github.com/zeebo/xxh3"
)

type dto struct {
	Key       string
	Value     jsontext.Value
	Digest    string
	ExpiresAt *time.Time
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

	var expiresAt *time.Time
	// Allow negative values for testing.
	if ttl != 0 {
		expiresAt = new(time.Now().Add(ttl))
	}
	return &dto{
		Key:       key,
		Value:     b,
		Digest:    fmt.Sprint(xxh3.Hash(b)),
		ExpiresAt: expiresAt,
	}, nil
}

func (d *dto) NullExpiresAt() sql.NullTime {
	if d.ExpiresAt == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{
		Time:  *d.ExpiresAt,
		Valid: true,
	}
}

func (d *dto) Load[T any]() (T, error) {
	if !d.Valid() {
		var zero T
		return zero, ErrNotExist
	}
	var res T
	err := json.Unmarshal(d.Value, &res)
	return res, err
}

func (d *dto) Valid() bool {
	if d.ExpiresAt == nil {
		return true
	}
	return d.ExpiresAt != nil && time.Until(*d.ExpiresAt) > 0
}

func toDto(row *postgres.DbtxCache) *dto {
	var expiresAt *time.Time
	if row.ExpiresAt.Valid {
		expiresAt = new(row.ExpiresAt.Time)
	}
	return &dto{
		Key:       row.Key,
		Value:     row.Value,
		Digest:    row.Digest,
		ExpiresAt: expiresAt,
	}
}
