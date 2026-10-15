// Package inbox applies incoming messages once. Each Receive runs the apply
// function in the same transaction that records the message, so a
// redelivered message is recognised and not applied again.
//
// Three modes (decision D10):
//   - Dedup: apply once per (source, message ID).
//   - Sequenced: messages carry a producer sequence per aggregate (from an
//     ordered outbox). Only seq = last+1 is applied; older ones are
//     duplicates; newer ones are buffered and applied automatically, in
//     order, once the gap fills.
//   - Versioned: messages carry a version or timestamp per aggregate and only
//     newer versions are applied (for third-party sources without sequences).
package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Mode selects how a message is deduplicated and ordered.
type Mode string

const (
	ModeDedup     Mode = "dedup"
	ModeSequenced Mode = "sequenced"
	ModeVersioned Mode = "versioned"
)

// Message is one incoming message.
type Message struct {
	Source       string // who sent it; part of the identity
	MessageID    string // stable across redeliveries (not a broker delivery tag)
	AggregateKey string // Sequenced and Versioned modes
	Seq          int64  // Sequenced: producer sequence, starting at 1
	Version      int64  // Versioned: larger is newer (e.g. UnixNano of the event time)
	Type         string
	Payload      json.RawMessage
	IsGap        bool // a gap marker: advance past Seq without applying
}

// Result reports what Receive did. In every case except an error, it is safe
// to acknowledge the broker.
type Result string

const (
	Applied    Result = "applied"     // apply ran and committed
	Duplicate  Result = "duplicate"   // already seen; apply did not run
	Buffered   Result = "buffered"    // sequenced: waiting for an earlier seq
	GapSkipped Result = "gap_skipped" // a gap marker advanced the stream
	Enqueued   Result = "enqueued"    // Enqueue stored the message as a job
)

// ApplyFunc applies a message's effects using txCtx's transaction.
type ApplyFunc func(txCtx context.Context, msg Message) error

// Config is copied at construction.
type Config struct {
	DB *dbtx.DB
	// Jobs is required only for Enqueue.
	Jobs     *jobs.Client
	Observer durable.Observer
}

type Inbox struct{ cfg Config }

func New(cfg Config) (*Inbox, error) {
	if cfg.DB == nil || cfg.DB.Unwrap() == nil {
		return nil, fmt.Errorf("%w: DB is required", durable.ErrInvalidConfig)
	}
	return &Inbox{cfg: cfg}, nil
}

func (i *Inbox) validate(mode Mode, m Message) error {
	if m.Source == "" || m.MessageID == "" {
		return fmt.Errorf("%w: source and message ID are required", durable.ErrInvalidArgument)
	}
	switch mode {
	case ModeDedup:
	case ModeSequenced:
		if m.AggregateKey == "" || m.Seq < 1 {
			return fmt.Errorf("%w: sequenced messages need an aggregate key and seq >= 1", durable.ErrInvalidArgument)
		}
	case ModeVersioned:
		if m.AggregateKey == "" {
			return fmt.Errorf("%w: versioned messages need an aggregate key", durable.ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: mode %q", durable.ErrInvalidArgument, mode)
	}
	return nil
}

// Receive records msg and applies it according to mode, in one transaction
// (joining a dbtx transaction in ctx). If apply fails, nothing is recorded and
// the error is returned: do not acknowledge the broker.
func (i *Inbox) Receive(ctx context.Context, mode Mode, msg Message, apply ApplyFunc) (Result, error) {
	if apply == nil {
		return "", durable.ErrInvalidArgument
	}
	if err := i.validate(mode, msg); err != nil {
		return "", err
	}
	var res Result
	err := i.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		var err error
		switch mode {
		case ModeDedup:
			res, err = i.receiveDedup(txCtx, msg, apply)
		case ModeSequenced:
			res, err = i.receiveSequenced(txCtx, msg, apply)
		case ModeVersioned:
			res, err = i.receiveVersioned(txCtx, msg, apply)
		}
		return err
	})
	return res, err
}

func (i *Inbox) q(ctx context.Context) *postgres.Queries { return postgres.New(i.cfg.DB.DBTx(ctx)) }

func (i *Inbox) record(ctx context.Context, m Message, status string) (bool, error) {
	p := postgres.RecordMessageParams{Source: m.Source, MessageID: m.MessageID, Status: status}
	if m.AggregateKey != "" {
		p.AggregateKey = sql.NullString{String: m.AggregateKey, Valid: true}
	}
	if m.Seq > 0 {
		p.Seq = sql.NullInt64{Int64: m.Seq, Valid: true}
	}
	if m.Version != 0 {
		p.Version = sql.NullInt64{Int64: m.Version, Valid: true}
	}
	n, err := i.q(ctx).RecordMessage(ctx, p)
	return n == 1, err
}

