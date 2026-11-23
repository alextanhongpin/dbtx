// Package keys makes synchronous requests idempotent. Begin claims a
// (scope, key) pair; the first caller does the work and stores a response
// that later callers with the same request receive instead of redoing it.
//
// Every claim counts as an attempt. A failed attempt (Abort, or an error from
// the function given to Do) makes the key wait for Config.Backoff before the
// next attempt, and once Config.MaxAttempts are used up the key is exhausted:
// later callers get ErrExhausted instead of another attempt. A claim whose
// holder crashed counts too, so a request that keeps crashing its handler
// stops being retried.
//
// Requests with several steps that each commit on their own use DoSteps (or
// Claim.SaveCheckpoint): each step's writes commit with its checkpoint, and a
// retry resumes after the last finished step.
package keys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/engine"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

const keysTable = "durable.idempotency_keys"

// DefaultMaxAttempts is used when Config.MaxAttempts is zero.
const DefaultMaxAttempts = 10

var (
	// ErrBackoff matches a *BackoffError: an earlier attempt failed and the
	// key cannot be attempted again yet.
	ErrBackoff = errors.New("keys: retry backoff")
	// ErrExhausted means every attempt was used up; the key will not run
	// again until it expires.
	ErrExhausted = errors.New("keys: attempts exhausted")
	// ErrInvalidResult means a step returned neither or both of
	// Result.Checkpoint and Result.Response.
	ErrInvalidResult = errors.New("keys: result must set exactly one of Checkpoint or Response")
)

// BackoffError is returned by Do and DoSteps to callers that arrive before
// the backoff after a failed attempt has passed. It matches ErrBackoff.
type BackoffError struct {
	RetryAfter time.Time // the earliest time of the next attempt
	LastError  string    // why the previous attempt failed
}

func (e *BackoffError) Error() string {
	return fmt.Sprintf("keys: retry backoff until %s: %s", e.RetryAfter.Format(time.RFC3339Nano), e.LastError)
}

func (e *BackoffError) Is(target error) bool { return target == ErrBackoff }

// Config is copied at construction. Zero values select defaults.
type Config struct {
	DB             *dbtx.DB
	InFlightTTL    time.Duration // lease on an in-flight key, renewed while held; default 30s
	Retention      time.Duration // how long completed keys are replayed; default 24h
	RequestTimeout time.Duration // per database call; default 5s
	// MaxAttempts is how many claims a key gets before it is exhausted;
	// default DefaultMaxAttempts. Release gives an attempt back.
	MaxAttempts int
	// Backoff maps the number of attempts made to the wait after a failed
	// one, e.g. durable.ExponentialBackoff(100*time.Millisecond, time.Minute).
	// Nil means no wait. An error wrapped with durable.RetryAfter overrides it.
	Backoff  durable.Backoff
	Observer durable.Observer
	Hooks    *durable.Hooks
}

// Keys is safe for concurrent use.
type Keys struct{ cfg Config }

