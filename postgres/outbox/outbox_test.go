package outbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

var (
	ErrRollback = errors.New("rollback")

	dbtestOpts = dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook:  migrate,
	}
)

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	o := outbox.New(db)
	return o.Migrate(context.Background())
}

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtestOpts)
	defer stop()

	m.Run()
}

func TestOutbox(t *testing.T) {
	db := dbtest.New(t, dbtestOpts)

	ctx := t.Context()
	ob := outbox.New(db.DB(t))

	var ids []uuid.UUID
	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		id, err := ob.Create(txCtx, outbox.Message{
			AggregateID:   "a-id-1",
			AggregateType: "a-type-1",
			Type:          "type-1",
			Payload:       json.RawMessage(`{"foo": "bar"}`),
		},
		)
		if err != nil {
			return err
		}
		ids = append(ids, id)
		id, err = ob.Create(txCtx, outbox.Message{
			AggregateID:   "a-id-2",
			AggregateType: "a-type-2",
			Type:          "type-2",
			Payload:       json.RawMessage(`{"one": 1}`),
		})
		if err != nil {
			return err
		}
		ids = append(ids, id)

		return nil
	})
	is := assert.New(t)
	is.NoError(err, err)

	count, err := ob.Count(ctx)
	is.NoError(err)
	is.Equal(int64(2), count)

	t.Run("process failed", func(t *testing.T) {
		is := assert.New(t)
		for range 3 {
			err := ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
				is.Contains(ids, msg.ID)
				return ErrRollback
			})
			is.ErrorIs(err, ErrRollback)
		}

		count, err := ob.Count(ctx)
		is.NoError(err)
		is.Equal(int64(2), count)
	})

	t.Run("process success", func(t *testing.T) {
		is := assert.New(t)

		var errs = []error{nil, nil, outbox.ErrEOQ}
		var counts = []int64{1, 0, 0}

		for i := range 3 {
			err := ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
				is.True(dbtx.IsTx(txCtx))
				is.Contains(ids, msg.ID)
				t.Log("iter", i, "message", msg)
				return err
			})
			is.ErrorIs(err, errs[i])

			count, err := ob.Count(ctx)
			is.NoError(err)
			is.Equal(counts[i], count)
		}
	})
}

func TestOutbox_MaxRetry(t *testing.T) {
	ctx := t.Context()
	ob := outbox.New(dbtest.Tx(t))
	ob.MaxRetry = 3

	var ids []uuid.UUID
	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		id, err := ob.Create(txCtx, outbox.Message{
			AggregateID:   "a-id-1",
			AggregateType: "a-type-1",
			Type:          "type-1",
			Payload:       json.RawMessage(`{"foo": "bar"}`),
		},
		)
		if err != nil {
			return err
		}
		ids = append(ids, id)

		return nil
	})
	is := assert.New(t)
	is.NoError(err, err)

	count, err := ob.Count(ctx)
	is.NoError(err)
	is.Equal(int64(1), count)

	t.Run("max retry exceeded", func(t *testing.T) {
		is := assert.New(t)
		for range 3 {
			err := ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
				is.Contains(ids, msg.ID)
				return ErrRollback
			})
			is.ErrorIs(err, ErrRollback)
		}

		count, err := ob.Count(ctx)
		is.NoError(err)
		is.Equal(int64(0), count)

		count, err = ob.CountDLQ(ctx)
		is.NoError(err)
		is.Equal(int64(1), count)
	})
}

func TestOutbox_RetryAfter(t *testing.T) {
	db := dbtest.New(t, dbtestOpts)

	ctx := t.Context()
	ob := outbox.New(db.DB(t))
	ob.RetryAfter = func(attempts int) time.Duration {
		return 100 * time.Millisecond
	}

	var ids []uuid.UUID
	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		id, err := ob.Create(txCtx, outbox.Message{
			AggregateID:   "a-id-1",
			AggregateType: "a-type-1",
			Type:          "type-1",
			Payload:       json.RawMessage(`{"foo": "bar"}`),
		},
		)
		if err != nil {
			return err
		}
		ids = append(ids, id)

		return nil
	})
	is := assert.New(t)
	is.NoError(err, err)

	count, err := ob.Count(ctx)
	is.NoError(err)
	is.Equal(int64(1), count)

	t.Run("visibility timeout", func(t *testing.T) {
		is := assert.New(t)
		err := ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
			is.Contains(ids, msg.ID)
			return ErrRollback
		})
		is.ErrorIs(err, ErrRollback)

		err = ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
			panic("won't be called")
		})
		is.ErrorIs(err, outbox.ErrEOQ)

		time.Sleep(110 * time.Millisecond)

		err = ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
			is.Contains(ids, msg.ID)
			return ErrRollback
		})
		is.ErrorIs(err, ErrRollback)
	})
}

func TestOutbox_DLQ(t *testing.T) {
	db := dbtest.New(t, dbtestOpts)

	ctx := t.Context()
	ob := outbox.New(db.DB(t))

	var ids []uuid.UUID
	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		id, err := ob.Create(txCtx, outbox.Message{
			AggregateID:   "a-id-1",
			AggregateType: "a-type-1",
			Type:          "type-1",
			Payload:       json.RawMessage(`{"foo": "bar"}`),
		},
		)
		if err != nil {
			return err
		}
		ids = append(ids, id)

		return nil
	})
	is := assert.New(t)
	is.NoError(err, err)

	count, err := ob.Count(ctx)
	is.NoError(err)
	is.Equal(int64(1), count)

	t.Run("dlq", func(t *testing.T) {
		is := assert.New(t)
		err := ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
			is.Contains(ids, msg.ID)
			return outbox.ErrDLQ
		})
		is.ErrorIs(err, outbox.ErrDLQ)

		err = ob.Poll(ctx, func(txCtx context.Context, msg *outbox.Message) error {
			panic("won't be called")
		})
		is.ErrorIs(err, outbox.ErrEOQ)

		count, err := ob.Count(ctx)
		is.NoError(err)
		is.Equal(int64(0), count)

		count, err = ob.CountDLQ(ctx)
		is.NoError(err)
		is.Equal(int64(1), count)
	})
}
