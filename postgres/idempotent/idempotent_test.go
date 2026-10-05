package idempotent_test

import (
	_ "github.com/lib/pq"

	"context"
	"database/sql"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/idempotent"
	"github.com/alextanhongpin/dbtx/postgres/idempotent/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
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
	opts := dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook:  migrate,
	}
	stop := dbtest.Init(opts)
	defer stop()

	m.Run()
}

var keys sync.Map

// keyOf returns a key unique to this test run, so that repeated runs with
// -count do not see keys from earlier runs.
func keyOf(t *testing.T) string {
	k, _ := keys.LoadOrStore(t, t.Name()+"/"+uuid.NewV7().String())
	return k.(string)
}

var req = idempotent.Request{Data: []byte(`{"name": "foo"}`)}

func completed(data string) *idempotent.Result {
	return &idempotent.Result{
		Response: &idempotent.Response{
			Status: string(idempotent.StatusCompleted),
			Data:   []byte(data),
		},
	}
}

func greet(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
	return completed(`{"msg": "hi, foo"}`), nil
}

func newRepository(t *testing.T) *repository.Repository {
	t.Helper()
	return repository.New(dbtest.DB(t))
}

func newIdempotent(t *testing.T) *idempotent.Idempotent {
	t.Helper()
	return idempotent.New(newRepository(t))
}

