package outbox_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"uuid"

	_ "github.com/lib/pq"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/alextanhongpin/dbtx/postgres/outbox/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

var (
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
	_, err = db.Exec(repository.Schema)
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
	repo     *repository.Repository
	maxRetry int
}

func (suite *OutboxTestSuite) SetupTest() {
	t := suite.T()

	db := dbtest.New(t, dbtestOpts).DB(t)
	_, err := db.ExecContext(t.Context(), `delete from dbtx.outbox`)
	suite.Require().NoError(err)

	suite.repo = repository.New(db)
	suite.ob = outbox.New(suite.repo)
	suite.ob.Backoff = func(msg *outbox.Message) time.Duration {
		return 0
	}
	suite.ids = nil
	// Default.
	suite.maxRetry = 10
}

func (suite *OutboxTestSuite) TestEnqueueError() {
	ctx := suite.T().Context()

	err := suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
		_, err := suite.ob.Enqueue(txCtx, suite.params(1))
		suite.NoError(err)
		return assert.AnError
	})
	suite.ErrorIs(err, assert.AnError)
	suite.count(0)
}

func (suite *OutboxTestSuite) TestDequeueError() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)
	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		suite.Contains(suite.ids, msg.ID)
		return assert.AnError
	})
	suite.NoError(err)

	// Plain errors count as a retry.
	msg := suite.find(suite.ids[0])
	suite.Equal(int32(1), msg.Attempts)
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
		err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
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
	suite.createN(1)

	for range suite.maxRetry - 1 {
		err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			return assert.AnError
		})
		suite.NoError(err)
		suite.count(1)
	}

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		return assert.AnError
	})
	suite.NoError(err)
	suite.count(0)

	err = ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		panic("will not be called")
	})
	suite.ErrorIs(err, outbox.ErrEOQ)
	suite.count(0)

	dead, err := ob.DeadLetters(ctx, 10)
	suite.NoError(err)
	suite.Require().Len(dead, 1)
	suite.Equal(outbox.StatusDead, dead[0].Status)
	suite.Equal(assert.AnError.Error(), dead[0].LastError)
}

func (suite *OutboxTestSuite) TestDequeueErrorRollsBack() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		_, err := ob.Enqueue(txCtx, suite.params(2))
		suite.NoError(err)
		return assert.AnError
	})
	suite.NoError(err)

	// The message enqueued by the failed handler is rolled back.
	suite.count(1)
	msg := suite.find(suite.ids[0])
	suite.Equal(outbox.StatusPending, msg.Status)
	suite.Equal(assert.AnError.Error(), msg.LastError)
}

func (suite *OutboxTestSuite) TestDequeueSuccessCommits() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		_, err := ob.Enqueue(txCtx, suite.params(2))
		return err
	})
	suite.NoError(err)

	// The original message is done, the one enqueued by the handler is visible.
	suite.count(1)
	suite.Equal(outbox.StatusDone, suite.find(suite.ids[0]).Status)
}

func (suite *OutboxTestSuite) TestDequeueBatch() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(3)

	var seen []uuid.UUID
	err := ob.Dequeue(ctx, 10, func(txCtx context.Context, msg *outbox.Message) error {
		seen = append(seen, msg.ID)
		return nil
	})
	suite.NoError(err)
	suite.Equal(suite.ids, seen)
	suite.count(0)
}

func (suite *OutboxTestSuite) TestEnqueueOptions() {
	ctx := suite.T().Context()
	ob := suite.ob

	params := suite.params(1)
	params.AvailableAt = time.Now().Add(time.Hour)
	delayed, err := ob.Enqueue(ctx, params)
	suite.NoError(err)
	suite.count(0)

	params = suite.params(2)
	params.MaxAttempts = 1
	id, err := ob.Enqueue(ctx, params)
	suite.NoError(err)
	suite.count(1)

	err = ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		suite.Equal(id, msg.ID)
		return assert.AnError
	})
	suite.NoError(err)
	suite.count(0)
	suite.Equal(outbox.StatusDead, suite.find(id).Status)
	suite.Equal(outbox.StatusPending, suite.find(delayed).Status)
}

