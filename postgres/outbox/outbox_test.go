package outbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
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

func TestOutboxTestSuite(t *testing.T) {
	suite.Run(t, new(OutboxTestSuite))
}

type OutboxTestSuite struct {
	suite.Suite
	ids      []uuid.UUID
	ob       *outbox.Outbox
	maxRetry int
}

func (suite *OutboxTestSuite) SetupTest() {
	t := suite.T()

	db := dbtest.New(t, dbtestOpts)
	ob := outbox.New(db.DB(t))

	suite.ob = ob
	suite.maxRetry = 3
}

func (suite *OutboxTestSuite) TestDequeueError() {
	ctx := suite.T().Context()
	n := 1
	ob := suite.ob
	suite.createN(n)
	err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
		suite.Contains(suite.ids, msg.ID)
		return ErrRollback
	})
	suite.ErrorIs(err, ErrRollback)
	suite.count(n)
}

func (suite *OutboxTestSuite) TestDequeueSuccess() {
	ctx := suite.T().Context()
	ob := suite.ob
	n := 2
	suite.createN(n)

	errs := []error{nil, nil, outbox.ErrEOQ}
	counts := []int{1, 0, 0}

	for i := range n + 1 {
		err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			return nil
		})
		suite.ErrorIs(err, errs[i])
		suite.count(counts[i])
	}
}

func (suite *OutboxTestSuite) TestMaxRetry() {
	ctx := suite.T().Context()
	ob := suite.ob
	n := 1
	suite.createN(n)

	errs := []error{assert.AnError, assert.AnError, assert.AnError, outbox.ErrEOQ}
	counts := []int{1, 1, 0, 0}

	for i := range len(errs) {
		err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			return outbox.Nack(assert.AnError)
		})
		suite.ErrorIs(err, errs[i], "errors[%d]", i)
		suite.count(counts[i], "counts[%d]", i)
	}
}

func (suite *OutboxTestSuite) TestTimeout() {
	ctx := suite.T().Context()
	ob := suite.ob
	n := 1
	suite.createN(n)

	sleep := []time.Duration{0, 0, 100 * time.Millisecond}
	count := []int{0, 0, 0}
	errs := []error{assert.AnError, outbox.ErrEOQ, assert.AnError}

	for i := range 3 {
		time.Sleep(sleep[i])
		err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			nack := outbox.Nack(assert.AnError)
			nack.Timeout = 100 * time.Millisecond
			return nack
		})
		suite.ErrorIs(err, errs[i], "errs[%d]", i)
		suite.count(count[i], "counts[%d]", i)
	}
}

func (suite *OutboxTestSuite) TestSkip() {
	ctx := suite.T().Context()
	ob := suite.ob
	n := 1
	suite.createN(n)

	errs := []error{assert.AnError, outbox.ErrEOQ}
	count := []int{0, 0}

	for i := range len(count) {
		err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			nack := outbox.Nack(assert.AnError)
			nack.Skip = true
			return nack
		})
		suite.ErrorIs(err, errs[i], "errs[%d]", i)
		suite.count(count[i], "count[%d]", i)
	}
}

/* Helpers */

func (suite *OutboxTestSuite) createN(n int) {
	t := suite.T()
	t.Helper()

	ctx := t.Context()
	ob := suite.ob

	err := ob.RunInTx(ctx, func(txCtx context.Context) error {
		for i := range n {
			id, err := ob.Enqueue(txCtx, outbox.EnqueueParams{
				AggregateID:   fmt.Sprintf("a-id-%d", i+1),
				AggregateType: fmt.Sprintf("a-type-%d", i+1),
				Type:          fmt.Sprintf("type-%d", i+1),
				Payload:       json.RawMessage(`{"foo": "bar"}`),
				MaxRetry:      int32(suite.maxRetry),
			},
			)
			if err != nil {
				return err
			}
			suite.ids = append(suite.ids, id)
		}
		return nil
	})

	suite.NoError(err)
	suite.count(n)
}

func (suite *OutboxTestSuite) count(n int, msgAndArgs ...any) {
	t := suite.T()
	t.Helper()
	ctx := t.Context()

	count, err := suite.ob.Count(ctx)
	suite.NoError(err)
	suite.Equal(int64(n), count, msgAndArgs...)
}
