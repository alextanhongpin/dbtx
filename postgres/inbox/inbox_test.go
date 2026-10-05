package inbox_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	_ "github.com/lib/pq"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/inbox"
	"github.com/alextanhongpin/dbtx/postgres/inbox/repository"
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

func TestInboxTestSuite(t *testing.T) {
	suite.Run(t, new(InboxTestSuite))
}

type InboxTestSuite struct {
	suite.Suite
	ids      []uuid.UUID
	ob       *inbox.Inbox
	repo     *repository.Repository
	maxRetry int
}

func (suite *InboxTestSuite) SetupTest() {
	t := suite.T()

	db := dbtest.New(t, dbtestOpts).DB(t)
	_, err := db.ExecContext(t.Context(), `delete from dbtx.inbox`)
	suite.Require().NoError(err)

	suite.repo = repository.New(db)
	suite.ob = inbox.New(suite.repo)
	suite.ob.Backoff = func(msg *inbox.Message) time.Duration {
		return 0
	}
	suite.ids = nil
	// Default.
	suite.maxRetry = 10
}

func (suite *InboxTestSuite) TestEnqueueError() {
	ctx := suite.T().Context()

	err := suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
		_, err := suite.ob.Enqueue(txCtx, suite.params(1))
		suite.NoError(err)
		return assert.AnError
	})
	suite.ErrorIs(err, assert.AnError)
	suite.count(0)
}

func (suite *InboxTestSuite) TestDequeueError() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)
	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		suite.Contains(suite.ids, msg.ID)
		return assert.AnError
	})
	suite.NoError(err)

	// Plain errors count as a retry.
	msg := suite.find(suite.ids[0])
	suite.Equal(int32(1), msg.Attempts)
	suite.count(1)
}

func (suite *InboxTestSuite) TestDequeueSuccess() {
	ctx := suite.T().Context()
	ob := suite.ob
	n := 2
	suite.createN(n)

	errs := []error{nil, nil, inbox.ErrEOQ}
	counts := []int{1, 0, 0}

	for i := range n + 1 {
		err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			return nil
		})
		suite.ErrorIs(err, errs[i])
		suite.count(counts[i])
	}
}

func (suite *InboxTestSuite) TestMaxRetry() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	for range suite.maxRetry - 1 {
		err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			return assert.AnError
		})
		suite.NoError(err)
		suite.count(1)
	}

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		return assert.AnError
	})
	suite.NoError(err)
	suite.count(0)

	err = ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		panic("will not be called")
	})
	suite.ErrorIs(err, inbox.ErrEOQ)
	suite.count(0)

	dead, err := ob.DeadLetters(ctx, 10)
	suite.NoError(err)
	suite.Require().Len(dead, 1)
	suite.Equal(inbox.StatusDead, dead[0].Status)
	suite.Equal(assert.AnError.Error(), dead[0].LastError)
}

func (suite *InboxTestSuite) TestDequeueErrorRollsBack() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		_, err := ob.Enqueue(txCtx, suite.params(2))
		suite.NoError(err)
		return assert.AnError
	})
	suite.NoError(err)

	// The message enqueued by the failed handler is rolled back.
	suite.count(1)
	msg := suite.find(suite.ids[0])
	suite.Equal(inbox.StatusPending, msg.Status)
	suite.Equal(assert.AnError.Error(), msg.LastError)
}

func (suite *InboxTestSuite) TestDequeueSuccessCommits() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		_, err := ob.Enqueue(txCtx, suite.params(2))
		return err
	})
	suite.NoError(err)

	// The original message is done, the one enqueued by the handler is visible.
	suite.count(1)
	msg := suite.find(suite.ids[0])
	suite.Equal(inbox.StatusDone, msg.Status)
	suite.False(msg.ProcessedAt.IsZero())
	suite.Empty(msg.LockedBy)
}

func (suite *InboxTestSuite) TestEnqueueDuplicate() {
	ctx := suite.T().Context()
	suite.createN(1)

	_, err := suite.ob.Enqueue(ctx, suite.params(1))
	suite.ErrorIs(err, inbox.ErrExists)
	suite.count(1)
}

func (suite *InboxTestSuite) TestEnqueueDBError() {
	ctx, cancel := context.WithCancel(suite.T().Context())
	cancel()

	id, err := suite.ob.Enqueue(ctx, suite.params(1))
	suite.ErrorIs(err, context.Canceled)
	suite.Equal(uuid.Nil(), id)
}

