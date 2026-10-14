// Package keys makes synchronous requests idempotent. Begin claims a
// (scope, key) pair; the first caller does the work and stores a response
// that later callers with the same request receive instead of redoing it.
package keys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// Config is copied at construction. Zero durations select defaults.
type Config struct {
	DB             *dbtx.DB
	InFlightTTL    time.Duration // lease on an in-flight key, renewed while held; default 30s
	Retention      time.Duration // how long completed keys are replayed; default 24h
	RequestTimeout time.Duration // per database call; default 5s
	Observer       durable.Observer
	Hooks          *durable.Hooks
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
	Acquired  State = "acquired"  // you own the key: do the work, then Complete/Fail/Release
	InFlight  State = "in_flight" // another caller is working on it; retry later
	Completed State = "completed" // replay Claim.Response
	Failed    State = "failed"    // a stored failure; replay Claim.Response
	Linked    State = "linked"    // the key belongs to an execution (Claim.ExecutionID)
)

// Claim is the result of Begin. Only an Acquired claim has methods that do
// anything; it renews its lease in the background until it is finished.
type Claim struct {
	State       State
	Response    *Response
	ExecutionID uuid.UUID
	Token       int64

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
	})
	if err != nil {
		return nil, fmt.Errorf("keys: begin: %w", err)
	}
	outcome, token, response := row.Outcome, row.Token, row.Response
	c := &Claim{State: State(outcome), Token: token, ExecutionID: row.ExecutionID, keys: k, req: req, owner: owner,
		row: engine.Row{Table: keysTable, Running: "in_flight", ID: row.KeyID, Owner: owner, Token: token}}
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
	case InFlight, Linked:
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

// Release gives the key up without storing anything, so the next caller can
// retry immediately. Releasing a lost claim is harmless.
func (c *Claim) Release(ctx context.Context) error {
	if c.State != Acquired {
		return nil
	}
	if c.keeper != nil {
		c.mu.Lock()
		stopped := c.stopped
		c.mu.Unlock()
		if !stopped {
			if err := c.stopHeartbeat(); err != nil {
				return err
			}
		}
		defer c.keeper.Close()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.keys.cfg.RequestTimeout)
	defer cancel()
	return postgres.New(c.keys.cfg.DB.DB()).ReleaseKey(ctx, postgres.ReleaseKeyParams{
		ID: c.row.ID, OwnerID: sql.NullString{String: c.owner, Valid: true}, Token: c.Token,
	})
}

// Do runs fn at most once per successful request. fn runs inside a transaction
// that also stores its response. Replays return the stored response.
//
// Errors: ErrRequestMismatch, ErrInFlight, ErrLeaseLost, or fn's error. When
// fn returns an error marked durable.Permanent together with a response that
// has a non-zero Status, that response is stored and replayed (Q1); any other
// error releases the key so the caller can retry.
func (k *Keys) Do(ctx context.Context, req Request, fn func(txCtx context.Context) (Response, error)) (Response, error) {
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
	case Linked:
		return Response{}, fmt.Errorf("%w: key belongs to execution %s", durable.ErrRequestMismatch, c.ExecutionID)
	}
	finished := false
	defer func() {
		// Covers panics in fn and every error path that did not store an
		// outcome: the key must not stay in flight while this process lives.
		if !finished {
			_ = c.Release(ctx)
		}
	}()
	var resp Response
	err = k.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		var fnErr error
		resp, fnErr = fn(txCtx)
		if fnErr != nil {
			return fnErr
		}
		return c.Complete(txCtx, resp)
	})
	if err == nil {
		finished = true
		return resp, nil
	}
	if durable.IsPermanent(err) && resp.Status != 0 {
		if ferr := c.Fail(ctx, resp); ferr != nil {
			return Response{}, errors.Join(err, ferr)
		}
		finished = true
		return resp, err
	}
	return Response{}, err
}

// Purge deletes up to limit expired keys, never one with a live lease. Run it
// periodically until it returns 0.
func (k *Keys) Purge(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	return postgres.New(k.cfg.DB.DB()).PurgeKeys(ctx, int32(limit))
}
