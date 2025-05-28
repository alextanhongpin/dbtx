package outbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	_ "embed"

	"github.com/alextanhongpin/core/storage/pg/pgtest"
	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/stretchr/testify/assert"
)

var ErrRollback = errors.New("rollback")

const postgresVersion = "postgres:17.4"

//go:embed internal/schema.sql
var schema string

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

func TestMain(m *testing.M) {
	stop := pgtest.Init(pgtest.Image(postgresVersion), pgtest.Hook(migrate))
	defer stop()

	m.Run()
}

func TestOutbox(t *testing.T) {
	ctx := t.Context()
	ob := outbox.New(dbtx.New(pgtest.DB(t)))

	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		return ob.Create(txCtx,
			outbox.Message{
				AggregateID:   "a-id-1",
				AggregateType: "a-type-1",
				Type:          "type-1",
				Payload:       json.RawMessage(`{"foo": "bar"}`),
			},
			outbox.Message{
				AggregateID:   "a-id-2",
				AggregateType: "a-type-2",
				Type:          "type-2",
				Payload:       json.RawMessage(`{"one": 1}`),
			},
		)
	})
	is := assert.New(t)
	is.NoError(err, err)

	count, err := ob.Count(ctx)
	is.NoError(err)
	is.Equal(int64(2), count)

	t.Run("process failed", func(t *testing.T) {
		is := assert.New(t)
		err := ob.RunInTx(ctx, func(txCtx context.Context) error {
			evt, err := ob.LoadAndDelete(txCtx)
			is.NoError(err)
			is.NotNil(evt)

			return ErrRollback
		})
		is.ErrorIs(err, ErrRollback)

		count, err := ob.Count(ctx)
		is.NoError(err)
		is.Equal(int64(2), count)
	})

	t.Run("process success", func(t *testing.T) {
		is := assert.New(t)

		var errs = []error{nil, nil, sql.ErrNoRows}
		var counts = []int64{1, 0, 0}

		for i := range 2 {
			err := ob.RunInTx(ctx, func(txCtx context.Context) error {
				is.True(dbtx.IsTx(txCtx))
				evt, err := ob.LoadAndDelete(txCtx)
				t.Log("iter", i, "event", evt)
				return err
			})
			is.ErrorIs(err, errs[i])

			count, err := ob.Count(ctx)
			is.NoError(err)
			is.Equal(counts[i], count)
		}
	})
}