func (suite *InboxTestSuite) TestLeaseLost() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(2)

	var handled []uuid.UUID
	err := ob.Dequeue(ctx, 2, func(txCtx context.Context, msg *inbox.Message) error {
		handled = append(handled, msg.ID)

		// Simulate another worker claiming the next message after its lease
		// expired.
		_, err := suite.repo.DBTx(ctx).ExecContext(ctx, `
			update dbtx.inbox set locked_by = 'other' where id = $1`, suite.ids[1])
		suite.Require().NoError(err)
		return nil
	})
	suite.ErrorIs(err, inbox.ErrLeaseLost)

	// The next message is skipped, and left to the other worker.
	suite.Equal([]uuid.UUID{suite.ids[0]}, handled)
	msg := suite.find(suite.ids[1])
	suite.Equal(inbox.StatusProcessing, msg.Status)
	suite.Equal("other", msg.LockedBy)
}

func (suite *InboxTestSuite) TestClaimSkipsExhausted() {
	ctx := suite.T().Context()
	suite.createN(1)

	_, err := suite.repo.DBTx(ctx).ExecContext(ctx, `
		update dbtx.inbox set attempts = max_attempts where id = $1`, suite.ids[0])
	suite.Require().NoError(err)
	suite.count(0)

	err = suite.ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		panic("will not be called")
	})
	suite.ErrorIs(err, inbox.ErrEOQ)
}

func (suite *InboxTestSuite) TestOnError() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	var got []error
	ob.OnError = func(msg *inbox.Message, err error) {
		suite.Equal(suite.ids[0], msg.ID)
		got = append(got, err)
	}
	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		return assert.AnError
	})
	suite.NoError(err)
	suite.Equal([]error{assert.AnError}, got)
}

func (suite *InboxTestSuite) TestOrdered() {
	ctx := suite.T().Context()
	ob := suite.ob
	ob.Ordered = true

	enqueue := func(i int, aggregateID string) uuid.UUID {
		params := suite.params(i)
		params.AggregateID = aggregateID
		id, err := ob.Enqueue(ctx, params)
		suite.Require().NoError(err)
		return id
	}
	a1 := enqueue(1, "a")
	a2 := enqueue(2, "a")
	b1 := enqueue(3, "b")
	n1 := enqueue(4, "")
	n2 := enqueue(5, "")

	dequeue := func() []uuid.UUID {
		var seen []uuid.UUID
		err := ob.Dequeue(ctx, 10, func(txCtx context.Context, msg *inbox.Message) error {
			seen = append(seen, msg.ID)
			return nil
		})
		suite.NoError(err)
		return seen
	}

	// One message per aggregate, messages without an aggregate are not ordered.
	suite.Equal([]uuid.UUID{a1, b1, n1, n2}, dequeue())
	suite.Equal([]uuid.UUID{a2}, dequeue())
	suite.Empty(suite.find(n1).AggregateID)
}

func (suite *InboxTestSuite) TestDequeueBatch() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(3)

	var seen []uuid.UUID
	err := ob.Dequeue(ctx, 10, func(txCtx context.Context, msg *inbox.Message) error {
		seen = append(seen, msg.ID)
		return nil
	})
	suite.NoError(err)
	suite.Equal(suite.ids, seen)
	suite.count(0)
}

func (suite *InboxTestSuite) TestEnqueueOptions() {
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

	err = ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		suite.Equal(id, msg.ID)
		return assert.AnError
	})
	suite.NoError(err)
	suite.count(0)
	suite.Equal(inbox.StatusDead, suite.find(id).Status)
	suite.Equal(inbox.StatusPending, suite.find(delayed).Status)
}

func (suite *InboxTestSuite) TestLeaseExpiredOnFinalAttempt() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	// Simulate a worker that crashed on the final attempt.
	_, err := suite.repo.DBTx(ctx).ExecContext(ctx, `
		update dbtx.inbox
		   set status = 'processing', locked_by = 'crashed', attempts = max_attempts
		 where id = $1`, suite.ids[0])
	suite.Require().NoError(err)
	suite.count(0)

	err = ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		panic("will not be called")
	})
	suite.ErrorIs(err, inbox.ErrEOQ)

	msg := suite.find(suite.ids[0])
	suite.Equal(inbox.StatusDead, msg.Status)
	suite.Equal("lease expired on final attempt", msg.LastError)
}

func (suite *InboxTestSuite) TestPurge() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(2)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		return nil
	})
	suite.NoError(err)

	_, err = ob.Purge(ctx, inbox.StatusPending, time.Now().Add(time.Hour))
	suite.Error(err)

	n, err := ob.Purge(ctx, inbox.StatusDone, time.Now().Add(time.Hour))
	suite.NoError(err)
	suite.Equal(int64(1), n)
	suite.count(1)
}