func New(cfg Config) (*Keys, error) {
	if cfg.DB == nil || cfg.DB.Unwrap() == nil {
		return nil, fmt.Errorf("%w: DB is required", durable.ErrInvalidConfig)
	}
	if cfg.InFlightTTL == 0 {
		cfg.InFlightTTL = 30 * time.Second
	}
	if cfg.Retention == 0 {
		cfg.Retention = 24 * time.Hour
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.MaxAttempts < 0 || cfg.MaxAttempts > math.MaxInt32 {
		return nil, fmt.Errorf("%w: MaxAttempts must be between 1 and %d", durable.ErrInvalidConfig, math.MaxInt32)
	}
	if cfg.InFlightTTL < 6*time.Millisecond || cfg.Retention <= 0 || cfg.RequestTimeout <= 0 {
		return nil, fmt.Errorf("%w: InFlightTTL >= 6ms, positive Retention and RequestTimeout", durable.ErrInvalidConfig)
	}
	return &Keys{cfg: cfg}, nil
}

// Request identifies one logical operation. Body must be JSON; it is compared
// with jsonb equality, so key order and whitespace do not matter.
type Request struct {
	Scope string // mandatory: tenant + operation, e.g. "t1:payments.create"
	Key   string
	Body  json.RawMessage
}

// Response is what later callers replay. For HTTP, store the status code,
// selected headers and body.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// State is the outcome of Begin.
type State string

const (
	Acquired  State = "acquired"  // you own the key: do the work, then Complete/Fail/Abort/Release
	InFlight  State = "in_flight" // another caller is working on it; retry later
	Backoff   State = "backoff"   // an attempt failed; retry after Claim.RetryAfter
	Completed State = "completed" // replay Claim.Response
	Failed    State = "failed"    // a stored failure; replay Claim.Response
	Exhausted State = "exhausted" // every attempt was used up; Claim.LastError says why
	Linked    State = "linked"    // the key belongs to an execution (Claim.ExecutionID)
)

// Checkpoint marks the last finished step of a multi-step request. The zero
// Checkpoint (empty Name) means no step has finished yet.
type Checkpoint struct {
	Name string
	Data json.RawMessage // JSON; nil is stored as NULL
}

// Claim is the result of Begin. Only an Acquired claim has methods that do
// anything; it renews its lease in the background until it is finished.
type Claim struct {
	State       State
	Response    *Response
	ExecutionID uuid.UUID
	Token       int64
	// Attempts made on the key, including this one when Acquired.
	Attempts int
	// RetryAfter is set when State is Backoff.
	RetryAfter time.Time
	// LastError is why the previous attempt failed (Acquired, Backoff,
	// Exhausted), or "" when none did.
	LastError string
	// Checkpoint is where an Acquired claim resumes: the last checkpoint a
	// previous attempt saved, or the zero Checkpoint.
	Checkpoint Checkpoint

	keys    *Keys
	req     Request
	owner   string
	row     engine.Row
	keeper  *engine.Keeper
	stopHB  context.CancelFunc
	hbDone  chan struct{}
	mu      sync.Mutex
	stopped bool
}

// Derive builds an unambiguous key from business identifiers, for callers
// that have no client-supplied key.
func Derive(parts ...string) string {
	b, _ := json.Marshal(parts)
	return string(b)
}

// Begin claims req. It returns ErrRequestMismatch when the key was used with a
// different body. It must not run inside a transaction: the claim has to be
// visible to concurrent callers immediately.
func (k *Keys) Begin(ctx context.Context, req Request) (*Claim, error) {
	if k.cfg.DB.IsTx(ctx) || dbtx.IsTx(ctx) {
		return nil, durable.ErrTxInContext
	}
	if req.Scope == "" || req.Key == "" {
		return nil, fmt.Errorf("%w: scope and key are required", durable.ErrInvalidArgument)
	}
	body, err := pg.ValidJSON(req.Body, "{}")
	if err != nil {
		return nil, fmt.Errorf("%w: body: %v", durable.ErrInvalidArgument, err)
	}
	req.Body = body
	owner := rand.Text()
	sent := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, k.cfg.RequestTimeout)
	defer cancel()
	row, err := postgres.New(k.cfg.DB.DB()).BeginKey(reqCtx, postgres.BeginKeyParams{
		Scope: req.Scope, Key: req.Key, Request: body, OwnerID: owner,
		LeaseUs: pg.Micros(k.cfg.InFlightTTL), RetentionUs: pg.Micros(k.cfg.Retention),
		MaxAttempts: int32(k.cfg.MaxAttempts),
	})
	if err != nil {
		return nil, fmt.Errorf("keys: begin: %w", err)
	}
	outcome, token, response := row.Outcome, row.Token, row.Response
	c := &Claim{State: State(outcome), Token: token, ExecutionID: row.ExecutionID, keys: k, req: req, owner: owner,
		Attempts: int(row.Attempts), LastError: row.LastError,
		Checkpoint: Checkpoint{Name: row.CheckpointName, Data: row.CheckpointData.Raw()},
		row:        engine.Row{Table: keysTable, Running: "in_flight", ID: row.KeyID, Owner: owner, Token: token}}
	if row.RetryAt.Unix() != 0 {
		c.RetryAfter = row.RetryAt
	}
	switch c.State {
	case "mismatch":
		return nil, durable.ErrRequestMismatch
	case Completed, Failed:
		if len(response) > 0 {
			c.Response = new(Response)
			if err := json.Unmarshal(response, c.Response); err != nil {
				return nil, fmt.Errorf("keys: stored response: %w", err)
			}
		}
		return c, nil
	case InFlight, Linked, Backoff, Exhausted:
		return c, nil
	case Acquired:
	default:
		return nil, fmt.Errorf("keys: unexpected outcome %q", outcome)
	}
	if !time.Now().Before(engine.SafeDeadline(sent, k.cfg.InFlightTTL)) {
		_ = c.Release(ctx)
		return nil, durable.ErrLeaseLost
	}
	c.keeper = engine.NewKeeper(k.cfg.InFlightTTL, k.cfg.RequestTimeout, sent, c.renew, func(ctx context.Context) {
		k.cfg.Hooks.CallBeforeHeartbeat(ctx, req.Key)
	})
	hbCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	c.stopHB, c.hbDone = stop, make(chan struct{})
	go func() {
		defer close(c.hbDone)
		c.keeper.Run(hbCtx, req.Key, func(err error) {
			durable.Observe(hbCtx, k.cfg.Observer, durable.Event{
				Type: durable.EventLeaseLost, Component: "keys", Key: req.Key, Token: token, Err: err,
			})
		})
	}()
	return c, nil
}