func TestDo(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		res, err := newIdempotent(t).Do(t.Context(), keyOf(t), greet, req)
		is := assert.New(t)
		is.NoError(err)
		is.Equal(string(idempotent.StatusCompleted), res.Status)
		is.JSONEq(`{"msg": "hi, foo"}`, string(res.Data))
	})

	t.Run("cached", func(t *testing.T) {
		var calls atomic.Int64
		fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			calls.Add(1)
			return greet(ctx, p)
		}
		idp := newIdempotent(t)
		_, err := idp.Do(t.Context(), keyOf(t), fn, req)
		is := assert.New(t)
		is.NoError(err)

		// Key order and whitespace do not matter.
		res, err := idp.Do(t.Context(), keyOf(t), fn, idempotent.Request{Data: []byte(`{ "name":"foo" }`)})
		is.NoError(err)
		is.Equal(string(idempotent.StatusCompleted), res.Status)
		is.JSONEq(`{"msg": "hi, foo"}`, string(res.Data))
		is.Equal(int64(1), calls.Load())
	})

	t.Run("failed is cached", func(t *testing.T) {
		var calls atomic.Int64
		fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			calls.Add(1)
			return &idempotent.Result{
				Response: &idempotent.Response{
					Status: string(idempotent.StatusFailed),
					Data:   []byte(`{"reason": "insufficient funds"}`),
					Error:  "insufficient funds",
				},
			}, nil
		}
		idp := newIdempotent(t)
		res, err := idp.Do(t.Context(), keyOf(t), fn, req)
		is := assert.New(t)
		is.NoError(err)
		is.Equal(string(idempotent.StatusFailed), res.Status)

		res, err = idp.Do(t.Context(), keyOf(t), fn, req)
		is.NoError(err)
		is.Equal(string(idempotent.StatusFailed), res.Status)
		is.JSONEq(`{"reason": "insufficient funds"}`, string(res.Data))
		is.Equal(int64(1), calls.Load())
	})

	t.Run("request mismatch", func(t *testing.T) {
		idp := newIdempotent(t)
		_, err := idp.Do(t.Context(), keyOf(t), greet, req)
		is := assert.New(t)
		is.NoError(err)

		_, err = idp.Do(t.Context(), keyOf(t), greet, idempotent.Request{Data: []byte(`{"name": "bar"}`)})
		is.ErrorIs(err, idempotent.ErrRequestMismatch)
	})

	t.Run("handler error is returned and retried", func(t *testing.T) {
		wantErr := errors.New("bad request")
		var calls atomic.Int64
		fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			if calls.Add(1) == 1 {
				return nil, wantErr
			}
			return greet(ctx, p)
		}
		idp := newIdempotent(t)
		res, err := idp.Do(t.Context(), keyOf(t), fn, req)
		is := assert.New(t)
		is.ErrorIs(err, wantErr)
		is.Nil(res)

		res, err = idp.Do(t.Context(), keyOf(t), fn, req)
		is.NoError(err)
		is.Equal(string(idempotent.StatusCompleted), res.Status)
		is.Equal(int64(2), calls.Load())
	})

	t.Run("invalid result", func(t *testing.T) {
		results := map[string]*idempotent.Result{
			"nil":   nil,
			"empty": {},
			"both": {
				Checkpoint: &idempotent.Checkpoint{Name: "step", Data: []byte(`null`)},
				Response:   completed(`null`).Response,
			},
		}
		for name, result := range results {
			t.Run(name, func(t *testing.T) {
				fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
					return result, nil
				}
				idp := newIdempotent(t)
				_, err := idp.Do(t.Context(), keyOf(t), fn, req)
				is := assert.New(t)
				is.ErrorIs(err, idempotent.ErrInvalidResult)

				// The key is released, not left in progress.
				res, err := idp.Do(t.Context(), keyOf(t), greet, req)
				is.NoError(err)
				is.Equal(string(idempotent.StatusCompleted), res.Status)
			})
		}
	})

	t.Run("max attempts", func(t *testing.T) {
		wantErr := errors.New("bad request")
		var calls atomic.Int64
		fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			calls.Add(1)
			return nil, wantErr
		}
		idp := newIdempotent(t)
		idp.MaxAttempts = 2
		is := assert.New(t)
		for range 2 {
			_, err := idp.Do(t.Context(), keyOf(t), fn, req)
			is.ErrorIs(err, wantErr)
		}

		// The last failed attempt marks the key as failed.
		res, err := idp.Do(t.Context(), keyOf(t), fn, req)
		is.NoError(err)
		is.Equal(string(idempotent.StatusFailed), res.Status)
		is.Equal(wantErr.Error(), res.Error)
		is.Equal(int64(2), calls.Load())
	})

	t.Run("backoff", func(t *testing.T) {
		wantErr := errors.New("bad request")
		var calls atomic.Int64
		fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			if calls.Add(1) == 1 {
				return nil, wantErr
			}
			return greet(ctx, p)
		}
		idp := newIdempotent(t)
		idp.BaseBackoff = 200 * time.Millisecond
		idp.MaxBackoff = time.Second
		_, err := idp.Do(t.Context(), keyOf(t), fn, req)
		is := assert.New(t)
		is.ErrorIs(err, wantErr)

		_, err = idp.Do(t.Context(), keyOf(t), fn, req)
		is.ErrorIs(err, idempotent.ErrBackoff)

		time.Sleep(300 * time.Millisecond)
		res, err := idp.Do(t.Context(), keyOf(t), fn, req)
		is.NoError(err)
		is.Equal(string(idempotent.StatusCompleted), res.Status)
		is.Equal(int64(2), calls.Load())
	})

	t.Run("max attempts after expired lease", func(t *testing.T) {
		repo := newRepository(t)
		_, err := repo.Claim(t.Context(), idempotent.ClaimParams{
			IdempotencyKey: keyOf(t),
			LeaseOwner:     "crashed",
			LeaseSeconds:   0.1,
			MaxAttempts:    1,
			Request:        req.Data,
		})
		is := assert.New(t)
		is.NoError(err)
		time.Sleep(200 * time.Millisecond)

		idp := idempotent.New(repo)
		idp.MaxAttempts = 1
		_, err = idp.Do(t.Context(), keyOf(t), greet, req)
		is.ErrorIs(err, idempotent.ErrMaxAttempts)
	})

	t.Run("in flight", func(t *testing.T) {
		repo := newRepository(t)
		_, err := repo.Claim(t.Context(), idempotent.ClaimParams{
			IdempotencyKey: keyOf(t),
			LeaseOwner:     "other",
			LeaseSeconds:   60,
			MaxAttempts:    idempotent.DefaultMaxAttempts,
			Request:        req.Data,
		})
		is := assert.New(t)
		is.NoError(err)

		_, err = idempotent.New(repo).Do(t.Context(), keyOf(t), greet, req)
		is.ErrorIs(err, idempotent.ErrRequestInFlight)
	})

	t.Run("empty data", func(t *testing.T) {
		statuses := map[string]idempotent.Status{
			"completed": idempotent.StatusCompleted,
			"failed":    idempotent.StatusFailed,
		}
		for name, status := range statuses {
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int64
				fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
					calls.Add(1)
					if p.Checkpoint.Name != "step" {
						return &idempotent.Result{
							Checkpoint: &idempotent.Checkpoint{Name: "step"},
						}, nil
					}
					return &idempotent.Result{
						Response: &idempotent.Response{Status: string(status)},
					}, nil
				}
				idp := newIdempotent(t)
				res, err := idp.Do(t.Context(), keyOf(t), fn, idempotent.Request{})
				is := assert.New(t)
				is.NoError(err)
				is.Equal(string(status), res.Status)
				is.Equal(int64(2), calls.Load())

				// Nil and empty requests are the same request.
				res, err = idp.Do(t.Context(), keyOf(t), fn, idempotent.Request{Data: []byte{}})
				is.NoError(err)
				is.Equal(string(status), res.Status)
				is.JSONEq(`null`, string(res.Data))
				is.Equal(int64(2), calls.Load())
			})
		}
	})

	t.Run("invalid max attempts", func(t *testing.T) {
		for _, n := range []int{-1, math.MaxInt32 + 1} {
			idp := newIdempotent(t)
			idp.MaxAttempts = n
			_, err := idp.Do(t.Context(), keyOf(t), greet, req)
			assert.ErrorIs(t, err, idempotent.ErrInvalidConfig, n)
		}

		// The key was not claimed.
		res, err := newIdempotent(t).Do(t.Context(), keyOf(t), greet, req)
		is := assert.New(t)
		is.NoError(err)
		is.Equal(string(idempotent.StatusCompleted), res.Status)
	})

	t.Run("in transaction", func(t *testing.T) {
		repo := newRepository(t)
		var calls atomic.Int64
		fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			calls.Add(1)
			return greet(ctx, p)
		}
		err := repo.RunInTx(t.Context(), func(txCtx context.Context) error {
			_, err := idempotent.New(repo).Do(txCtx, keyOf(t), fn, req)
			return err
		})
		is := assert.New(t)
		is.ErrorIs(err, idempotent.ErrTxInContext)
		is.Zero(calls.Load())

		// The key was not claimed.
		res, err := idempotent.New(repo).Do(t.Context(), keyOf(t), fn, req)
		is.NoError(err)
		is.Equal(string(idempotent.StatusCompleted), res.Status)
		is.Equal(int64(1), calls.Load())
	})

	t.Run("concurrent", func(t *testing.T) {
		var calls atomic.Int64
		fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			calls.Add(1)
			time.Sleep(50 * time.Millisecond)
			return greet(ctx, p)
		}
		idp := newIdempotent(t)
		key := keyOf(t)

		n := 10
		var wg sync.WaitGroup
		var done, inFlight atomic.Int64
		errs := make(chan error, n)
		start := make(chan struct{})
		for range n {
			wg.Go(func() {
				<-start
				_, err := idp.Do(t.Context(), key, fn, req)
				switch {
				case err == nil:
					done.Add(1)
				case errors.Is(err, idempotent.ErrRequestInFlight):
					inFlight.Add(1)
				default:
					errs <- err
				}
			})
		}
		close(start)
		wg.Wait()
		close(errs)

		is := assert.New(t)
		for err := range errs {
			is.NoError(err)
		}
		is.Equal(int64(n), done.Load()+inFlight.Load())
		is.Equal(int64(1), calls.Load())
	})
}

