package repository_test

import (
	_ "github.com/lib/pq"

	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/jobs"
	"github.com/alextanhongpin/dbtx/postgres/jobs/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// The claim query filters on NOW(), which is fixed for the duration of a
// transaction, so the tests run against the pool instead of dbtest.Tx and
// truncate the tables instead.
func newRepository(t *testing.T) (*repository.Repository, *sql.DB) {
	t.Helper()
	db := dbtest.DB(t)
	_, err := db.ExecContext(t.Context(), `truncate dbtx.jobs, dbtx.jobs_archive, dbtx.job_keys`)
	require.NoError(t, err)
	return repository.New(db), db
}

func create(t *testing.T, repo *repository.Repository, key string, maxAttempts int32) uuid.UUID {
	t.Helper()
	res, err := repo.Create(t.Context(), jobs.CreateParams{
		IdempotencyKey: key,
		Request:        json.RawMessage(`{"key": "` + key + `"}`),
		MaxAttempts:    maxAttempts,
	})
	require.NoError(t, err)
	require.True(t, res.Created)
	return res.ID
}

func claim(t *testing.T, repo *repository.Repository, workerID string, limit int32) []jobs.Job {
	t.Helper()
	res, err := repo.FindAvailableJobs(t.Context(), jobs.FindAvailableJobsParams{
		WorkerID:     workerID,
		LeaseSeconds: 60,
		Limit:        limit,
	})
	require.NoError(t, err)
	return res
}

func claimOne(t *testing.T, repo *repository.Repository, workerID string) jobs.Job {
	t.Helper()
	res := claim(t, repo, workerID, 1)
	require.Len(t, res, 1)
	return res[0]
}

// makeVisible ends a lease or backoff now, as if its time had passed.
func makeVisible(t *testing.T, db *sql.DB, id uuid.UUID) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`update dbtx.jobs set visible_at = now() - interval '1 second' where id = $1`, id)
	require.NoError(t, err)
}

func status(t *testing.T, db *sql.DB, table string, id uuid.UUID) (jobs.Status, error) {
	t.Helper()
	var s jobs.Status
	err := db.QueryRowContext(t.Context(),
		`select status from dbtx.`+table+` where id = $1`, id).Scan(&s)
	return s, err
}

func TestCreate(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		repo, _ := newRepository(t)
		res, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{"a": 1}`),
			MaxAttempts:    3,
		})
		is := assert.New(t)
		is.NoError(err)
		is.True(res.Created)
		is.True(res.PayloadMatches)
		is.Equal(jobs.StatusQueued, res.Status)
		is.Nil(res.Error)
		is.NotEqual(uuid.Nil(), res.ID)
	})

	t.Run("replay", func(t *testing.T) {
		repo, _ := newRepository(t)
		id := create(t, repo, "k", 3)

		res, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{"key": "k"}`),
			MaxAttempts:    3,
		})
		is := assert.New(t)
		is.NoError(err)
		is.False(res.Created)
		is.True(res.PayloadMatches)
		is.Equal(id, res.ID)
	})

	t.Run("key order and whitespace do not matter", func(t *testing.T) {
		repo, _ := newRepository(t)
		_, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{"a": 1, "b": 2}`),
			MaxAttempts:    3,
		})
		require.NoError(t, err)

		res, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{"b":2,"a":1}`),
			MaxAttempts:    3,
		})
		is := assert.New(t)
		is.NoError(err)
		is.False(res.Created)
		is.True(res.PayloadMatches)
	})

	t.Run("payload mismatch", func(t *testing.T) {
		repo, _ := newRepository(t)
		id := create(t, repo, "k", 3)

		res, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{"other": true}`),
			MaxAttempts:    3,
		})
		is := assert.New(t)
		is.NoError(err)
		is.False(res.Created)
		is.False(res.PayloadMatches)
		is.Equal(id, res.ID)
	})

	t.Run("delay", func(t *testing.T) {
		repo, db := newRepository(t)
		res, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{}`),
			DelaySeconds:   60,
			MaxAttempts:    3,
		})
		require.NoError(t, err)
		assert.Empty(t, claim(t, repo, "w1", 10))

		makeVisible(t, db, res.ID)
		assert.Len(t, claim(t, repo, "w1", 10), 1)
	})

	t.Run("concurrent", func(t *testing.T) {
		repo, _ := newRepository(t)
		n := 10
		results := make(chan jobs.CreateResult, n)
		errs := make(chan error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				<-start
				res, err := repo.Create(t.Context(), jobs.CreateParams{
					IdempotencyKey: "k",
					Request:        json.RawMessage(`{}`),
					MaxAttempts:    3,
				})
				if err != nil {
					errs <- err
					return
				}
				results <- res
			})
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		is := assert.New(t)
		for err := range errs {
			is.NoError(err)
		}
		var created int
		ids := map[uuid.UUID]bool{}
		for res := range results {
			if res.Created {
				created++
			}
			ids[res.ID] = true
		}
		is.Equal(1, created)
		is.Len(ids, 1)
	})
}

