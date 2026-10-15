package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// StreamInfo describes one ordered stream.
type StreamInfo struct {
	Topic, AggregateKey string
	Status              string // active, blocked, stopped
	LastPublishedSeq    int64
	HeadSeq             int64 // last sequence assigned to a producer
	Pending             int   // events not yet published
	Attempts            int   // consecutive failures of the head event
	LastError           string
	RunAt               time.Time
}

// Stream returns the state of one ordered stream.
func (o *Outbox) Stream(ctx context.Context, topic, aggregateKey string) (StreamInfo, error) {
	si := StreamInfo{Topic: topic, AggregateKey: aggregateKey, Status: "active"}
	q := postgres.New(o.cfg.DB.DBTx(ctx))
	st, err := q.GetStream(ctx, postgres.GetStreamParams{Topic: topic, AggregateKey: aggregateKey})
	switch {
	case err == nil:
		si.Status, si.LastPublishedSeq, si.Attempts = st.Status, st.LastPublishedSeq, int(st.Attempts)
		si.LastError, si.RunAt = st.LastError, st.RunAt
	case !errors.Is(err, sql.ErrNoRows):
		return si, err
	}
	head, err := q.GetStreamHead(ctx, postgres.GetStreamHeadParams{
		Topic: topic, AggregateKey: aggregateKey,
	})
	if err != nil {
		return si, err
	}
	si.HeadSeq, si.Pending = head.HeadSeq, int(head.Pending)
	if si.HeadSeq == 0 {
		return si, durable.ErrNotFound
	}
	return si, nil
}

// Resume reactivates a blocked or stopped stream and resets its failures. A
// new token fences out any relay still holding the stream (D18).
func (o *Outbox) Resume(ctx context.Context, topic, aggregateKey string) error {
	n, err := postgres.New(o.cfg.DB.DBTx(ctx)).ActivateStream(ctx, postgres.ActivateStreamParams{
		Topic: topic, AggregateKey: aggregateKey, OnlyInactive: true,
	})
	return expectOne(n, err, "blocked or stopped stream")
}

// Skip replaces the unpublished event at seq with a gap marker (consumers see
// the gap explicitly), records reason, and resumes the stream.
func (o *Outbox) Skip(ctx context.Context, topic, aggregateKey string, seq int64, reason string) error {
	db := o.cfg.DB
	return db.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(db.DBTx(txCtx))
		n, err := q.MarkEventGap(txCtx, postgres.MarkEventGapParams{
			Reason: reason, Topic: topic, AggregateKey: aggregateKey,
			Seq: seq,
		})
		if err := expectOne(n, err, "pending event at that sequence"); err != nil {
			return err
		}
		_, err = q.ActivateStream(txCtx, postgres.ActivateStreamParams{Topic: topic, AggregateKey: aggregateKey})
		return err
	})
}

// Replay republishes a stream from fromSeq, for a consumer that lost its
// state. Only events still retained (not purged) can be replayed; it returns
// how many were queued again. Any relay holding the stream is fenced out.
func (o *Outbox) Replay(ctx context.Context, topic, aggregateKey string, fromSeq int64) (int64, error) {
	if fromSeq < 1 {
		return 0, fmt.Errorf("%w: fromSeq must be >= 1", durable.ErrInvalidArgument)
	}
	var n int64
	db := o.cfg.DB
	key := aggregateKey
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(db.DBTx(txCtx))
		oldest, err := q.OldestRetainedSeq(txCtx, postgres.OldestRetainedSeqParams{Topic: topic, AggregateKey: key, FromSeq: fromSeq})
		if err != nil {
			return err
		}
		if oldest != fromSeq {
			return fmt.Errorf("%w: events from seq %d are no longer retained", durable.ErrNotFound, fromSeq)
		}
		if n, err = q.RequeuePublishedFrom(txCtx, postgres.RequeuePublishedFromParams{Topic: topic, AggregateKey: key, FromSeq: fromSeq}); err != nil {
			return err
		}
		return q.RewindStream(txCtx, postgres.RewindStreamParams{FromSeq: fromSeq, Topic: topic, AggregateKey: aggregateKey})
	})
	return n, err
}

// PurgePublished deletes up to limit events published before cutoff (Q7:
// keep a replay window). Stream positions and heads are kept.
func (o *Outbox) PurgePublished(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	return postgres.New(o.cfg.DB.DB()).PurgePublishedEvents(ctx, postgres.PurgePublishedEventsParams{Cutoff: cutoff, Lim: int32(limit)})
}

// DeadEvent is an unordered event that ran out of attempts.
type DeadEvent struct {
	ID        uuid.UUID
	Topic     string
	Type      string
	Attempts  int
	LastError string
}

// DeadLetters lists dead unordered events, oldest first.
func (o *Outbox) DeadLetters(ctx context.Context, limit int) ([]DeadEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := postgres.New(o.cfg.DB.DBTx(ctx)).DeadEvents(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]DeadEvent, len(rows))
	for i, r := range rows {
		out[i] = DeadEvent{ID: r.ID, Topic: r.Topic, Type: r.Type, Attempts: int(r.Attempts), LastError: r.LastError}
	}
	return out, nil
}

// Requeue makes a dead unordered event pending again with fresh attempts.
func (o *Outbox) Requeue(ctx context.Context, id uuid.UUID) error {
	n, err := postgres.New(o.cfg.DB.DBTx(ctx)).RequeueDeadEvent(ctx, id)
	return expectOne(n, err, "dead event")
}

func expectOne(n int64, err error, what string) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: no %s", durable.ErrNotFound, what)
	}
	return nil
}