func TestCheckpoint(t *testing.T) {
	wantErr := errors.New("step 2 failed")
	var steps []string
	var calls atomic.Int64
	fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
		steps = append(steps, p.Checkpoint.Name)
		switch p.Checkpoint.Name {
		case "started":
			return &idempotent.Result{
				Checkpoint: &idempotent.Checkpoint{Name: "step1", Data: []byte(`{"n": 1}`)},
			}, nil
		case "step1":
			return &idempotent.Result{
				Checkpoint: &idempotent.Checkpoint{Name: "step2", Data: []byte(`{"n": 2}`)},
			}, nil
		case "step2":
			if calls.Add(1) == 1 {
				return nil, wantErr
			}
			return completed(string(p.Checkpoint.Data)), nil
		default:
			return nil, errors.New("unexpected checkpoint")
		}
	}

	idp := newIdempotent(t)
	_, err := idp.Do(t.Context(), keyOf(t), fn, req)
	is := assert.New(t)
	is.ErrorIs(err, wantErr)

	// The retry resumes from the saved checkpoint.
	res, err := idp.Do(t.Context(), keyOf(t), fn, req)
	is.NoError(err)
	is.JSONEq(`{"n": 2}`, string(res.Data))
	is.Equal([]string{"started", "step1", "step2", "step2"}, steps)

	// Each checkpoint appends the one it replaced to the logs.
	var logs string
	err = dbtest.DB(t).QueryRowContext(t.Context(),
		`select checkpoint_logs from dbtx.idempotency_keys where idempotency_key = $1`,
		keyOf(t)).Scan(&logs)
	is.NoError(err)
	is.JSONEq(`[
		{"name": "started", "data": null},
		{"name": "step1", "data": {"n": 1}}
	]`, logs)
}

