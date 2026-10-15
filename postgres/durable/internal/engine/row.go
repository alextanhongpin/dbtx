package engine

import (
	"context"
	"database/sql"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Row identifies one held lease on a table that follows the lease contract
// (see durable.lease_* in schema.sql). Its methods are the only way features
// change ownership, so the fencing rules live in the database functions.
type Row struct {
	Table   string // e.g. "durable.executions"
	Running string // status of a held row, e.g. "running"
	ID      uuid.UUID
	Owner   string
	Token   int64
}

// Renew extends a live lease; false means it is lost.
func (r Row) Renew(ctx context.Context, q *postgres.Queries, ttl time.Duration) (bool, error) {
	return q.LeaseRenew(ctx, postgres.LeaseRenewParams{
		Tbl: r.Table, ID: r.ID, OwnerID: r.Owner, Token: r.Token,
		RunningStatus: r.Running, TtlUs: pg.Micros(ttl),
	})
}

// Lock takes the row lock and reports whether this owner still holds the
// lease. Call it first in a transaction that then writes feature columns.
func (r Row) Lock(ctx context.Context, q *postgres.Queries, requireUnexpired bool) (bool, error) {
	return q.LeaseLock(ctx, postgres.LeaseLockParams{
		Tbl: r.Table, ID: r.ID, OwnerID: r.Owner, Token: r.Token,
		RunningStatus: r.Running, RequireUnexpired: requireUnexpired,
	})
}

// Finish describes how a lease ends.
type Finish struct {
	Status string
	// RequireUnexpired is false only while the caller holds the row lock.
	RequireUnexpired bool
	// Delay sets run_at to now + Delay when non-nil.
	Delay *time.Duration
	// LastError sets last_error when non-nil ("" clears it).
	LastError *string
	// AttemptDelta adjusts attempts (-1 gives an attempt back).
	AttemptDelta int
}

// Finish ends the lease; false means this owner no longer held it.
func (r Row) Finish(ctx context.Context, q *postgres.Queries, f Finish) (bool, error) {
	p := postgres.LeaseFinishParams{
		Tbl: r.Table, ID: r.ID, OwnerID: r.Owner, Token: r.Token, RunningStatus: r.Running,
		RequireUnexpired: f.RequireUnexpired, Status: f.Status, AttemptDelta: int32(f.AttemptDelta),
	}
	if f.Delay != nil {
		p.DelayUs = sql.NullInt64{Int64: pg.Micros(*f.Delay), Valid: true}
	}
	if f.LastError != nil {
		p.LastError = sql.NullString{String: *f.LastError, Valid: true}
	}
	return q.LeaseFinish(ctx, p)
}

// Expire moves an expired lease (owner presumed dead) to status.
func (r Row) Expire(ctx context.Context, q *postgres.Queries, status, reason string) (bool, error) {
	return q.LeaseExpire(ctx, postgres.LeaseExpireParams{
		Tbl: r.Table, ID: r.ID, OwnerID: r.Owner, Token: r.Token,
		RunningStatus: r.Running, Status: status, Reason: reason,
	})
}

// Ptr returns a pointer to v (for Finish fields).
func Ptr[T any](v T) *T { return &v }