func (i *Inbox) receiveDedup(ctx context.Context, m Message, apply ApplyFunc) (Result, error) {
	status := "applied"
	if m.IsGap {
		status = "skipped"
	}
	inserted, err := i.record(ctx, m, status)
	if err != nil || !inserted {
		return Duplicate, err
	}
	if m.IsGap {
		return GapSkipped, nil
	}
	return Applied, apply(ctx, m)
}

// lockStream creates the stream row if needed and locks it.
func (i *Inbox) lockStream(ctx context.Context, source, key string) (last int64, version *int64, err error) {
	q := i.q(ctx)
	if err := q.EnsureInboxStream(ctx, postgres.EnsureInboxStreamParams{Source: source, AggregateKey: key}); err != nil {
		return 0, nil, err
	}
	st, err := q.LockInboxStream(ctx, postgres.LockInboxStreamParams{Source: source, AggregateKey: key})
	if err != nil {
		return 0, nil, err
	}
	if st.LastVersion.Valid {
		version = &st.LastVersion.Int64
	}
	return st.LastAppliedSeq, version, nil
}

func (i *Inbox) receiveSequenced(ctx context.Context, m Message, apply ApplyFunc) (Result, error) {
	last, _, err := i.lockStream(ctx, m.Source, m.AggregateKey)
	if err != nil {
		return "", err
	}
	q := i.q(ctx)
	switch {
	case m.Seq <= last:
		return Duplicate, nil
	case m.Seq > last+1:
		if err := q.BufferMessage(ctx, postgres.BufferMessageParams{
			Source: m.Source, AggregateKey: m.AggregateKey, Seq: m.Seq, MessageID: m.MessageID,
			Type: m.Type, Payload: pg.JSON(m.Payload), IsGap: m.IsGap,
		}); err != nil {
			return "", err
		}
		if err := q.MarkInboxGap(ctx, postgres.MarkInboxGapParams{Source: m.Source, AggregateKey: m.AggregateKey}); err != nil {
			return "", err
		}
		durable.Observe(ctx, i.cfg.Observer, durable.Event{Type: durable.EventInboxGap, Component: "inbox",
			Kind: m.Source, Key: m.AggregateKey, Seq: last + 1})
		return Buffered, nil
	}
	res := Applied
	if m.IsGap {
		res = GapSkipped
	} else if err := apply(ctx, m); err != nil {
		return "", err
	}
	if _, err := i.record(ctx, m, map[bool]string{true: "skipped", false: "applied"}[m.IsGap]); err != nil {
		return "", err
	}
	if _, err := i.drainLocked(ctx, m.Source, m.AggregateKey, m.Seq, apply); err != nil {
		return "", err
	}
	return res, nil
}

// drainLocked applies buffered messages that follow last, in order, each in
// a savepoint. A failing message stops the drain and stays buffered. The
// stream row must be locked by the caller's transaction.
func (i *Inbox) drainLocked(ctx context.Context, source, key string, last int64, apply ApplyFunc) (int, error) {
	q := i.q(ctx)
	applied := 0
	for {
		b, err := q.GetBuffered(ctx, postgres.GetBufferedParams{Source: source, AggregateKey: key, Seq: last + 1})
		if isNoRows(err) {
			break
		}
		if err != nil {
			return applied, err
		}
		m := Message{Source: source, AggregateKey: key, Seq: b.Seq, MessageID: b.MessageID,
			Type: b.Type, Payload: b.Payload.Raw(), IsGap: b.IsGap}
		err = i.cfg.DB.RunInSubTx(ctx, func(subCtx context.Context) error {
			if !m.IsGap {
				if err := apply(subCtx, m); err != nil {
					return err
				}
			}
			if err := i.q(subCtx).DeleteBuffered(subCtx, postgres.DeleteBufferedParams{
				Source: source, AggregateKey: key, Seq: m.Seq,
			}); err != nil {
				return err
			}
			_, err := i.record(subCtx, m, map[bool]string{true: "skipped", false: "applied"}[m.IsGap])
			return err
		})
		if err != nil {
			break // stays buffered; Drain or a later Receive retries it
		}
		last++
		applied++
	}
	return applied, q.SetInboxApplied(ctx, postgres.SetInboxAppliedParams{Seq: last, Source: source, AggregateKey: key})
}

func (i *Inbox) receiveVersioned(ctx context.Context, m Message, apply ApplyFunc) (Result, error) {
	_, version, err := i.lockStream(ctx, m.Source, m.AggregateKey)
	if err != nil {
		return "", err
	}
	if version != nil && m.Version <= *version {
		return Duplicate, nil
	}
	if err := apply(ctx, m); err != nil {
		return "", err
	}
	if _, err := i.record(ctx, m, "applied"); err != nil {
		return "", err
	}
	return Applied, i.q(ctx).SetInboxVersion(ctx, postgres.SetInboxVersionParams{
		Version: sql.NullInt64{Int64: m.Version, Valid: true}, Source: m.Source, AggregateKey: m.AggregateKey,
	})
}