func TestTakeover(t *testing.T) {
	repo := newRepository(t)
	key := keyOf(t)

	// A worker claims the key and crashes.
	stale, err := repo.Claim(t.Context(), idempotent.ClaimParams{
		IdempotencyKey: key,
		LeaseOwner:     "crashed",
		LeaseSeconds:   0.1,
		MaxAttempts:    idempotent.DefaultMaxAttempts,
		Request:        req.Data,
	})
	is := assert.New(t)
	is.NoError(err)
	is.Equal(idempotent.OutcomeClaimed, stale.Outcome)
	time.Sleep(200 * time.Millisecond)

	// Another worker takes over once the lease expires.
	res, err := idempotent.New(repo).Do(t.Context(), key, greet, req)
	is.NoError(err)
	is.Equal(string(idempotent.StatusCompleted), res.Status)

	// The crashed worker's fencing token is rejected.
	err = repo.Ack(t.Context(), idempotent.AckParams{
		IdempotencyKey: key,
		FencingToken:   stale.FencingToken,
		Response:       []byte(`{"msg": "stale"}`),
	})
	is.ErrorIs(err, idempotent.ErrClaimed)
}

func TestLongRunningStep(t *testing.T) {
	// fn runs longer than the lease. The step transaction holds the row lock
	// until it commits, so a concurrent Claim reports the key as in progress
	// instead of taking it over, or waiting for the step.
	var calls atomic.Int64
	inFn := make(chan struct{})
	fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
		if calls.Add(1) == 1 {
			close(inFn)
		}
		time.Sleep(time.Second)
		return greet(ctx, p)
	}
	idp := newIdempotent(t)
	idp.Lease = 200 * time.Millisecond
	key := keyOf(t)

	var first *idempotent.Response
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		first, firstErr = idp.Do(t.Context(), key, fn, req)
	}()
	<-inFn
	time.Sleep(500 * time.Millisecond) // Let the lease expire.

	start := time.Now()
	_, err := idp.Do(t.Context(), key, fn, req)
	is := assert.New(t)
	is.ErrorIs(err, idempotent.ErrRequestInFlight)
	is.Less(time.Since(start), 250*time.Millisecond, "Claim waited for the step")
	<-done

	is.NoError(firstErr)
	is.Equal(string(idempotent.StatusCompleted), first.Status)
	res, err := idp.Do(t.Context(), key, fn, req)
	is.NoError(err)
	is.Equal(string(idempotent.StatusCompleted), res.Status)
	is.Equal(int64(1), calls.Load())
}

