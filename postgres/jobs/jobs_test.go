package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"uuid"
)

// fakeRepo is a tiny in-memory Repository with fencing semantics.
type fakeRepo struct {
	mu       sync.Mutex
	jobs     map[uuid.UUID]*Job
	keys     map[string]CreateResult
	payloads map[string]string
	hbErr    error // injected heartbeat error
	hbHang   bool  // heartbeats block until their ctx is done

	// claimDelay delays FindAvailableJobs after it claims, like a driver
	// waiting for the reply of a committed statement.
	claimDelay time.Duration
	calls      map[string]int
}

func newFake() *fakeRepo {
	return &fakeRepo{jobs: map[uuid.UUID]*Job{}, keys: map[string]CreateResult{}, payloads: map[string]string{}, calls: map[string]int{}}
}

func (f *fakeRepo) Create(_ context.Context, p CreateParams) (CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.keys[p.IdempotencyKey]; ok {
		r.Created = false
		r.PayloadMatches = f.payloads[p.IdempotencyKey] == string(p.Request)
		return r, nil
	}
	j := &Job{ID: uuid.New(), Status: StatusQueued, Request: p.Request, MaxAttempts: p.MaxAttempts}
	f.jobs[j.ID] = j
	r := CreateResult{ID: j.ID, Status: StatusQueued, PayloadMatches: true, Created: true}
	f.keys[p.IdempotencyKey] = r
	f.payloads[p.IdempotencyKey] = string(p.Request)
	return r, nil
}

func (f *fakeRepo) FindAvailableJobs(ctx context.Context, p FindAvailableJobsParams) ([]Job, error) {
	out := f.claim(p)
	if f.claimDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.claimDelay):
		}
	}
	return out, nil
}

func (f *fakeRepo) claim(p FindAvailableJobsParams) []Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Job
	for _, j := range f.jobs {
		if len(out) >= int(p.Limit) {
			break
		}
		if j.Status == StatusQueued || j.Status == StatusError {
			j.Status, j.FencingToken, j.Attempts = StatusRunning, j.FencingToken+1, j.Attempts+1
			w := p.WorkerID
			j.WorkerID = &w
			out = append(out, *j)
		}
	}
	return out
}

func (f *fakeRepo) guard(id uuid.UUID, tok int64, worker string) (*Job, error) {
	j := f.jobs[id]
	if j == nil || j.Status != StatusRunning || j.FencingToken != tok || j.WorkerID == nil || *j.WorkerID != worker {
		return nil, ErrNoRows
	}
	return j, nil
}

func (f *fakeRepo) Heartbeat(ctx context.Context, p HeartbeatParams) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["heartbeat"]++
	if f.hbHang {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return Job{}, ctx.Err()
	}
	if f.hbErr != nil {
		return Job{}, f.hbErr
	}
	j, err := f.guard(p.ID, p.FencingToken, p.WorkerID)
	if err != nil {
		return Job{}, err
	}
	return *j, nil
}

func (f *fakeRepo) Complete(_ context.Context, p CompleteParams) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.guard(p.ID, p.FencingToken, p.WorkerID)
	if err != nil {
		return Job{}, err
	}
	j.Status, j.Response, j.WorkerID = StatusSuccess, p.Response, nil
	return *j, nil
}

func (f *fakeRepo) Fail(_ context.Context, p FailParams) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.guard(p.ID, p.FencingToken, p.WorkerID)
	if err != nil {
		return Job{}, err
	}
	j.Status, j.WorkerID = StatusFailed, nil
	return *j, nil
}

func (f *fakeRepo) Retry(_ context.Context, p RetryParams) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.guard(p.ID, p.FencingToken, p.WorkerID)
	if err != nil {
		return Job{}, err
	}
	j.Status, j.WorkerID = StatusError, nil
	if j.Attempts >= j.MaxAttempts {
		j.Status = StatusFailed
	}
	return *j, nil
}

func (f *fakeRepo) IsTx(context.Context) bool { return false }
func (f *fakeRepo) ReapExhausted(context.Context, ReapExhaustedParams) (int64, error) {
	return 0, nil
}
func (f *fakeRepo) ArchiveOldJobs(context.Context, ArchiveOldJobsParams) (int64, error) {
	return 0, nil
}
func (f *fakeRepo) PurgeOldJobKeys(context.Context, PurgeOldJobKeysParams) (int64, error) {
	return 0, nil
}