func TestFindAvailableJobs(t *testing.T) {
	t.Run("claims", func(t *testing.T) {
		repo, _ := newRepository(t)
		id := create(t, repo, "k", 3)

		job := claimOne(t, repo, "w1")
		is := assert.New(t)
		is.Equal(id, job.ID)
		is.Equal("k", job.IdempotencyKey)
		is.JSONEq(`{"key": "k"}`, string(job.Request))
		is.Equal(jobs.StatusRunning, job.Status)
		is.Equal("w1", *job.WorkerID)
		is.Equal(int64(1), job.FencingToken)
		is.Equal(int32(1), job.Attempts)
		is.Equal(int32(3), job.MaxAttempts)
		is.Nil(job.Error)
		is.Nil(job.ProcessedAt)
		is.WithinDuration(time.Now().Add(time.Minute), job.VisibleAt, 5*time.Second)
	})

	t.Run("empty", func(t *testing.T) {
		repo, _ := newRepository(t)
		assert.Empty(t, claim(t, repo, "w1", 10))
	})

	t.Run("limit and order", func(t *testing.T) {
		repo, _ := newRepository(t)
		a := create(t, repo, "a", 3)
		b := create(t, repo, "b", 3)
		c := create(t, repo, "c", 3)

		got := claim(t, repo, "w1", 2)
		is := assert.New(t)
		is.Len(got, 2)
		is.ElementsMatch([]uuid.UUID{a, b}, []uuid.UUID{got[0].ID, got[1].ID})
		is.Equal(c, claimOne(t, repo, "w1").ID)
	})

	t.Run("leased job is not claimed again", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 3)
		claimOne(t, repo, "w1")

		assert.Empty(t, claim(t, repo, "w2", 10))
	})

	t.Run("expired lease is reclaimed with a new token", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 3)
		first := claimOne(t, repo, "w1")
		makeVisible(t, db, first.ID)

		second := claimOne(t, repo, "w2")
		is := assert.New(t)
		is.Equal(first.ID, second.ID)
		is.Equal("w2", *second.WorkerID)
		is.Equal(first.FencingToken+1, second.FencingToken)
		is.Equal(int32(2), second.Attempts)
	})

	t.Run("exhausted job is not claimed", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 1)
		job := claimOne(t, repo, "w1")
		makeVisible(t, db, job.ID)

		assert.Empty(t, claim(t, repo, "w2", 10))
	})

	t.Run("concurrent", func(t *testing.T) {
		repo, _ := newRepository(t)
		n := 20
		for i := range n {
			create(t, repo, fmt.Sprintf("k%d", i), 3)
		}

		var mu sync.Mutex
		seen := map[uuid.UUID]int{}
		errs := make(chan error, 5)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for w := range 5 {
			wg.Go(func() {
				<-start
				for {
					got, err := repo.FindAvailableJobs(t.Context(), jobs.FindAvailableJobsParams{
						WorkerID:     fmt.Sprintf("w%d", w),
						LeaseSeconds: 60,
						Limit:        3,
					})
					if err != nil {
						errs <- err
						return
					}
					if len(got) == 0 {
						return
					}
					mu.Lock()
					for _, j := range got {
						seen[j.ID]++
					}
					mu.Unlock()
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
		is.Len(seen, n)
		for id, count := range seen {
			is.Equal(1, count, "job %s claimed %d times", id, count)
		}
	})
}

func TestHeartbeat(t *testing.T) {
	t.Run("extends the lease", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")

		res, err := repo.Heartbeat(t.Context(), jobs.HeartbeatParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			LeaseSeconds: 3600,
		})
		is := assert.New(t)
		is.NoError(err)
		is.Equal(jobs.StatusRunning, res.Status)
		is.WithinDuration(time.Now().Add(time.Hour), res.VisibleAt, 5*time.Second)
	})

	t.Run("expired but not reclaimed", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")
		makeVisible(t, db, job.ID)

		_, err := repo.Heartbeat(t.Context(), jobs.HeartbeatParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			LeaseSeconds: 60,
		})
		assert.NoError(t, err)
	})

	t.Run("lease lost", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")
		makeVisible(t, db, job.ID)
		claimOne(t, repo, "w2")

		_, err := repo.Heartbeat(t.Context(), jobs.HeartbeatParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			LeaseSeconds: 60,
		})
		assert.ErrorIs(t, err, jobs.ErrNoRows)
	})

	t.Run("wrong worker", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")

		_, err := repo.Heartbeat(t.Context(), jobs.HeartbeatParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w2",
			LeaseSeconds: 60,
		})
		assert.ErrorIs(t, err, jobs.ErrNoRows)
	})
}