func (c *Claim) renew(ctx context.Context) (bool, error) {
	return c.row.Renew(ctx, postgres.New(c.keys.cfg.DB.DB()), c.keys.cfg.InFlightTTL)
}

// Done is closed when an Acquired claim is lost, expires locally, or finishes.
func (c *Claim) Done() <-chan struct{} {
	if c.keeper == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.keeper.Done()
}

// Err reports why Done closed.
func (c *Claim) Err() error {
	if c.keeper == nil {
		return nil
	}
	return c.keeper.Err()
}

func (c *Claim) stopHeartbeat() error {
	if c.State != Acquired || c.keeper == nil {
		return fmt.Errorf("%w: claim is %s, not acquired", durable.ErrInvalidArgument, c.State)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return fmt.Errorf("%w: claim already finished", durable.ErrInvalidArgument)
	}
	c.stopped = true
	c.stopHB()
	<-c.hbDone
	return nil
}

// Complete stores resp as the replayed result. It must run inside the caller's
// transaction (ctx from dbtx RunInTx) so the response commits atomically with
// the work. It fails with ErrLeaseLost if the claim expired or was taken over.
// If that transaction later rolls back, the key stays in flight until its
// lease expires and a retry can claim it.
func (c *Claim) Complete(txCtx context.Context, resp Response) error {
	return c.finish(txCtx, "completed", resp, true)
}

// Fail stores resp as a permanent failure that later callers replay. Use it
// for errors that retrying cannot fix; use Release for transient errors.
// It runs in its own statement, or joins a transaction in ctx.
func (c *Claim) Fail(ctx context.Context, resp Response) error {
	return c.finish(ctx, "failed", resp, false)
}

func (c *Claim) finish(ctx context.Context, status string, resp Response, requireTx bool) error {
	db := c.keys.cfg.DB
	if requireTx && !db.IsTx(ctx) {
		return durable.ErrNotInTx
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if err := c.stopHeartbeat(); err != nil {
		return err
	}
	defer c.keeper.Close()
	if err := c.keeper.Err(); err != nil {
		return err
	}
	c.keys.cfg.Hooks.CallBeforeComplete(ctx, c.req.Key)
	// Lock and check, finish the lease, then store the response: all in the
	// caller's transaction (or one of our own).
	return db.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(db.DBTx(txCtx))
		held, err := c.row.Lock(txCtx, q, true)
		if err != nil {
			return err
		}
		if !held {
			return durable.ErrLeaseLost
		}
		if _, err := c.row.Finish(txCtx, q, engine.Finish{Status: status}); err != nil {
			return err
		}
		return q.SetKeyResponse(txCtx, postgres.SetKeyResponseParams{
			ID: c.row.ID, Response: b, RetentionUs: pg.Micros(c.keys.cfg.Retention),
		})
	})
}

// halt stops the heartbeat of an Acquired claim unless a finishing call
// already did. The caller must Close the keeper afterwards.
func (c *Claim) halt() error {
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return nil
	}
	return c.stopHeartbeat()
}