func TestPurge(t *testing.T) {
	db := dbtest.DB(t)
	idp := idempotent.New(repository.New(db))
	key := keyOf(t)
	_, err := idp.Do(t.Context(), key, greet, req)
	is := assert.New(t)
	is.NoError(err)

	_, err = db.ExecContext(t.Context(), `
		update dbtx.idempotency_keys
		   set expires_at = now() - interval '1 second'
		 where idempotency_key = $1`, key)
	is.NoError(err)

	n, err := repository.New(db).Purge(t.Context())
	is.NoError(err)
	is.GreaterOrEqual(n, int64(1))

	// The key can be reused after it is purged.
	var calls atomic.Int64
	_, err = idp.Do(t.Context(), key, func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
		calls.Add(1)
		return greet(ctx, p)
	}, req)
	is.NoError(err)
	is.Equal(int64(1), calls.Load())
}

func TestFencingTokenAfterPurge(t *testing.T) {
	db := dbtest.DB(t)
	repo := repository.New(db)
	key := keyOf(t)
	claim := func(owner string) *idempotent.ClaimResponse {
		t.Helper()
		res, err := repo.Claim(t.Context(), idempotent.ClaimParams{
			IdempotencyKey: key,
			LeaseOwner:     owner,
			LeaseSeconds:   0.1,
			MaxAttempts:    idempotent.DefaultMaxAttempts,
			Request:        req.Data,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Worker A claims the key and stalls past its lease and the key's TTL.
	stale := claim("stalled")
	is := assert.New(t)
	is.Equal(idempotent.OutcomeClaimed, stale.Outcome)
	time.Sleep(200 * time.Millisecond)
	_, err := db.ExecContext(t.Context(), `
		update dbtx.idempotency_keys
		   set expires_at = now() - interval '1 second'
		 where idempotency_key = $1`, key)
	is.NoError(err)
	n, err := repo.Purge(t.Context())
	is.NoError(err)
	is.GreaterOrEqual(n, int64(1))

	// Worker B claims the purged key afresh.
	fresh := claim("fresh")
	is.Equal(idempotent.OutcomeClaimed, fresh.Outcome)
	is.Greater(fresh.FencingToken, stale.FencingToken)

	// A's token does not match B's row.
	err = repo.Ack(t.Context(), idempotent.AckParams{
		IdempotencyKey: key,
		FencingToken:   stale.FencingToken,
		Response:       []byte(`{"msg": "stale"}`),
	})
	is.ErrorIs(err, idempotent.ErrClaimed)

	// A takeover still reports a resumed claim, with a new token.
	time.Sleep(200 * time.Millisecond)
	takeover := claim("takeover")
	is.Equal(idempotent.OutcomeResumed, takeover.Outcome)
	is.Greater(takeover.FencingToken, fresh.FencingToken)
}

func TestLongRunningStepFails(t *testing.T) {
	// fn runs longer than the lease and then fails. The failure is recorded
	// before the row lock is released, so a Claim after the step sees the
	// backoff instead of taking over.
	wantErr := errors.New("boom")
	var calls atomic.Int64
	inFn := make(chan struct{})
	fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
		if calls.Add(1) == 1 {
			close(inFn)
			time.Sleep(time.Second)
			return nil, wantErr
		}
		return greet(ctx, p)
	}
	idp := newIdempotent(t)
	idp.Lease = 200 * time.Millisecond
	idp.BaseBackoff = time.Hour
	idp.MaxBackoff = time.Hour
	key := keyOf(t)

	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, firstErr = idp.Do(t.Context(), key, fn, req)
	}()
	<-inFn
	time.Sleep(500 * time.Millisecond) // Let the lease expire.

	_, err := idp.Do(t.Context(), key, fn, req)
	is := assert.New(t)
	is.ErrorIs(err, idempotent.ErrRequestInFlight)
	<-done

	is.ErrorIs(firstErr, wantErr)
	_, err = idp.Do(t.Context(), key, fn, req)
	is.ErrorIs(err, idempotent.ErrBackoff)
	is.Equal(int64(1), calls.Load())

	var status, errText string
	err = dbtest.DB(t).QueryRowContext(t.Context(),
		`select status, error from dbtx.idempotency_keys where idempotency_key = $1`,
		key).Scan(&status, &errText)
	is.NoError(err)
	is.Equal(string(idempotent.StatusRetryable), status)
	is.Equal(wantErr.Error(), errText)
}