func (f *fakeRepo) status(id uuid.UUID) Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs[id].Status
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func runWorker(t *testing.T, repo Repository, h Handler, cfg WorkerConfig) (stop func()) {
	t.Helper()
	cfg.PollInterval = 10 * time.Millisecond
	w := NewWorker(repo, h, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func TestSubmitIdempotency(t *testing.T) {
	repo := newFake()
	s := NewSubmitter(repo, SubmitConfig{})
	ctx := context.Background()

	a, err := s.Submit(ctx, SubmitInput{IdempotencyKey: "k", Request: json.RawMessage(`{"a":1}`)})
	if err != nil || !a.Created {
		t.Fatalf("first submit: %+v %v", a, err)
	}
	b, err := s.Submit(ctx, SubmitInput{IdempotencyKey: "k", Request: json.RawMessage(`{"a":1}`)})
	if err != nil || b.Created || b.JobID != a.JobID {
		t.Fatalf("replay: %+v %v", b, err)
	}
	if _, err := s.Submit(ctx, SubmitInput{IdempotencyKey: "k", Request: json.RawMessage(`{"a":2}`)}); !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Fatalf("want key reuse error, got %v", err)
	}
	if _, err := s.Submit(ctx, SubmitInput{IdempotencyKey: ""}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want invalid input, got %v", err)
	}
	if _, err := s.Submit(ctx, SubmitInput{IdempotencyKey: "x", Request: json.RawMessage(`{oops`)}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want invalid json error, got %v", err)
	}
}

func TestWorkerCompletes(t *testing.T) {
	repo := newFake()
	r, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 3})
	stop := runWorker(t, repo, func(ctx context.Context, j Job) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}, WorkerConfig{})
	defer stop()
	waitFor(t, func() bool { return repo.status(r.ID) == StatusSuccess })
}

func TestWorkerPermanentAndRetryAndPanic(t *testing.T) {
	repo := newFake()
	perm, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "p", Request: json.RawMessage(`{}`), MaxAttempts: 3})
	pan, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "x", Request: json.RawMessage(`{}`), MaxAttempts: 1})
	stop := runWorker(t, repo, func(ctx context.Context, j Job) (json.RawMessage, error) {
		if j.ID == perm.ID {
			return nil, Permanent(errors.New("bad input"))
		}
		panic("boom")
	}, WorkerConfig{Backoff: func(int32) time.Duration { return 0 }})
	defer stop()
	waitFor(t, func() bool { return repo.status(perm.ID) == StatusFailed })
	// panic on the only attempt => retry becomes terminal failure
	waitFor(t, func() bool { return repo.status(pan.ID) == StatusFailed })
}

func TestWorkerLeaseLostCancelsHandler(t *testing.T) {
	repo := newFake()
	r, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 3})
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	stop := runWorker(t, repo, func(ctx context.Context, j Job) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		cancelled <- context.Cause(ctx)
		return nil, ctx.Err()
	}, WorkerConfig{LeaseDuration: 150 * time.Millisecond})
	defer stop()

	<-started
	// Simulate another worker reclaiming the job: token changes under us.
	repo.mu.Lock()
	repo.jobs[r.ID].FencingToken++
	repo.mu.Unlock()

	select {
	case c := <-cancelled:
		if !errors.Is(c, ErrLeaseLost) {
			t.Fatalf("cause = %v, want ErrLeaseLost", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not cancelled after lease loss")
	}
	if repo.status(r.ID) != StatusRunning { // untouched: stale worker must not write
		t.Fatalf("stale worker modified job: %s", repo.status(r.ID))
	}
}

func TestWorkerHeartbeatOutageStopsHandlerButStillFinalizes(t *testing.T) {
	repo := newFake()
	repo.hbErr = errors.New("db down")
	// One attempt only, so the Retry that finalizes the job becomes terminal.
	r, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 1})
	stop := runWorker(t, repo, func(ctx context.Context, j Job) (json.RawMessage, error) {
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), errLeaseExpiredLocally) {
			t.Errorf("cause = %v", context.Cause(ctx))
		}
		return nil, ctx.Err()
	}, WorkerConfig{LeaseDuration: 120 * time.Millisecond, Backoff: func(int32) time.Duration { return 0 }})
	defer stop()
	// Heartbeats failed, but the token is unchanged, so finalizing still works.
	waitFor(t, func() bool { return repo.status(r.ID) == StatusFailed })
}