// SaveCheckpoint records cp as the last finished step. It must run inside
// the transaction that holds the step's writes (ctx from dbtx RunInTx), so
// they commit together; it fails with ErrLeaseLost if the claim expired or
// was taken over, which fences out the step's writes too. The claim stays
// held: run the next step, then Complete, Fail, Abort or Release.
func (c *Claim) SaveCheckpoint(txCtx context.Context, cp Checkpoint) error {
	db := c.keys.cfg.DB
	if !db.IsTx(txCtx) {
		return durable.ErrNotInTx
	}
	if c.State != Acquired || c.keeper == nil {
		return fmt.Errorf("%w: claim is %s, not acquired", durable.ErrInvalidArgument, c.State)
	}
	data, err := pg.ValidJSON(cp.Data, "null")
	if err != nil {
		return fmt.Errorf("%w: checkpoint data: %v", durable.ErrInvalidArgument, err)
	}
	if len(cp.Data) == 0 {
		data = nil
	}
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return fmt.Errorf("%w: claim already finished", durable.ErrInvalidArgument)
	}
	if err := c.keeper.Err(); err != nil {
		return err
	}
	q := postgres.New(db.DBTx(txCtx))
	held, err := c.row.Lock(txCtx, q, true)
	if err != nil {
		return err
	}
	if !held {
		return durable.ErrLeaseLost
	}
	return q.SetKeyCheckpoint(txCtx, postgres.SetKeyCheckpointParams{
		ID: c.row.ID, CheckpointName: cp.Name, CheckpointData: pg.JSON(data),
	})
}

// Release gives the key up without counting the attempt, so the next caller
// can retry immediately. A key with no other attempt and no checkpoint is
// deleted. Use Abort instead when the attempt failed. Releasing a lost claim
// is harmless.
func (c *Claim) Release(ctx context.Context) error {
	if c.State != Acquired {
		return nil
	}
	if c.keeper != nil {
		if err := c.halt(); err != nil {
			return err
		}
		defer c.keeper.Close()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.keys.cfg.RequestTimeout)
	defer cancel()
	return postgres.New(c.keys.cfg.DB.DB()).ReleaseKey(ctx, postgres.ReleaseKeyParams{
		ID: c.row.ID, OwnerID: sql.NullString{String: c.owner, Valid: true}, Token: c.Token,
	})
}

// Abort ends a failed attempt and records cause. With attempts left, the key
// can be attempted again after Config.Backoff (or the delay of a
// durable.RetryAfter cause); otherwise it becomes Exhausted. Saved
// checkpoints are kept. Aborting a lost claim is harmless.
func (c *Claim) Abort(ctx context.Context, cause error) error {
	_, err := c.abort(ctx, cause)
	return err
}