func (suite *OutboxTestSuite) TestLeaseExpiredOnFinalAttempt() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	// Simulate a worker that crashed on the final attempt.
	_, err := suite.repo.DBTx(ctx).ExecContext(ctx, `
		update dbtx.outbox
		   set status = 'processing', locked_by = 'crashed', attempts = max_attempts
		 where id = $1`, suite.ids[0])
	suite.Require().NoError(err)
	suite.count(0)

	err = ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		panic("will not be called")
	})
	suite.ErrorIs(err, outbox.ErrEOQ)

	msg := suite.find(suite.ids[0])
	suite.Equal(outbox.StatusDead, msg.Status)
	suite.Equal("lease expired on final attempt", msg.LastError)
}

func (suite *OutboxTestSuite) TestPurge() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(2)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		return nil
	})
	suite.NoError(err)

	_, err = ob.Purge(ctx, outbox.StatusPending, time.Now().Add(time.Hour))
	suite.Error(err)

	n, err := ob.Purge(ctx, outbox.StatusDone, time.Now().Add(time.Hour))
	suite.NoError(err)
	suite.Equal(int64(1), n)
	suite.count(1)
}

func (suite *OutboxTestSuite) TestDeadLetter() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	errs := []error{nil, outbox.ErrEOQ}
	count := []int{0, 0}

	for i := range len(count) {
		err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			return fmt.Errorf("%w: %w", outbox.ErrDeadLetter, assert.AnError)
		})
		suite.ErrorIs(err, errs[i], "errs[%d]", i)
		suite.count(count[i], "count[%d]", i)
	}
}

func (suite *OutboxTestSuite) TestDeadLetters() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(2)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *outbox.Message) error {
		return errors.Join(assert.AnError, outbox.ErrDeadLetter)
	})
	suite.NoError(err)
	suite.count(1)

	dead, err := ob.DeadLetters(ctx, 10)
	suite.NoError(err)
	suite.Require().Len(dead, 1)

	// Only dead messages can be requeued.
	err = ob.Requeue(ctx, suite.ids[1])
	suite.ErrorIs(err, outbox.ErrNotFound)

	err = ob.Requeue(ctx, dead[0].ID)
	suite.NoError(err)
	suite.count(2)

	// Kill it again, then purge.
	err = ob.Dequeue(ctx, 2, func(ctx context.Context, msg *outbox.Message) error {
		return outbox.ErrDeadLetter
	})
	suite.NoError(err)

	n, err := ob.Purge(ctx, outbox.StatusDead, time.Now().Add(-time.Hour))
	suite.NoError(err)
	suite.Equal(int64(0), n)

	n, err = ob.Purge(ctx, outbox.StatusDead, time.Now().Add(time.Hour))
	suite.NoError(err)
	suite.Equal(int64(2), n)
	suite.count(0)
}

func TestDefaultBackoff(t *testing.T) {
	is := assert.New(t)

	cases := []struct {
		attempts int32
		min, max time.Duration
	}{
		{1, 500 * time.Millisecond, time.Second},
		{2, time.Second, 2 * time.Second},
		{4, 4 * time.Second, 8 * time.Second},
		{20, 30 * time.Minute, time.Hour},
	}
	for _, tc := range cases {
		for range 100 {
			d := outbox.DefaultBackoff(&outbox.Message{Attempts: tc.attempts})
			is.GreaterOrEqual(d, tc.min, "attempts %d", tc.attempts)
			is.LessOrEqual(d, tc.max, "attempts %d", tc.attempts)
		}
	}
}

/* Helpers */

func (suite *OutboxTestSuite) params(i int) outbox.EnqueueParams {
	return outbox.EnqueueParams{
		AggregateID:   fmt.Sprintf("a-id-%d", i),
		AggregateType: fmt.Sprintf("a-type-%d", i),
		EventType:     fmt.Sprintf("type-%d", i),
		Payload:       jsontext.Value(`{"foo": "bar"}`),
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

func (suite *OutboxTestSuite) TestDequeueInTx() {
	ctx := suite.T().Context()
	suite.createN(1)

	var calls int
	handle := func(context.Context, *outbox.Message) error {
		calls++
		return nil
	}
	err := suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
		return suite.ob.Dequeue(txCtx, 1, handle)
	})
	suite.ErrorIs(err, outbox.ErrTxInContext)
	suite.Zero(calls)

	// The message was not claimed.
	suite.NoError(suite.ob.Dequeue(ctx, 1, handle))
	suite.Equal(1, calls)
}