func (suite *InboxTestSuite) TestDeadLetter() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(1)

	errs := []error{nil, inbox.ErrEOQ}
	count := []int{0, 0}

	for i := range len(count) {
		err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
			suite.True(dbtx.IsTx(txCtx))
			suite.Contains(suite.ids, msg.ID)
			return fmt.Errorf("%w: %w", inbox.ErrDeadLetter, assert.AnError)
		})
		suite.ErrorIs(err, errs[i], "errs[%d]", i)
		suite.count(count[i], "count[%d]", i)
	}
}

func (suite *InboxTestSuite) TestDeadLetters() {
	ctx := suite.T().Context()
	ob := suite.ob
	suite.createN(2)

	err := ob.Dequeue(ctx, 1, func(txCtx context.Context, msg *inbox.Message) error {
		return errors.Join(assert.AnError, inbox.ErrDeadLetter)
	})
	suite.NoError(err)
	suite.count(1)

	dead, err := ob.DeadLetters(ctx, 10)
	suite.NoError(err)
	suite.Require().Len(dead, 1)

	// Only dead messages can be requeued.
	err = ob.Requeue(ctx, suite.ids[1])
	suite.ErrorIs(err, inbox.ErrNotFound)

	err = ob.Requeue(ctx, dead[0].ID)
	suite.NoError(err)
	suite.count(2)

	// Kill it again, then purge.
	err = ob.Dequeue(ctx, 2, func(ctx context.Context, msg *inbox.Message) error {
		return inbox.ErrDeadLetter
	})
	suite.NoError(err)

	n, err := ob.Purge(ctx, inbox.StatusDead, time.Now().Add(-time.Hour))
	suite.NoError(err)
	suite.Equal(int64(0), n)

	n, err = ob.Purge(ctx, inbox.StatusDead, time.Now().Add(time.Hour))
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
			d := inbox.DefaultBackoff(&inbox.Message{Attempts: tc.attempts})
			is.GreaterOrEqual(d, tc.min, "attempts %d", tc.attempts)
			is.LessOrEqual(d, tc.max, "attempts %d", tc.attempts)
		}
	}
}

/* Helpers */

func (suite *InboxTestSuite) params(i int) inbox.EnqueueParams {
	return inbox.EnqueueParams{
		Source:      fmt.Sprintf("source-%d", i),
		MessageID:   fmt.Sprintf("msg-id-%d", i),
		MessageType: fmt.Sprintf("msg-type-%d", i),
		AggregateID: fmt.Sprintf("a-id-%d", i),
		Payload:     jsontext.Value(`{"foo": "bar"}`),
	}
}

func (suite *InboxTestSuite) createN(n int) {
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

func (suite *InboxTestSuite) find(id uuid.UUID) *inbox.Message {
	t := suite.T()
	t.Helper()

	msg, err := suite.repo.Find(t.Context(), id)
	suite.Require().NoError(err)
	return msg
}

func (suite *InboxTestSuite) count(n int, msgAndArgs ...any) {
	t := suite.T()
	t.Helper()
	ctx := t.Context()

	count, err := suite.ob.Count(ctx)
	suite.NoError(err)
	suite.Equal(int64(n), count, msgAndArgs...)
}

func (suite *InboxTestSuite) TestDequeueInTx() {
	ctx := suite.T().Context()
	suite.createN(1)

	var calls int
	handle := func(context.Context, *inbox.Message) error {
		calls++
		return nil
	}
	err := suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
		return suite.ob.Dequeue(txCtx, 1, handle)
	})
	suite.ErrorIs(err, inbox.ErrTxInContext)
	suite.Zero(calls)

	// The message was not claimed.
	suite.NoError(suite.ob.Dequeue(ctx, 1, handle))
	suite.Equal(1, calls)
}

