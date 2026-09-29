package outbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"uuid"

	_ "github.com/lib/pq"

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
	_, err = db.Exec(outbox.Schema)
	return err
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
	repo     *outbox.PostgresRepository
	maxRetry int
}

func (suite *OutboxTestSuite) SetupTest() {
	t := suite.T()

	db := dbtest.New(t, dbtestOpts).DB(t)
	_, err := db.ExecContext(t.Context(), `delete from dbtx.outbox`)
	suite.Require().NoError(err)

	suite.repo = outbox.NewRepository(db)
	suite.ob = outbox.New(suite.repo)
	// Retry immediately, so that tests do not need to wait for the backoff.
	suite.ob.Backoff = func(outbox.Message) time.Duration { return 0 }
	suite.ids = nil
	suite.maxRetry = 3
}

func (suite *OutboxTestSuite) TestEnqueueRollback() {
	ctx := suite.T().Context()

	err := suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
		_, err := suite.ob.Enqueue(txCtx, suite.params(1))
		suite.NoError(err)
		return ErrRollback
	})
	suite.ErrorIs(err, ErrRollback)
	suite.count(0)

	err = suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
		_, err := suite.ob.Enqueue(txCtx, suite.params(1))
		return err
	})
	suite.NoError(err)
	suite.count(1)
}

func (suite *OutboxTestSuite) TestEnqueueVisibleAt() {
	ctx := suite.T().Context()
	params := suite.params(1)
	params.VisibleAt = new(time.Now().Add(time.Hour))
	_, err := suite.ob.Enqueue(ctx, params)
	suite.NoError(err)
	suite.count(0)
}

func (suite *OutboxTestSuite) TestDequeueError() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)
	err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
		suite.Contains(suite.ids, msg.ID)
		return ErrRollback
	})
	suite.ErrorIs(err, ErrRollback)
	_, ok := errors.AsType[*outbox.NackError](err)
	suite.True(ok, "handler errors are returned as *NackError")

	// Plain errors count as a retry.
	msg := suite.find(suite.ids[0])
	suite.Equal(int32(1), msg.RetryCount)
	suite.Equal(ErrRollback.Error(), msg.LastError)
	suite.count(1)
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

func (suite *OutboxTestSuite) TestHandlerWritesRolledBackOnError() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	for _, fail := range []error{ErrRollback, outbox.Nack(ErrRollback)} {
		err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
			_, err := ob.Enqueue(txCtx, suite.params(2))
			suite.NoError(err)
			return fail
		})
		suite.ErrorIs(err, ErrRollback)
		// Only the original message remains.
		suite.count(1)
	}
}

func (suite *OutboxTestSuite) TestHandlerWritesCommittedOnSuccess() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	var next uuid.UUID
	err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
		var err error
		next, err = ob.Enqueue(txCtx, suite.params(2))
		return err
	})
	suite.NoError(err)
	suite.count(1)
	suite.Equal(next, suite.find(next).ID)
}

func (suite *OutboxTestSuite) TestMaxRetry() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

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

	dead, err := ob.DeadLetters(ctx, 10)
	suite.NoError(err)
	suite.Len(dead, 1)
}

func (suite *OutboxTestSuite) TestUnlimitedRetry() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.maxRetry = 0
	suite.createN(1)

	for range 5 {
		err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
			return assert.AnError
		})
		suite.ErrorIs(err, assert.AnError)
		suite.count(1)
	}
	suite.Equal(int32(5), suite.find(suite.ids[0]).RetryCount)
}

func (suite *OutboxTestSuite) TestTimeout() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	sleep := []time.Duration{0, 0, 150 * time.Millisecond}
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

func (suite *OutboxTestSuite) TestDefaultBackoff() {
	ctx := suite.T().Context()
	ob := outbox.New(suite.repo)
	suite.createN(1)

	err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
		return assert.AnError
	})
	suite.ErrorIs(err, assert.AnError)
	suite.count(0)
}

func (suite *OutboxTestSuite) TestSkip() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

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

func (suite *OutboxTestSuite) TestDeadLetters() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(2)

	err := ob.Dequeue(ctx, func(txCtx context.Context, msg outbox.Message) error {
		nack := outbox.Nack(assert.AnError)
		nack.Skip = true
		return nack
	})
	suite.ErrorIs(err, assert.AnError)
	suite.count(1)

	dead, err := ob.DeadLetters(ctx, 10)
	suite.NoError(err)
	suite.Require().Len(dead, 1)
	suite.Equal(assert.AnError.Error(), dead[0].LastError)

	// Only dead messages can be requeued.
	_, err = ob.Requeue(ctx, suite.ids[1], 3)
	suite.ErrorIs(err, outbox.ErrNotFound)

	msg, err := ob.Requeue(ctx, dead[0].ID, 3)
	suite.NoError(err)
	suite.Equal(int32(0), msg.RetryCount)
	suite.Equal(int32(3), msg.MaxRetry)
	suite.count(2)

	// Kill it again, then purge.
	err = ob.Handle(ctx, dead[0].ID, func(ctx context.Context, msg outbox.Message) error {
		nack := outbox.Nack(assert.AnError)
		nack.Skip = true
		return nack
	})
	suite.ErrorIs(err, assert.AnError)

	n, err := ob.PurgeDead(ctx, time.Now().Add(-time.Hour))
	suite.NoError(err)
	suite.Equal(int64(0), n)

	n, err = ob.PurgeDead(ctx, time.Now().Add(time.Hour))
	suite.NoError(err)
	suite.Equal(int64(1), n)
	suite.count(1)
}