// Enqueue is store-and-forward: it records msg (Dedup) and submits a job for
// it in one transaction, so the broker can be acknowledged immediately and
// slow work runs with the job's retries. The job key is Source:MessageID in
// scope "inbox". It returns the job ID (zero for a duplicate).
func (i *Inbox) Enqueue(ctx context.Context, msg Message, req jobs.SubmitRequest) (Result, uuid.UUID, error) {
	if i.cfg.Jobs == nil {
		return "", uuid.UUID{}, fmt.Errorf("%w: Config.Jobs is required for Enqueue", durable.ErrInvalidConfig)
	}
	if err := i.validate(ModeDedup, msg); err != nil {
		return "", uuid.UUID{}, err
	}
	var (
		res Result
		id  uuid.UUID
	)
	err := i.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		inserted, err := i.record(txCtx, msg, "enqueued")
		if err != nil {
			return err
		}
		if !inserted {
			res = Duplicate
			return nil
		}
		if req.Payload == nil {
			req.Payload = msg.Payload
		}
		req.Scope, req.Key = "inbox", msg.Source+":"+msg.MessageID
		s, err := i.cfg.Jobs.Submit(txCtx, req)
		if err != nil {
			return err
		}
		id, res = s.ID, Enqueued
		return i.q(txCtx).SetMessageExecution(txCtx, postgres.SetMessageExecutionParams{
			ExecutionID: id, Source: msg.Source, MessageID: msg.MessageID,
		})
	})
	return res, id, err
}

// Drain applies buffered messages of one sequenced stream that are now next
// in line (for example after an earlier apply failure). It returns how many
// were applied.
func (i *Inbox) Drain(ctx context.Context, source, key string, apply ApplyFunc) (int, error) {
	var n int
	err := i.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		last, _, err := i.lockStream(txCtx, source, key)
		if err != nil {
			return err
		}
		n, err = i.drainLocked(txCtx, source, key, last, apply)
		return err
	})
	return n, err
}

// Gap is a sequenced stream waiting for a missing message.
type Gap struct {
	Source, AggregateKey string
	LastApplied          int64
	NextBuffered         int64
	Since                time.Time
}

// Gaps lists sequenced streams that have been waiting longer than minAge.
func (i *Inbox) Gaps(ctx context.Context, minAge time.Duration) ([]Gap, error) {
	rows, err := i.q(ctx).ListInboxGaps(ctx, pg.Micros(minAge))
	if err != nil {
		return nil, err
	}
	out := make([]Gap, len(rows))
	for n, r := range rows {
		out[n] = Gap{Source: r.Source, AggregateKey: r.AggregateKey, LastApplied: r.LastAppliedSeq,
			NextBuffered: r.NextBuffered, Since: r.GapSince}
	}
	return out, nil
}

// SkipGap gives up on the missing messages of one stream: it advances past
// them to the first buffered message and applies what follows. It returns
// the skipped range and how many buffered messages were applied.
func (i *Inbox) SkipGap(ctx context.Context, source, key string, apply ApplyFunc) (from, to int64, applied int, err error) {
	err = i.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		last, _, err := i.lockStream(txCtx, source, key)
		if err != nil {
			return err
		}
		next, err := i.q(txCtx).MinBufferedSeq(txCtx, postgres.MinBufferedSeqParams{Source: source, AggregateKey: key})
		if err != nil {
			return err
		}
		if next == 0 {
			return fmt.Errorf("%w: stream %s/%s has no buffered messages", durable.ErrNotFound, source, key)
		}
		from, to = last+1, next-1
		durable.Observe(txCtx, i.cfg.Observer, durable.Event{Type: durable.EventInboxGap, Component: "inbox",
			Kind: source, Key: key, Seq: from, Err: fmt.Errorf("inbox: skipped seq %d..%d", from, to)})
		applied, err = i.drainLocked(txCtx, source, key, to, apply)
		return err
	})
	return from, to, applied, err
}

// PurgeMessages deletes up to limit dedup records of source received before
// cutoff. Keep them longer than the source may redeliver. Stream positions
// and buffered messages are never purged here.
func (i *Inbox) PurgeMessages(ctx context.Context, source string, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	return postgres.New(i.cfg.DB.DB()).PurgeInboxMessages(ctx, postgres.PurgeInboxMessagesParams{
		Source: source, Cutoff: cutoff, Lim: int32(limit),
	})
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