func (suite *InboxTestSuite) TestBatchLeaseExpired() {
	ctx := suite.T().Context()
	suite.createN(2)
	suite.ob.Lease = 300 * time.Millisecond

	var mu sync.Mutex
	handled := map[uuid.UUID]int{}
	handle := func(_ context.Context, msg *inbox.Message) error {
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
	b := inbox.New(suite.repo)
	suite.NoError(b.Dequeue(ctx, 10, handle))
	<-done

	// A skips the message B handled.
	suite.ErrorIs(errA, inbox.ErrLeaseLost)
	suite.Equal(map[uuid.UUID]int{suite.ids[0]: 1, suite.ids[1]: 1}, handled)
	for _, id := range suite.ids {
		suite.Equal(inbox.StatusDone, suite.find(id).Status)
	}
}

func (suite *InboxTestSuite) TestDequeueCancelReleases() {
	ctx, cancel := context.WithCancel(suite.T().Context())
	defer cancel()
	suite.createN(3)

	var calls int
	err := suite.ob.Dequeue(ctx, 3, func(context.Context, *inbox.Message) error {
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
		suite.Equal(inbox.StatusPending, msg.Status)
		suite.Zero(msg.Attempts)
	}
	suite.count(2)
}

func (suite *InboxTestSuite) TestDequeuePanicReleases() {
	ctx := suite.T().Context()
	suite.createN(3)

	func() {
		defer func() {
			suite.Equal("boom", recover())
		}()
		_ = suite.ob.Dequeue(ctx, 3, func(context.Context, *inbox.Message) error {
			panic("boom")
		})
	}()

	// The panicking message used its attempt, and is retried once its lease
	// expires. The others are released.
	msg := suite.find(suite.ids[0])
	suite.Equal(inbox.StatusProcessing, msg.Status)
	suite.Equal(int32(1), msg.Attempts)
	for _, id := range suite.ids[1:] {
		msg := suite.find(id)
		suite.Equal(inbox.StatusPending, msg.Status)
		suite.Zero(msg.Attempts)
	}
}

func (suite *InboxTestSuite) TestFinalAttemptNotDeadWhileHandled() {
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
		errA = suite.ob.Dequeue(ctx, 1, func(context.Context, *inbox.Message) error {
			close(started)
			time.Sleep(500 * time.Millisecond)
			return nil
		})
	}()
	<-started
	time.Sleep(300 * time.Millisecond)

	// The lease expired on the final attempt, but the handler is still
	// running, so another worker does not mark the message as dead.
	err = inbox.New(suite.repo).Dequeue(ctx, 1, func(context.Context, *inbox.Message) error {
		suite.Fail("delivered twice")
		return nil
	})
	suite.ErrorIs(err, inbox.ErrEOQ)
	<-done

	suite.NoError(errA)
	suite.Equal(inbox.StatusDone, suite.find(id).Status)
}

// concurrency counts the handlers running at once.
type concurrency struct {
	active, max atomic.Int32
}

func (c *concurrency) handle(d time.Duration) inbox.HandlerFunc {
	return func(context.Context, *inbox.Message) error {
		n := c.active.Add(1)
		defer c.active.Add(-1)
		for {
			m := c.max.Load()
			if n <= m || c.max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(d)
		return nil
	}
}

func (suite *InboxTestSuite) enqueueAggregate(i int, aggregateID string) uuid.UUID {
	params := suite.params(i)
	params.AggregateID = aggregateID
	id, err := suite.ob.Enqueue(suite.T().Context(), params)
	suite.Require().NoError(err)
	return id
}

func (suite *InboxTestSuite) TestOrderedRequeue() {
	ctx := suite.T().Context()
	ob := suite.ob
	ob.Ordered = true
	first := suite.enqueueAggregate(1, "x")
	second := suite.enqueueAggregate(2, "x")

	suite.NoError(ob.Dequeue(ctx, 10, func(context.Context, *inbox.Message) error {
		return inbox.ErrDeadLetter
	}))
	suite.Equal(inbox.StatusDead, suite.find(first).Status)

	// While the second message is handled, the first one is requeued.
	var c concurrency
	done := make(chan error)
	go func() {
		done <- ob.Dequeue(ctx, 10, c.handle(500*time.Millisecond))
	}()
	time.Sleep(100 * time.Millisecond)
	suite.NoError(ob.Requeue(ctx, first))

	// The requeued message waits for the second one.
	suite.ErrorIs(ob.Dequeue(ctx, 10, c.handle(0)), inbox.ErrEOQ)
	suite.NoError(<-done)
	suite.Equal(inbox.StatusDone, suite.find(second).Status)

	suite.NoError(ob.Dequeue(ctx, 10, c.handle(0)))
	suite.Equal(inbox.StatusDone, suite.find(first).Status)
	suite.Equal(int32(1), c.max.Load())
}

func (suite *InboxTestSuite) TestOrderedLateCommit() {
	ctx := suite.T().Context()
	ob := suite.ob
	ob.Ordered = true

	// The first message is enqueued in a transaction that commits after the
	// second message is claimed.
	commit := make(chan struct{})
	enqueued := make(chan uuid.UUID)
	committed := make(chan error)
	go func() {
		committed <- suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
			params := suite.params(1)
			params.AggregateID = "x"
			id, err := ob.Enqueue(txCtx, params)
			enqueued <- id
			<-commit
			return err
		})
	}()
	first := <-enqueued
	suite.enqueueAggregate(2, "x")

	var c concurrency
	done := make(chan error)
	go func() {
		done <- ob.Dequeue(ctx, 10, c.handle(500*time.Millisecond))
	}()
	time.Sleep(100 * time.Millisecond)
	close(commit)
	suite.NoError(<-committed)

	// The first message waits for the second one.
	suite.ErrorIs(ob.Dequeue(ctx, 10, c.handle(0)), inbox.ErrEOQ)
	suite.NoError(<-done)

	suite.NoError(ob.Dequeue(ctx, 10, c.handle(0)))
	suite.Equal(inbox.StatusDone, suite.find(first).Status)
	suite.Equal(int32(1), c.max.Load())
}

