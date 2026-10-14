package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/engine"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Execution is the handler's view of one claimed attempt.
type Execution struct {
	ID             uuid.UUID
	Queue, Kind    string
	Scope, Key     string
	Payload        json.RawMessage
	PayloadVersion int
	Mode           Mode
	Attempt        int // 1-based
	MaxAttempts    int
	Token          int64

	checkpointName string
	checkpointData json.RawMessage

	w         *Worker
	owner     string
	keeper    *engine.Keeper // activity mode only
	mu        sync.Mutex
	finalized bool
	// inLockedTx is set while a tx-mode step or Finalize holds the row lock.
	inLockedTx bool
}

// Decode unmarshals the payload into v.
func (e *Execution) Decode(v any) error { return json.Unmarshal(e.Payload, v) }

// Checkpoint returns the last saved checkpoint; the name is "" before any.
func (e *Execution) Checkpoint() (name string, data json.RawMessage) {
	return e.checkpointName, e.checkpointData
}

// DecodeCheckpoint unmarshals the checkpoint data into v. It is a no-op
// before the first checkpoint.
func (e *Execution) DecodeCheckpoint(v any) error {
	if len(e.checkpointData) == 0 {
		return nil
	}
	return json.Unmarshal(e.checkpointData, v)
}

func (e *Execution) db() *dbtx.DB { return e.w.cfg.DB }

// row is this attempt's lease on the executions table. Ownership changes go
// through it, so the fencing rules live in the durable.lease_* functions.
func (e *Execution) row() engine.Row {
	return engine.Row{Table: executionsTable, Running: string(StatusRunning), ID: e.ID, Owner: e.owner, Token: e.Token}
}

// Result is what a handler returns: Done finishes, Next saves a checkpoint
// and calls the handler again.
type Result struct {
	output     any
	checkpoint bool
	name       string
	data       any
}

// Done finishes the execution with output (JSON-encoded; nil becomes null).
func Done(output any) Result { return Result{output: output} }

// Next saves a checkpoint and calls the handler again with it. In ModeTx the
// checkpoint commits with the step's writes.
func Next(name string, data any) Result {
	return Result{checkpoint: true, name: name, data: data}
}

// Handler runs one call of an execution.
type Handler func(ctx context.Context, exec *Execution) (Result, error)

// Finalize completes an activity-mode execution atomically with fn's writes:
// fn runs in a transaction admitted only while this attempt still holds a
// valid lease, and its output is stored as the result in the same
// transaction. After a successful Finalize the handler's own Result is
// ignored. In ModeTx use Done instead.
func (e *Execution) Finalize(ctx context.Context, fn func(txCtx context.Context) (any, error)) error {
	if e.Mode != ModeActivity || e.keeper == nil {
		return fmt.Errorf("%w: Finalize is for activity mode", durable.ErrInvalidArgument)
	}
	if err := e.keeper.BeginOp(ctx, false); err != nil {
		return err
	}
	defer e.keeper.EndOp()
	if err := e.keeper.Err(); err != nil {
		return &durable.TxOutcomeError{Outcome: durable.TxRolledBack, Err: err}
	}
	cfg := e.w.cfg
	txCtx, cancel := context.WithTimeout(ctx, cfg.StepTimeout)
	defer cancel()
	callbackDone := false
	defer e.keeper.Unprotect()
	err := cfg.DB.RunInTx(txCtx, func(txCtx context.Context) error {
		q := postgres.New(cfg.DB.DBTx(txCtx))
		if err := q.SetLocalTimeouts(txCtx, pg.Timeout(cfg.StepTimeout)); err != nil {
			return err
		}
		held, err := e.row().Lock(txCtx, q, true)
		if err != nil {
			return err
		}
		if !held {
			e.keeper.Lose(durable.ErrLeaseLost)
			return durable.ErrLeaseLost
		}
		deadline, _ := txCtx.Deadline()
		if err := e.keeper.Protect(deadline); err != nil {
			return err
		}
		e.inLockedTx = true
		defer func() { e.inLockedTx = false }()
		out, err := fn(txCtx)
		if err != nil {
			return err
		}
		output, err := pg.Marshal(out)
		if err != nil {
			return durable.Permanent(fmt.Errorf("jobs: output: %w", err))
		}
		cfg.Hooks.CallBeforeComplete(txCtx, e.ID.String())
		if err := e.w.complete(txCtx, q, e, output); err != nil {
			return err
		}
		callbackDone = true
		return nil
	})
	if err != nil {
		outcome := durable.TxRolledBack
		if callbackDone {
			outcome = durable.TxUnknown
		}
		return &durable.TxOutcomeError{Outcome: outcome, Err: err}
	}
	e.mu.Lock()
	e.finalized = true
	e.mu.Unlock()
	return nil
}