func TestGracefulShutdownWaitsForInflight(t *testing.T) {
	repo := newFake()
	r, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 3})
	started := make(chan struct{})
	w := NewWorker(repo, func(ctx context.Context, j Job) (json.RawMessage, error) {
		close(started)
		time.Sleep(200 * time.Millisecond) // ignores ctx on purpose; finishes within grace
		return nil, nil
	}, WorkerConfig{PollInterval: 10 * time.Millisecond, ShutdownGrace: 2 * time.Second}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	<-started
	cancel() // stop claiming; in-flight job must still complete
	<-done
	if repo.status(r.ID) != StatusSuccess {
		t.Fatalf("job not completed during graceful shutdown: %s", repo.status(r.ID))
	}
}

func TestJanitorValidatesTTL(t *testing.T) {
	if _, err := NewJanitor(newFake(), JanitorConfig{Retention: time.Hour, KeyTTL: time.Hour}, nil); err == nil {
		t.Fatal("expected KeyTTL <= Retention to be rejected")
	}
}

func TestWorkerHangingHeartbeatStopsHandlerBeforeLeaseExpires(t *testing.T) {
	repo := newFake()
	repo.hbHang = true
	_, _ = repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 3})
	const lease = 300 * time.Millisecond

	type result struct {
		elapsed time.Duration
		cause   error
	}
	stopped := make(chan result, 1)
	stop := runWorker(t, repo, func(ctx context.Context, j Job) (json.RawMessage, error) {
		start := time.Now()
		<-ctx.Done()
		stopped <- result{time.Since(start), context.Cause(ctx)}
		return nil, ctx.Err()
	}, WorkerConfig{LeaseDuration: lease})
	defer stop()

	select {
	case r := <-stopped:
		if !errors.Is(r.cause, errLeaseExpiredLocally) {
			t.Fatalf("cause = %v, want errLeaseExpiredLocally", r.cause)
		}
		// Another worker may claim the job once the lease expires.
		if r.elapsed >= lease {
			t.Fatalf("handler stopped after %s, want before the %s lease", r.elapsed, lease)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not stopped")
	}
}

func TestWorkerHeartbeatsKeepHandlerRunning(t *testing.T) {
	repo := newFake()
	r, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 3})
	stop := runWorker(t, repo, func(ctx context.Context, j Job) (json.RawMessage, error) {
		// Several leases long.
		select {
		case <-ctx.Done():
			t.Errorf("handler stopped: %v", context.Cause(ctx))
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return nil, nil
		}
	}, WorkerConfig{LeaseDuration: 150 * time.Millisecond})
	defer stop()
	waitFor(t, func() bool { return repo.status(r.ID) == StatusSuccess })
}

func TestWorkerShutdownDuringClaim(t *testing.T) {
	repo := newFake()
	repo.claimDelay = 200 * time.Millisecond
	r, _ := repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 1})

	var calls atomic.Int64
	w := NewWorker(repo, func(context.Context, Job) (json.RawMessage, error) {
		calls.Add(1)
		return nil, nil
	}, WorkerConfig{PollInterval: 10 * time.Millisecond}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Shut down after the claim committed, while its reply is in flight.
	waitFor(t, func() bool { return repo.status(r.ID) == StatusRunning })
	cancel()
	<-done

	// The claimed job ran instead of being left to expire on its only attempt.
	if s := repo.status(r.ID); s != StatusSuccess {
		t.Fatalf("status = %s, want success", s)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
}

func TestWorkerShutdownTimeout(t *testing.T) {
	repo := newFake()
	_, _ = repo.Create(context.Background(), CreateParams{IdempotencyKey: "k", Request: json.RawMessage(`{}`), MaxAttempts: 3})
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	w := NewWorker(repo, func(context.Context, Job) (json.RawMessage, error) {
		close(started)
		<-release // ignores ctx
		return nil, nil
	}, WorkerConfig{
		PollInterval:    10 * time.Millisecond,
		ShutdownGrace:   50 * time.Millisecond,
		FinalizeTimeout: 100 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- w.Run(ctx) }()
	<-started
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrShutdownTimeout) {
			t.Fatalf("Run() = %v, want ErrShutdownTimeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
}