func (suite *InboxTestSuite) TestOrderedHandlerOutlivesLease() {
	ctx := suite.T().Context()
	ob := suite.ob
	ob.Ordered = true
	ob.Lease = 200 * time.Millisecond

	commit := make(chan struct{})
	enqueued := make(chan struct{})
	committed := make(chan error)
	go func() {
		committed <- suite.repo.RunInTx(ctx, func(txCtx context.Context) error {
			params := suite.params(1)
			params.AggregateID = "x"
			_, err := ob.Enqueue(txCtx, params)
			close(enqueued)
			<-commit
			return err
		})
	}()
	<-enqueued
	suite.enqueueAggregate(2, "x")

	// The second message's handler runs past its lease, so Claim no longer
	// sees the aggregate as busy once the first message commits.
	var c concurrency
	done := make(chan error)
	go func() {
		done <- ob.Dequeue(ctx, 10, c.handle(600*time.Millisecond))
	}()
	time.Sleep(300 * time.Millisecond)
	close(commit)
	suite.NoError(<-committed)

	// The first message is claimed, but its handler waits for the second.
	suite.NoError(ob.Dequeue(ctx, 10, c.handle(0)))
	suite.NoError(<-done)
	suite.Equal(int32(1), c.max.Load())
}

func (suite *InboxTestSuite) TestInvalidMaxAttempts() {
	ctx := suite.T().Context()
	params := suite.params(1)
	params.MaxAttempts = -1
	_, err := suite.ob.Enqueue(ctx, params)
	suite.ErrorIs(err, inbox.ErrInvalidMaxAttempts)

	// The schema rejects messages that could never be delivered.
	suite.createN(1)
	_, err = suite.repo.DBTx(ctx).ExecContext(ctx, `
		update dbtx.inbox set max_attempts = 0 where id = $1`, suite.ids[0])
	suite.ErrorContains(err, "check constraint")
}

func (suite *InboxTestSuite) TestExpiredLeaseLockedIsSkipped() {
	ctx := suite.T().Context()
	suite.createN(1)

	// A worker's lease expired on the final attempt, but its handler still
	// holds the row lock.
	_, err := suite.repo.DBTx(ctx).ExecContext(ctx, `
		update dbtx.inbox
		   set status = 'processing', locked_by = 'slow', attempts = max_attempts,
		       available_at = now() - interval '1 second'
		 where id = $1`, suite.ids[0])
	suite.Require().NoError(err)
	tx, err := suite.repo.Unwrap().BeginTx(ctx, nil)
	suite.Require().NoError(err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `select 1 from dbtx.inbox where id = $1 for update`, suite.ids[0])
	suite.Require().NoError(err)

	// Dequeue does not wait for the handler, and leaves the message alone.
	timeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	err = suite.ob.Dequeue(timeout, 1, func(context.Context, *inbox.Message) error {
		suite.Fail("delivered while locked")
		return nil
	})
	suite.ErrorIs(err, inbox.ErrEOQ)
	suite.Less(time.Since(start), time.Second)
	suite.Equal(inbox.StatusProcessing, suite.find(suite.ids[0]).Status)

	// Once the handler is gone, the message is dead.
	suite.NoError(tx.Rollback())
	suite.ErrorIs(suite.ob.Dequeue(ctx, 1, func(context.Context, *inbox.Message) error {
		suite.Fail("delivered after the final attempt")
		return nil
	}), inbox.ErrEOQ)
	suite.Equal(inbox.StatusDead, suite.find(suite.ids[0]).Status)
}