func TestComplete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")

		res, err := repo.Complete(t.Context(), jobs.CompleteParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			Response:     json.RawMessage(`{"ok": true}`),
		})
		is := assert.New(t)
		is.NoError(err)
		is.Equal(jobs.StatusSuccess, res.Status)
		is.JSONEq(`{"ok": true}`, string(res.Response))
		is.Nil(res.WorkerID)
		is.NotNil(res.ProcessedAt)

		// The replay returns the response.
		replay, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{"key": "k"}`),
			MaxAttempts:    3,
		})
		is.NoError(err)
		is.Equal(jobs.StatusSuccess, replay.Status)
		is.JSONEq(`{"ok": true}`, string(replay.Response))
	})

	t.Run("clears the previous error", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")
		_, err := repo.Retry(t.Context(), jobs.RetryParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			Error:        "boom",
		})
		require.NoError(t, err)
		makeVisible(t, db, job.ID)
		job = claimOne(t, repo, "w1")
		require.Equal(t, "boom", *job.Error)

		res, err := repo.Complete(t.Context(), jobs.CompleteParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			Response:     json.RawMessage(`{}`),
		})
		is := assert.New(t)
		is.NoError(err)
		is.Nil(res.Error)
	})

	t.Run("lease lost", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")
		makeVisible(t, db, job.ID)
		claimOne(t, repo, "w2")

		_, err := repo.Complete(t.Context(), jobs.CompleteParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			Response:     json.RawMessage(`{}`),
		})
		is := assert.New(t)
		is.ErrorIs(err, jobs.ErrNoRows)
		s, err := status(t, db, "jobs", job.ID)
		is.NoError(err)
		is.Equal(jobs.StatusRunning, s)
	})

	t.Run("already finished", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")
		params := jobs.CompleteParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			Response:     json.RawMessage(`{}`),
		}
		_, err := repo.Complete(t.Context(), params)
		require.NoError(t, err)

		_, err = repo.Complete(t.Context(), params)
		assert.ErrorIs(t, err, jobs.ErrNoRows)
	})
}

func TestFail(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")

		res, err := repo.Fail(t.Context(), jobs.FailParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			Error:        "bad input",
		})
		is := assert.New(t)
		is.NoError(err)
		is.Equal(jobs.StatusFailed, res.Status)
		is.Equal("bad input", *res.Error)
		is.Nil(res.WorkerID)
		is.NotNil(res.ProcessedAt)

		// A failed job is not retried, even with attempts left.
		makeVisible(t, db, job.ID)
		is.Empty(claim(t, repo, "w1", 10))
	})

	t.Run("lease lost", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")

		_, err := repo.Fail(t.Context(), jobs.FailParams{
			ID:           job.ID,
			FencingToken: job.FencingToken + 1,
			WorkerID:     "w1",
			Error:        "bad input",
		})
		assert.ErrorIs(t, err, jobs.ErrNoRows)
	})
}

func TestRetry(t *testing.T) {
	t.Run("backoff", func(t *testing.T) {
		repo, db := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")

		res, err := repo.Retry(t.Context(), jobs.RetryParams{
			ID:             job.ID,
			FencingToken:   job.FencingToken,
			WorkerID:       "w1",
			Error:          "boom",
			BackoffSeconds: 60,
		})
		is := assert.New(t)
		is.NoError(err)
		is.Equal(jobs.StatusError, res.Status)
		is.Equal("boom", *res.Error)
		is.Nil(res.WorkerID)
		is.Nil(res.ProcessedAt)
		is.WithinDuration(time.Now().Add(time.Minute), res.VisibleAt, 5*time.Second)

		// Hidden until the backoff ends.
		is.Empty(claim(t, repo, "w1", 10))
		makeVisible(t, db, job.ID)
		next := claimOne(t, repo, "w1")
		is.Equal(int32(2), next.Attempts)
		is.Equal("boom", *next.Error)
	})

	t.Run("last attempt fails the job", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 1)
		job := claimOne(t, repo, "w1")

		res, err := repo.Retry(t.Context(), jobs.RetryParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w1",
			Error:        "boom",
		})
		is := assert.New(t)
		is.NoError(err)
		is.Equal(jobs.StatusFailed, res.Status)
		is.Equal("boom", *res.Error)
		is.NotNil(res.ProcessedAt)
	})

	t.Run("lease lost", func(t *testing.T) {
		repo, _ := newRepository(t)
		create(t, repo, "k", 3)
		job := claimOne(t, repo, "w1")

		_, err := repo.Retry(t.Context(), jobs.RetryParams{
			ID:           job.ID,
			FencingToken: job.FencingToken,
			WorkerID:     "w2",
			Error:        "boom",
		})
		assert.ErrorIs(t, err, jobs.ErrNoRows)
	})
}

func TestReapExhausted(t *testing.T) {
	repo, db := newRepository(t)
	exhausted := create(t, repo, "exhausted", 1)
	claimOne(t, repo, "w1")
	leased := create(t, repo, "leased", 1)
	claimOne(t, repo, "w1")
	queued := create(t, repo, "queued", 1)

	// Only the exhausted job's lease has expired.
	makeVisible(t, db, exhausted)

	n, err := repo.ReapExhausted(t.Context(), jobs.ReapExhaustedParams{})
	is := assert.New(t)
	is.NoError(err)
	is.Equal(int64(1), n)

	s, err := status(t, db, "jobs", exhausted)
	is.NoError(err)
	is.Equal(jobs.StatusFailed, s)

	var errText string
	err = db.QueryRowContext(t.Context(), `select error from dbtx.jobs where id = $1`, exhausted).Scan(&errText)
	is.NoError(err)
	is.Equal("lease expired; max attempts exceeded", errText)

	s, err = status(t, db, "jobs", leased)
	is.NoError(err)
	is.Equal(jobs.StatusRunning, s)
	s, err = status(t, db, "jobs", queued)
	is.NoError(err)
	is.Equal(jobs.StatusQueued, s)

	n, err = repo.ReapExhausted(t.Context(), jobs.ReapExhaustedParams{})
	is.NoError(err)
	is.Zero(n)
}

func finish(t *testing.T, repo *repository.Repository, key string) uuid.UUID {
	t.Helper()
	create(t, repo, key, 3)
	job := claimOne(t, repo, "w1")
	_, err := repo.Complete(t.Context(), jobs.CompleteParams{
		ID:           job.ID,
		FencingToken: job.FencingToken,
		WorkerID:     "w1",
		Response:     json.RawMessage(`{"key": "` + key + `"}`),
	})
	require.NoError(t, err)
	return job.ID
}

func TestArchiveOldJobs(t *testing.T) {
	t.Run("moves finished jobs", func(t *testing.T) {
		repo, db := newRepository(t)
		done := finish(t, repo, "done")
		running := create(t, repo, "running", 3)
		claimOne(t, repo, "w1")
		queued := create(t, repo, "queued", 3)

		n, err := repo.ArchiveOldJobs(t.Context(), jobs.ArchiveOldJobsParams{BatchSize: 10})
		is := assert.New(t)
		is.NoError(err)
		is.Equal(int64(1), n)

		_, err = status(t, db, "jobs", done)
		is.ErrorIs(err, sql.ErrNoRows)
		s, err := status(t, db, "jobs_archive", done)
		is.NoError(err)
		is.Equal(jobs.StatusSuccess, s)

		for _, id := range []uuid.UUID{running, queued} {
			_, err := status(t, db, "jobs", id)
			is.NoError(err)
		}

		// The key still resolves to the archived job.
		res, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "done",
			Request:        json.RawMessage(`{"key": "done"}`),
			MaxAttempts:    3,
		})
		is.NoError(err)
		is.False(res.Created)
		is.Equal(done, res.ID)
		is.Equal(jobs.StatusSuccess, res.Status)
		is.JSONEq(`{"key": "done"}`, string(res.Response))
	})

	t.Run("batches", func(t *testing.T) {
		repo, _ := newRepository(t)
		for i := range 3 {
			finish(t, repo, fmt.Sprintf("k%d", i))
		}

		is := assert.New(t)
		for _, want := range []int64{2, 1, 0} {
			n, err := repo.ArchiveOldJobs(t.Context(), jobs.ArchiveOldJobsParams{BatchSize: 2})
			is.NoError(err)
			is.Equal(want, n)
		}
	})

	t.Run("retention", func(t *testing.T) {
		repo, _ := newRepository(t)
		finish(t, repo, "k")

		n, err := repo.ArchiveOldJobs(t.Context(), jobs.ArchiveOldJobsParams{
			RetentionSeconds: 3600,
			BatchSize:        10,
		})
		is := assert.New(t)
		is.NoError(err)
		is.Zero(n)
	})
}

func TestPurgeOldJobKeys(t *testing.T) {
	t.Run("keeps keys of live jobs", func(t *testing.T) {
		repo, _ := newRepository(t)
		finish(t, repo, "k")

		n, err := repo.PurgeOldJobKeys(t.Context(), jobs.PurgeOldJobKeysParams{BatchSize: 10})
		is := assert.New(t)
		is.NoError(err)
		is.Zero(n)
	})

	t.Run("purges keys of archived jobs", func(t *testing.T) {
		repo, _ := newRepository(t)
		old := finish(t, repo, "k")
		_, err := repo.ArchiveOldJobs(t.Context(), jobs.ArchiveOldJobsParams{BatchSize: 10})
		require.NoError(t, err)

		n, err := repo.PurgeOldJobKeys(t.Context(), jobs.PurgeOldJobKeysParams{BatchSize: 10})
		is := assert.New(t)
		is.NoError(err)
		is.Equal(int64(1), n)

		// The key creates a new job once it is purged.
		res, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: "k",
			Request:        json.RawMessage(`{"key": "k"}`),
			MaxAttempts:    3,
		})
		is.NoError(err)
		is.True(res.Created)
		is.NotEqual(old, res.ID)
	})

	t.Run("ttl", func(t *testing.T) {
		repo, _ := newRepository(t)
		finish(t, repo, "k")
		_, err := repo.ArchiveOldJobs(t.Context(), jobs.ArchiveOldJobsParams{BatchSize: 10})
		require.NoError(t, err)

		n, err := repo.PurgeOldJobKeys(t.Context(), jobs.PurgeOldJobKeysParams{
			KeyTTLSeconds: 3600,
			BatchSize:     10,
		})
		is := assert.New(t)
		is.NoError(err)
		is.Zero(n)
	})
}

func TestTxInContext(t *testing.T) {
	repo, _ := newRepository(t)
	handler := func(context.Context, jobs.Job) (json.RawMessage, error) {
		t.Error("handler called")
		return nil, nil
	}
	janitor, err := jobs.NewJanitor(repo, jobs.JanitorConfig{}, nil)
	require.NoError(t, err)

	err = repo.RunInTx(t.Context(), func(txCtx context.Context) error {
		is := assert.New(t)
		is.ErrorIs(jobs.NewWorker(repo, handler, jobs.WorkerConfig{}, nil).Run(txCtx), jobs.ErrTxInContext)
		is.ErrorIs(janitor.Run(txCtx), jobs.ErrTxInContext)

		// Submit joins the transaction, so the job is rolled back with it.
		_, err := jobs.NewSubmitter(repo, jobs.SubmitConfig{}).Submit(txCtx, jobs.SubmitInput{IdempotencyKey: "k"})
		is.NoError(err)
		return assert.AnError
	})
	assert.ErrorIs(t, err, assert.AnError)
	assert.Empty(t, claim(t, repo, "w1", 10))
}

// runJob submits a job, runs a worker until the job finishes, and returns the
// stored job and the number of times the handler ran.
func runJob(t *testing.T, handle func() (json.RawMessage, error)) (jobs.Job, int) {
	t.Helper()
	repo, db := newRepository(t)
	res, err := jobs.NewSubmitter(repo, jobs.SubmitConfig{}).Submit(t.Context(), jobs.SubmitInput{
		IdempotencyKey: "k",
		MaxAttempts:    3,
	})
	require.NoError(t, err)

	var mu sync.Mutex
	var calls int
	w := jobs.NewWorker(repo, func(context.Context, jobs.Job) (json.RawMessage, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return handle()
	}, jobs.WorkerConfig{
		PollInterval: 10 * time.Millisecond,
		Backoff:      func(int32) time.Duration { return 0 },
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := status(t, db, "jobs", res.JobID)
		require.NoError(t, err)
		if s.Terminal() || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	var job jobs.Job
	var errText sql.NullString
	err = db.QueryRowContext(t.Context(),
		`select status, response, error from dbtx.jobs where id = $1`, res.JobID).
		Scan(&job.Status, &job.Response, &errText)
	require.NoError(t, err)
	if errText.Valid {
		job.Error = &errText.String
	}
	mu.Lock()
	defer mu.Unlock()
	return job, calls
}

func TestWorkerStoresOutcome(t *testing.T) {
	t.Run("error text is made storable", func(t *testing.T) {
		long := strings.Repeat("a", 1999) + "é" + strings.Repeat("b", 10)
		errs := map[string]struct {
			err  error
			want string
		}{
			"truncated mid character": {errors.New(long), strings.Repeat("a", 1999) + "…"},
			"nul byte":                {errors.New("bad\x00byte"), "badbyte"},
			"invalid utf-8":           {errors.New("bad\xffbyte"), "bad�byte"},
		}
		for name, tc := range errs {
			t.Run(name, func(t *testing.T) {
				job, calls := runJob(t, func() (json.RawMessage, error) {
					return nil, jobs.Permanent(tc.err)
				})
				is := assert.New(t)
				is.Equal(jobs.StatusFailed, job.Status)
				is.Equal(1, calls)
				if is.NotNil(job.Error) {
					is.Equal(tc.want, *job.Error)
				}
			})
		}
	})

	t.Run("empty response is null", func(t *testing.T) {
		job, calls := runJob(t, func() (json.RawMessage, error) {
			return json.RawMessage{}, nil
		})
		is := assert.New(t)
		is.Equal(jobs.StatusSuccess, job.Status)
		is.Equal(1, calls)
		is.JSONEq(`null`, string(job.Response))
	})

	t.Run("invalid response fails the job", func(t *testing.T) {
		responses := map[string]string{
			"syntax":         `{oops`,
			"two values":     `{} {}`,
			"nul escape":     `{"a": "\u0000"}`,
			"lone surrogate": `"\ud800"`,
			"invalid utf-8":  "\"\xff\"",
			// Valid JSON that only the database rejects.
			"numeric overflow": `1e999999`,
		}
		for name, resp := range responses {
			t.Run(name, func(t *testing.T) {
				job, calls := runJob(t, func() (json.RawMessage, error) {
					return json.RawMessage(resp), nil
				})
				is := assert.New(t)
				is.Equal(jobs.StatusFailed, job.Status)
				is.Equal(1, calls)
				if is.NotNil(job.Error) {
					is.Contains(*job.Error, jobs.ErrInvalidResponse.Error())
				}
			})
		}
	})
}

func TestSubmitRejectsInvalidRequest(t *testing.T) {
	repo, _ := newRepository(t)
	s := jobs.NewSubmitter(repo, jobs.SubmitConfig{})
	for _, req := range []string{`{"a": "\u0000"}`, `"\ud800"`, "\"\xff\""} {
		_, err := s.Submit(t.Context(), jobs.SubmitInput{IdempotencyKey: "k", Request: json.RawMessage(req)})
		assert.ErrorIs(t, err, jobs.ErrInvalidInput, req)
	}
}

func TestInvalidMaxAttempts(t *testing.T) {
	repo, _ := newRepository(t)
	for _, n := range []int32{0, -1} {
		_, err := repo.Create(t.Context(), jobs.CreateParams{
			IdempotencyKey: fmt.Sprintf("k%d", n),
			Request:        json.RawMessage(`{}`),
			MaxAttempts:    n,
		})
		// -1 trips attempts <= max_attempts first.
		assert.ErrorContains(t, err, "violates check constraint", n)
	}

	_, err := jobs.NewSubmitter(repo, jobs.SubmitConfig{}).Submit(t.Context(), jobs.SubmitInput{
		IdempotencyKey: "k",
		MaxAttempts:    -1,
	})
	assert.ErrorIs(t, err, jobs.ErrInvalidInput)
}

func TestReapExhaustedGrace(t *testing.T) {
	repo, db := newRepository(t)
	id := create(t, repo, "k", 1)
	claimOne(t, repo, "w1")
	makeVisible(t, db, id) // The last lease expired a second ago.

	// The worker may still be recording its outcome.
	n, err := repo.ReapExhausted(t.Context(), jobs.ReapExhaustedParams{GraceSeconds: 60})
	is := assert.New(t)
	is.NoError(err)
	is.Zero(n)
	s, err := status(t, db, "jobs", id)
	is.NoError(err)
	is.Equal(jobs.StatusRunning, s)

	n, err = repo.ReapExhausted(t.Context(), jobs.ReapExhaustedParams{})
	is.NoError(err)
	is.Equal(int64(1), n)
}
