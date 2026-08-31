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
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

func newDto(row *postgres.DbtxCache) *dto {
	var expiresAt *time.Time
	if row.ExpiresAt.Valid {
		expiresAt = new(row.ExpiresAt.Time)
	}
	return &dto{
		Key:       row.Key,
		Value:     row.Value,
		Digest:    row.Digest,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		ExpiresAt: expiresAt,
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

func newRow(key string, value any, ttl time.Duration) (*postgres.StoreParams, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var a any
	err = json.Unmarshal(b, &a)
	if err != nil {
		return nil, err
	}
	b, err = json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return &postgres.StoreParams{
		Key:    key,
		Value:  b,
		Digest: fmt.Sprint(xxh3.Hash(b)),
		ExpiresAt: sql.NullTime{
			Time:  time.Now().Add(ttl),
			Valid: ttl > 0,
		},
	}, nil
}