func (suite *OutboxTestSuite) TestBatchLeaseExpired() {
	ctx := suite.T().Context()
	suite.createN(2)
	suite.ob.Lease = 300 * time.Millisecond

	var mu sync.Mutex
	handled := map[uuid.UUID]int{}
	handle := func(_ context.Context, msg *outbox.Message) error {
		mu.Lock()
		handled[msg.ID]++
		mu.Unlock()
		if msg.ID == suite.ids[0] {
			time.Sleep(600 * time.Millisecond)
		}
		return nil
	}

	// Worker A handles the first message for longer than the batch lease.
	var errA error
	done := make(chan struct{})
	go func() {
		defer close(done)
		errA = suite.ob.Dequeue(ctx, 2, handle)
	}()
	time.Sleep(400 * time.Millisecond)

	// Worker B reclaims the second message, whose lease expired while it
	// waited for its turn. The first one is locked by A's handler.
	b := outbox.New(suite.repo)
	suite.NoError(b.Dequeue(ctx, 10, handle))
	<-done

	// A skips the message B handled.
	suite.ErrorIs(errA, outbox.ErrLeaseExpired)
	suite.Equal(map[uuid.UUID]int{suite.ids[0]: 1, suite.ids[1]: 1}, handled)
	for _, id := range suite.ids {
		suite.Equal(outbox.StatusDone, suite.find(id).Status)
	}
}

func (suite *OutboxTestSuite) TestDequeueCancelReleases() {
	ctx, cancel := context.WithCancel(suite.T().Context())
	defer cancel()
	suite.createN(3)

	var calls int
	err := suite.ob.Dequeue(ctx, 3, func(context.Context, *outbox.Message) error {
		calls++
		cancel()
		return nil
	})
	suite.ErrorIs(err, context.Canceled)
	suite.Equal(1, calls)

	// The messages not handled yet keep their attempt, and can be dequeued
	// again right away.
	for _, id := range suite.ids[1:] {
		msg := suite.find(id)
		suite.Equal(outbox.StatusPending, msg.Status)
		suite.Zero(msg.Attempts)
	}
	suite.count(2)
}

func (suite *OutboxTestSuite) TestDequeuePanicReleases() {
	ctx := suite.T().Context()
	suite.createN(3)

	func() {
		defer func() {
			suite.Equal("boom", recover())
		}()
		_ = suite.ob.Dequeue(ctx, 3, func(context.Context, *outbox.Message) error {
			panic("boom")
		})
	}()

	// The panicking message used its attempt, and is retried once its lease
	// expires. The others are released.
	msg := suite.find(suite.ids[0])
	suite.Equal(outbox.StatusProcessing, msg.Status)
	suite.Equal(int32(1), msg.Attempts)
	for _, id := range suite.ids[1:] {
		msg := suite.find(id)
		suite.Equal(outbox.StatusPending, msg.Status)
		suite.Zero(msg.Attempts)
	}
}

func (suite *OutboxTestSuite) TestFinalAttemptNotDeadWhileHandled() {
	ctx := suite.T().Context()
	params := suite.params(1)
	params.MaxAttempts = 1
	id, err := suite.ob.Enqueue(ctx, params)
	suite.Require().NoError(err)
	suite.ob.Lease = 200 * time.Millisecond

	started := make(chan struct{})
	done := make(chan struct{})
	var errA error
	go func() {
		defer close(done)
		errA = suite.ob.Dequeue(ctx, 1, func(context.Context, *outbox.Message) error {
			close(started)
			time.Sleep(500 * time.Millisecond)
			return nil
		})
	}()
	<-started
	time.Sleep(300 * time.Millisecond)

	// The lease expired on the final attempt, but the handler is still
	// running, so another worker does not mark the message as dead.
	err = outbox.New(suite.repo).Dequeue(ctx, 1, func(context.Context, *outbox.Message) error {
		suite.Fail("delivered twice")
		return nil
	})
	suite.ErrorIs(err, outbox.ErrEOQ)
	<-done

	suite.NoError(errA)
	suite.Equal(outbox.StatusDone, suite.find(id).Status)
}

func (suite *OutboxTestSuite) TestInvalidMaxAttempts() {
	ctx := suite.T().Context()
	params := suite.params(1)
	params.MaxAttempts = -1
	_, err := suite.ob.Enqueue(ctx, params)
	suite.ErrorIs(err, outbox.ErrInvalidMaxAttempts)

	// The schema rejects messages that could never be delivered.
	suite.createN(1)
	_, err = suite.repo.DBTx(ctx).ExecContext(ctx, `
		update dbtx.outbox set max_attempts = 0 where id = $1`, suite.ids[0])
	suite.ErrorContains(err, "check constraint")
}