func (e *Execution) isFinalized() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.finalized
}

// RemoteKey is the idempotency key RunStep sends for step name: the same on
// every attempt of this execution.
func (e *Execution) RemoteKey(step string) string { return e.ID.String() + ":" + step }

// RunStep calls a remote API at most once per execution in effect, given an
// API that honours idempotency keys. The first time, build creates the
// request, which is saved before call runs with RemoteKey(name). A retry
// resends the saved request with the same key; once the response is saved,
// later attempts return it without calling. Activity mode only. Req and Resp
// are inferred from build and call.
func (e *Execution) RunStep[Req, Resp any](
	ctx context.Context,
	name string,
	build func(ctx context.Context) (Req, error),
	call func(ctx context.Context, idempotencyKey string, req Req) (Resp, error),
) (Resp, error) {
	var zero Resp
	if e.Mode != ModeActivity {
		return zero, durable.ErrRemoteStepInTx
	}
	if name == "" || build == nil || call == nil {
		return zero, durable.ErrInvalidArgument
	}
	db := e.db()
	q := postgres.New(db.DB())
	step, err := q.GetStep(ctx, postgres.GetStepParams{ExecutionID: e.ID, Name: name})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r, err := build(ctx)
		if err != nil {
			return zero, err
		}
		reqBytes, err := json.Marshal(r)
		if err != nil {
			return zero, durable.Permanent(fmt.Errorf("jobs: step %s request: %w", name, err))
		}
		err = e.locked(ctx, func(txCtx context.Context, q *postgres.Queries) error {
			_, err := q.InsertStep(txCtx, postgres.InsertStepParams{
				ExecutionID: e.ID, Name: name, RemoteKey: e.RemoteKey(name), Request: reqBytes,
			})
			return err
		})
		if err != nil {
			return zero, err
		}
		// A stale attempt may have saved the request first: always continue
		// from what is stored.
		if step, err = q.GetStep(ctx, postgres.GetStepParams{ExecutionID: e.ID, Name: name}); err != nil {
			return zero, err
		}
	case err != nil:
		return zero, err
	}
	if step.Status == "done" {
		var out Resp
		if err := json.Unmarshal(step.Response, &out); err != nil {
			return zero, durable.Permanent(fmt.Errorf("jobs: step %s saved response: %w", name, err))
		}
		return out, nil
	}
	var r Req
	if err := json.Unmarshal(step.Request, &r); err != nil {
		return zero, durable.Permanent(fmt.Errorf("jobs: step %s saved request: %w", name, err))
	}
	out, err := call(ctx, e.RemoteKey(name), r)
	e.w.cfg.Hooks.CallAfterRemoteCall(ctx, e.ID.String(), name)
	if err != nil {
		return zero, err
	}
	respB, err := json.Marshal(out)
	if err != nil {
		return zero, durable.Permanent(fmt.Errorf("jobs: step %s response: %w", name, err))
	}
	err = e.locked(ctx, func(txCtx context.Context, q *postgres.Queries) error {
		_, err := q.FinishStep(txCtx, postgres.FinishStepParams{ExecutionID: e.ID, Name: name, Response: respB})
		return err
	})
	if err != nil {
		return zero, err
	}
	e.w.cfg.Hooks.CallBetweenSteps(ctx, e.ID.String(), name)
	return out, nil
}

// locked runs fn in a transaction that first locks this attempt's lease
// (ErrLeaseLost when it is no longer held). Feature writes inside fn are
// therefore fenced.
func (e *Execution) locked(ctx context.Context, fn func(txCtx context.Context, q *postgres.Queries) error) error {
	db := e.db()
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(db.DBTx(txCtx))
		held, err := e.row().Lock(txCtx, q, true)
		if err != nil {
			return err
		}
		if !held {
			return durable.ErrLeaseLost
		}
		return fn(txCtx, q)
	})
	if errors.Is(err, durable.ErrLeaseLost) && e.keeper != nil {
		e.keeper.Lose(durable.ErrLeaseLost)
	}
	return err
}
