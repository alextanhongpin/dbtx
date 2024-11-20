package outbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	_ "embed"

	"github.com/alextanhongpin/core/storage/pg/pgtest"
	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/stretchr/testify/assert"
)

var ErrRollback = errors.New("rollback")

const postgresVersion = "postgres:15.1-alpine"

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
	is := assert.New(t)
	ob := outbox.New(pgtest.DB(t))
	ctx := context.Background()
	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		ok := outbox.Enqueue(txCtx,
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
		is.True(ok)

		return nil
	})
	is.Nil(err, err)

	count, err := ob.Count(ctx)
	is.Nil(err)
	is.Equal(int64(2), count)

	t.Run("process failed", func(t *testing.T) {
		is := assert.New(t)
		err := ob.Process(ctx, func(txCtx context.Context, evt outbox.Event) error {
			is.True(dbtx.IsTx(txCtx))

			return ErrRollback
		})
		is.ErrorIs(err, ErrRollback)

		count, err := ob.Count(ctx)
		is.Nil(err)
		is.Equal(int64(2), count)
	})

	t.Run("process success", func(t *testing.T) {
		is := assert.New(t)

		for _, i := range []int64{1, 0} {
			err := ob.Process(ctx, func(txCtx context.Context, evt outbox.Event) error {
				is.True(dbtx.IsTx(txCtx))

				return nil
			})
			is.Nil(err)

			count, err := ob.Count(ctx)
			is.Nil(err)
			is.Equal(i, count)
		}

		err := ob.Process(ctx, func(txCtx context.Context, evt outbox.Event) error {
			is.True(dbtx.IsTx(txCtx))

			return nil
		})
		is.ErrorIs(err, outbox.EOQ)
	})
}

func TestPoll(t *testing.T) {
	is := assert.New(t)

	ob := outbox.New(pgtest.New(t, pgtest.Image(postgresVersion), pgtest.Hook(migrate)).DB())
	ctx := context.Background()
	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		ok := outbox.Enqueue(txCtx,
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
		is.True(ok)

		return nil
	})
	is.Nil(err, err)

	count, err := ob.Count(ctx)
	is.Nil(err)
	is.Equal(int64(2), count)

	t.Run("pool", func(t *testing.T) {
		is := assert.New(t)

		var wg sync.WaitGroup
		wg.Add(2)
		stop := ob.Poll(ctx, func(txCtx context.Context, evt outbox.Event) error {
			defer wg.Done()

			is.True(dbtx.IsTx(txCtx))
			t.Log(evt)
			return nil
		}, &outbox.PollOptions{
			Concurrency:    5,
			BatchSize:      10,
			PollInterval:   time.Second,
			MaxIdleTimeout: time.Minute,
		})

		wg.Wait()
		stop()

		count, err := ob.Count(ctx)
		is.Nil(err)
		is.Equal(int64(0), count)
	})
}