func (suite *OutboxTestSuite) TestHandle() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(2)

	err := ob.Handle(ctx, uuid.NewV7(), func(ctx context.Context, msg outbox.Message) error {
		suite.Fail("unexpected call")
		return nil
	})
	suite.ErrorIs(err, outbox.ErrNotFound)

	err = ob.Handle(ctx, suite.ids[1], func(ctx context.Context, msg outbox.Message) error {
		suite.Equal(suite.ids[1], msg.ID)
		return outbox.Nack(assert.AnError)
	})
	suite.ErrorIs(err, assert.AnError)
	suite.Equal(int32(1), suite.find(suite.ids[1]).RetryCount)

	err = ob.Handle(ctx, suite.ids[1], func(ctx context.Context, msg outbox.Message) error {
		suite.True(dbtx.IsTx(ctx))
		return nil
	})
	suite.NoError(err)
	suite.count(1)

	_, err = suite.repo.Find(ctx, suite.ids[1])
	suite.ErrorIs(err, outbox.ErrNotFound)
}

func (suite *OutboxTestSuite) TestHandleLocked() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)
	id := suite.ids[0]

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ob.Handle(ctx, id, func(ctx context.Context, msg outbox.Message) error {
			close(locked)
			<-release
			return nil
		})
	}()

	<-locked
	err := ob.Handle(ctx, id, func(ctx context.Context, msg outbox.Message) error {
		suite.Fail("unexpected call")
		return nil
	})
	suite.ErrorIs(err, outbox.ErrLocked)

	close(release)
	suite.NoError(<-done)
	suite.count(0)
}

func (suite *OutboxTestSuite) TestConcurrentDequeue() {
	ctx := suite.T().Context()
	ob := suite.ob
	n := 2
	suite.createN(n)

	var (
		mu   sync.Mutex
		seen = make(map[uuid.UUID]bool)
		wg   sync.WaitGroup
		// Every worker waits until all have locked a message, so the
		// messages must have been handed out to different workers.
		entered sync.WaitGroup
		all     = make(chan struct{})
	)
	entered.Add(n)
	go func() {
		entered.Wait()
		close(all)
	}()

	for range n {
		wg.Go(func() {
			err := ob.Dequeue(ctx, func(ctx context.Context, msg outbox.Message) error {
				mu.Lock()
				seen[msg.ID] = true
				mu.Unlock()
				entered.Done()

				select {
				case <-all:
					return nil
				case <-time.After(5 * time.Second):
					return errors.New("timeout waiting for other workers")
				}
			})
			suite.NoError(err)
		})
	}
	wg.Wait()

	suite.Len(seen, n)
	suite.count(0)
}

func TestNack(t *testing.T) {
	is := assert.New(t)

	is.NotPanics(func() {
		is.Equal("outbox: nack", outbox.Nack(nil).Error())
	})
	is.NoError(outbox.Nack(nil).Unwrap())

	var err error = fmt.Errorf("wrapped: %w", outbox.Nack(assert.AnError))
	is.ErrorIs(err, assert.AnError)
	_, ok := errors.AsType[*outbox.NackError](err)
	is.True(ok)
}

func TestDefaultBackoff(t *testing.T) {
	is := assert.New(t)

	cases := []struct {
		retry    int32
		min, max time.Duration
	}{
		{0, 500 * time.Millisecond, time.Second},
		{1, time.Second, 2 * time.Second},
		{3, 4 * time.Second, 8 * time.Second},
		{20, 30 * time.Minute, time.Hour},
	}
	for _, tc := range cases {
		for range 100 {
			d := outbox.DefaultBackoff(outbox.Message{RetryCount: tc.retry})
			is.GreaterOrEqual(d, tc.min, "retry %d", tc.retry)
			is.LessOrEqual(d, tc.max, "retry %d", tc.retry)
		}
	}
}

/* Helpers */

func (suite *OutboxTestSuite) params(i int) outbox.EnqueueParams {
	return outbox.EnqueueParams{
		AggregateID:   fmt.Sprintf("a-id-%d", i),
		AggregateType: fmt.Sprintf("a-type-%d", i),
		Type:          fmt.Sprintf("type-%d", i),
		Payload:       json.RawMessage(`{"foo": "bar"}`),
		MaxRetry:      int32(suite.maxRetry),
	}
}

func (suite *OutboxTestSuite) createN(n int) {
	t := suite.T()
	t.Helper()

	ctx := t.Context()
	ob := suite.ob

	for i := range n {
		id, err := ob.Enqueue(ctx, suite.params(i+1))
		suite.NoError(err)
		suite.ids = append(suite.ids, id)
	}
	suite.count(n)
}

func (suite *OutboxTestSuite) find(id uuid.UUID) *outbox.Message {
	t := suite.T()
	t.Helper()

	msg, err := suite.repo.Find(t.Context(), id)
	suite.Require().NoError(err)
	return msg
}

func (suite *OutboxTestSuite) count(n int, msgAndArgs ...any) {
	t := suite.T()
	t.Helper()
	ctx := t.Context()

	count, err := suite.ob.Count(ctx)
	suite.NoError(err)
	suite.Equal(int64(n), count, msgAndArgs...)
}
