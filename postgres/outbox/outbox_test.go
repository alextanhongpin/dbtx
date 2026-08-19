package outbox_test

import (
	_ "embed"
	"sync"
	"time"

	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

var (
	ErrRollback = errors.New("rollback")

	dbtestOpts = dbtest.Options{
		Image: "postgres:18.4-bookworm",
		Hook:  migrate,
	}
)

//go:embed queries/schema.sql
var schema string

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(schema)
	return err
}

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtestOpts)
	defer stop()

	m.Run()
}

func TestOutbox(t *testing.T) {
	ctx := t.Context()
	ob := outbox.New(dbtx.New(dbtest.DB(t)))

	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		return ob.Create(txCtx,
			&outbox.Message{
				AggregateID:   "a-id-1",
				AggregateType: "a-type-1",
				Type:          "type-1",
				Payload:       json.RawMessage(`{"foo": "bar"}`),
			},
			&outbox.Message{
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

	t.Run("wait", func(t *testing.T) {
		var wg sync.WaitGroup

		err := ob.RunInTx(ctx, func(txCtx context.Context) error {
			return ob.Create(txCtx,
				&outbox.Message{
					AggregateID:   "a-id-1",
					AggregateType: "a-type-1",
					Type:          "type-1",
					Payload:       json.RawMessage(`{"foo": "bar"}`),
				},
				&outbox.Message{
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

		for range 3 {
			wg.Go(func() {
				err := ob.RunInTx(ctx, func(txCtx context.Context) error {
					evt, err := ob.LoadAndDelete(txCtx)
					if err != nil {
						return err
					}
					t.Log(evt)
					time.Sleep(time.Second)

					return nil
				})
				if err != nil {
					t.Log(err)
				}
			})
			wg.Wait()
		}
	})
}