func (c *Claim) abort(ctx context.Context, cause error) (State, error) {
	if c.State != Acquired {
		return c.State, nil
	}
	if c.keeper != nil {
		if err := c.halt(); err != nil {
			return "", err
		}
		defer c.keeper.Close()
	}
	msg := "aborted"
	if cause != nil {
		msg = cause.Error()
	}
	var delay time.Duration
	if d, ok := durable.RetryDelay(cause); ok {
		delay = d
	} else if c.keys.cfg.Backoff != nil {
		delay = c.keys.cfg.Backoff(max(c.Attempts, 1))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.keys.cfg.RequestTimeout)
	defer cancel()
	row, err := postgres.New(c.keys.cfg.DB.DB()).AbortKey(ctx, postgres.AbortKeyParams{
		ID: c.row.ID, OwnerID: sql.NullString{String: c.owner, Valid: true}, Token: c.Token,
		MaxAttempts: int32(c.keys.cfg.MaxAttempts), LastError: msg,
		DelayUs: pg.Micros(delay), RetentionUs: pg.Micros(c.keys.cfg.Retention),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // lost: the key belongs to someone else now
	}
	if err != nil {
		return "", fmt.Errorf("keys: abort: %w", err)
	}
	if row.Status == string(Exhausted) {
		return Exhausted, nil
	}
	if row.RetryAt.Valid {
		return Backoff, nil
	}
	return InFlight, nil
}

// Result is what one step of DoSteps returns: exactly one of Checkpoint (the
// step finished; run the next one) or Response (the request is done).
type Result struct {
	Checkpoint *Checkpoint
	Response   *Response
}

// Do runs fn at most once per successful request. fn runs inside a transaction
// that also stores its response. Replays return the stored response.
//
// Errors: ErrRequestMismatch, ErrInFlight, a *BackoffError (ErrBackoff),
// ErrExhausted, ErrLeaseLost, or fn's error. When fn returns an error marked
// durable.Permanent together with a response that has a non-zero Status, that
// response is stored and replayed (Q1). Any other error, or a panic, counts
// as a failed attempt (see Claim.Abort); the attempt that uses up
// MaxAttempts returns fn's error joined with ErrExhausted.
func (k *Keys) Do(ctx context.Context, req Request, fn func(txCtx context.Context) (Response, error)) (Response, error) {
	return k.DoSteps(ctx, req, func(txCtx context.Context, _ Checkpoint) (Result, error) {
		resp, err := fn(txCtx)
		return Result{Response: &resp}, err
	})
}

// DoSteps runs a request made of steps that commit one at a time. Each call
// of fn runs in its own transaction and gets the last saved checkpoint (the
// zero Checkpoint at the start). Returning Result.Checkpoint saves it with
// the step's writes, fenced by the claim's lease, and calls fn again;
// returning Result.Response stores it as in Do and ends the request.
//
// When an attempt fails part-way, the next attempt resumes from the last
// saved checkpoint: finished steps do not run again. Errors and the failure
// policy are those of Do; a result with neither or both fields set fails the
// attempt with ErrInvalidResult.
func (k *Keys) DoSteps(ctx context.Context, req Request, fn func(txCtx context.Context, cp Checkpoint) (Result, error)) (Response, error) {
	c, err := k.Begin(ctx, req)
	if err != nil {
		return Response{}, err
	}
	switch c.State {
	case Completed:
		return *c.Response, nil
	case Failed:
		return *c.Response, durable.Permanent(errors.New("keys: replayed stored failure"))
	case InFlight:
		return Response{}, durable.ErrInFlight
	case Backoff:
		return Response{}, &BackoffError{RetryAfter: c.RetryAfter, LastError: c.LastError}
	case Exhausted:
		return Response{}, durable.Permanent(fmt.Errorf("%w after %d attempts: %s", ErrExhausted, c.Attempts, c.LastError))
	case Linked:
		return Response{}, fmt.Errorf("%w: key belongs to execution %s", durable.ErrRequestMismatch, c.ExecutionID)
	}
	finished := false
	defer func() {
		// Covers panics in fn: the attempt failed and the key must not stay
		// in flight while this process lives.
		if !finished {
			p := recover()
			if p == nil { // runtime.Goexit
				_ = c.Abort(ctx, errors.New("handler exited"))
				return
			}
			_ = c.Abort(ctx, fmt.Errorf("panic: %v", p))
			panic(p)
		}
	}()
	resp, err := c.run(ctx, fn)
	finished = true
	if err == nil {
		return resp, nil
	}
	if durable.IsPermanent(err) && resp.Status != 0 {
		if ferr := c.Fail(ctx, resp); ferr != nil {
			return Response{}, errors.Join(err, ferr)
		}
		return resp, err
	}
	state, aerr := c.abort(ctx, err)
	if aerr != nil {
		return Response{}, errors.Join(err, aerr)
	}
	if state == Exhausted {
		return Response{}, durable.Permanent(errors.Join(err, ErrExhausted))
	}
	return Response{}, err
}

// run executes the steps of an Acquired claim. A permanent error comes back
// with the response fn returned alongside it.
func (c *Claim) run(ctx context.Context, fn func(context.Context, Checkpoint) (Result, error)) (Response, error) {
	cp := c.Checkpoint
	for {
		var res Result
		err := c.keys.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
			var fnErr error
			res, fnErr = fn(txCtx, cp)
			if fnErr != nil {
				return fnErr
			}
			if (res.Checkpoint == nil) == (res.Response == nil) {
				return ErrInvalidResult
			}
			if res.Checkpoint != nil {
				return c.SaveCheckpoint(txCtx, *res.Checkpoint)
			}
			return c.Complete(txCtx, *res.Response)
		})
		if err != nil {
			var resp Response
			if res.Response != nil && res.Checkpoint == nil {
				resp = *res.Response
			}
			return resp, err
		}
		if res.Response != nil {
			return *res.Response, nil
		}
		cp = *res.Checkpoint
	}
}

// Purge deletes up to limit expired keys, never one with a live lease. Run it
// periodically until it returns 0.
func (k *Keys) Purge(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	return postgres.New(k.cfg.DB.DB()).PurgeKeys(ctx, int32(limit))
}
