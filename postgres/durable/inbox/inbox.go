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
//
// # Failures and retries
//
// Receive never retries: if apply fails, nothing is recorded and the error
// is returned, so the broker redelivers and owns attempts, backoff and its
// own dead-letter queue. For retries owned by the database, use Enqueue: the
// message becomes a job (scope "inbox", key Source:MessageID) and gets the
// jobs package's attempts, backoff, failed state, jobs.Client.ListFailed and
// jobs.Client.Requeue. Inbox.Job finds the job for a message.
//
// Buffered sequenced messages are applied later, by a drain, when no broker
// delivery is in flight. A drain applies each one in a savepoint; when apply
// fails, the attempt and error are stored on the buffered message, an
// EventStreamBlocked is observed and the drain stops (the stream stays in
// order). After Config.MaxAttempts failures the message is parked
// (EventStreamParked): drains stop at it without calling apply until an
// operator lists it with Parked and calls Unpark (retry) or Discard (skip it).
// Drain, SkipGap, Unpark and Discard return a *DrainError for the failure;
// Receive does not, because the message it was given was applied.
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
	// MaxAttempts is how many times a drain may fail to apply one buffered
	// sequenced message before it is parked. Default 10.
	MaxAttempts int
}

// DefaultMaxAttempts is the default Config.MaxAttempts.
const DefaultMaxAttempts = 10

// ErrParked marks a DrainError whose message is parked.
var ErrParked = errors.New("inbox: buffered message is parked")

// DrainError reports the buffered message a drain stopped at because its
// apply failed (Err is that error), or because it is parked (Err is its last
// stored error). The failure is already recorded when this is returned.
type DrainError struct {
	Source, AggregateKey string
	Seq                  int64
	MessageID            string
	Attempts             int
	Parked               bool
	Err                  error
}

func (e *DrainError) Error() string {
	state := "failed"
	if e.Parked {
		state = "parked"
	}
	return fmt.Sprintf("inbox: buffered %s/%s seq %d (message %s) %s after %d attempt(s): %v",
		e.Source, e.AggregateKey, e.Seq, e.MessageID, state, e.Attempts, e.Err)
}

// Unwrap returns Err, plus ErrParked when the message is parked.
func (e *DrainError) Unwrap() []error {
	if e.Parked {
		return []error{e.Err, ErrParked}
	}
	return []error{e.Err}
}

type Inbox struct{ cfg Config }

func New(cfg Config) (*Inbox, error) {
	if cfg.DB == nil || cfg.DB.Unwrap() == nil {
		return nil, fmt.Errorf("%w: DB is required", durable.ErrInvalidConfig)
	}
	if cfg.MaxAttempts < 0 {
		return nil, fmt.Errorf("%w: MaxAttempts must not be negative", durable.ErrInvalidConfig)
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
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
	// A buffered message that fails here is recorded and observed by
	// drainLocked; msg itself was applied, so the broker may be acknowledged.
	if _, _, err := i.drainLocked(ctx, m.Source, m.AggregateKey, m.Seq, apply); err != nil {
		return "", err
	}
	return res, nil
}

// drainLocked applies buffered messages that follow last, in order, each in
// a savepoint. The stream row must be locked by the caller's transaction.
//
// It stops at the first message that fails or is parked and returns it as
// failed; a failure is counted on the buffered row (parking it at
// MaxAttempts) and observed before returning. err is only for database
// errors, after which the transaction must not commit.
func (i *Inbox) drainLocked(ctx context.Context, source, key string, last int64, apply ApplyFunc) (applied int, failed *DrainError, err error) {
	q := i.q(ctx)
	for {
		b, err := q.GetBuffered(ctx, postgres.GetBufferedParams{Source: source, AggregateKey: key, Seq: last + 1})
		if isNoRows(err) {
			break
		}
		if err != nil {
			return applied, nil, err
		}
		if b.ParkedAt.Valid {
			failed = &DrainError{Source: source, AggregateKey: key, Seq: b.Seq, MessageID: b.MessageID,
				Attempts: int(b.Attempts), Parked: true, Err: errors.New(b.LastError.String)}
			break
		}
		m := Message{Source: source, AggregateKey: key, Seq: b.Seq, MessageID: b.MessageID,
			Type: b.Type, Payload: b.Payload.Raw(), IsGap: b.IsGap}
		applyErr := i.cfg.DB.RunInSubTx(ctx, func(subCtx context.Context) error {
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
		if applyErr != nil {
			if ctx.Err() != nil {
				return applied, nil, ctx.Err() // not the message's fault; do not count it
			}
			failed, err = i.recordFailure(ctx, m, applyErr)
			if err != nil {
				return applied, nil, err
			}
			break // stays buffered; a later drain retries it unless parked
		}
		last++
		applied++
	}
	return applied, failed, q.SetInboxApplied(ctx, postgres.SetInboxAppliedParams{Seq: last, Source: source, AggregateKey: key})
}

func (i *Inbox) recordFailure(ctx context.Context, m Message, applyErr error) (*DrainError, error) {
	r, err := i.q(ctx).RecordBufferedFailure(ctx, postgres.RecordBufferedFailureParams{
		LastError: applyErr.Error(), MaxAttempts: int32(i.cfg.MaxAttempts),
		Source: m.Source, AggregateKey: m.AggregateKey, Seq: m.Seq,
	})
	if err != nil {
		return nil, errors.Join(applyErr, err)
	}
	ev := durable.Event{Type: durable.EventStreamBlocked, Component: "inbox", Kind: m.Source, Key: m.AggregateKey,
		Seq: m.Seq, Attempt: int(r.Attempts), Err: applyErr}
	if r.Parked {
		ev.Type = durable.EventStreamParked
	}
	durable.Observe(ctx, i.cfg.Observer, ev)
	return &DrainError{Source: m.Source, AggregateKey: m.AggregateKey, Seq: m.Seq, MessageID: m.MessageID,
		Attempts: int(r.Attempts), Parked: r.Parked, Err: applyErr}, nil
}

// drainTx locks a stream, runs fn and commits, then returns fn's drain
// failure (if any) so the recorded attempt survives.
func (i *Inbox) drainTx(ctx context.Context, source, key string, fn func(txCtx context.Context, last int64) (*DrainError, error)) error {
	if source == "" || key == "" {
		return fmt.Errorf("%w: source and aggregate key are required", durable.ErrInvalidArgument)
	}
	var failed *DrainError
	err := i.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		last, _, err := i.lockStream(txCtx, source, key)
		if err != nil {
			return err
		}
		failed, err = fn(txCtx, last)
		return err
	})
	if err != nil {
		return err
	}
	if failed != nil {
		return failed
	}
	return nil
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
// were applied, and a *DrainError if it stopped at a message that failed
// (the attempt is recorded) or is parked (match ErrParked). Messages applied
// before the failure stay committed.
func (i *Inbox) Drain(ctx context.Context, source, key string, apply ApplyFunc) (int, error) {
	if apply == nil {
		return 0, durable.ErrInvalidArgument
	}
	var n int
	err := i.drainTx(ctx, source, key, func(txCtx context.Context, last int64) (*DrainError, error) {
		var (
			failed *DrainError
			err    error
		)
		n, failed, err = i.drainLocked(txCtx, source, key, last, apply)
		return failed, err
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
// the skipped range and how many buffered messages were applied; err is a
// *DrainError if a buffered message then fails or is parked (the skip is
// still committed). A stream whose next message is buffered has no gap: use
// Drain, Unpark or Discard.
func (i *Inbox) SkipGap(ctx context.Context, source, key string, apply ApplyFunc) (from, to int64, applied int, err error) {
	if apply == nil {
		return 0, 0, 0, durable.ErrInvalidArgument
	}
	err = i.drainTx(ctx, source, key, func(txCtx context.Context, last int64) (*DrainError, error) {
		next, err := i.q(txCtx).MinBufferedSeq(txCtx, postgres.MinBufferedSeqParams{Source: source, AggregateKey: key})
		if err != nil {
			return nil, err
		}
		if next == 0 {
			return nil, fmt.Errorf("%w: stream %s/%s has no buffered messages", durable.ErrNotFound, source, key)
		}
		if next == last+1 {
			return nil, fmt.Errorf("%w: stream %s/%s has no gap; seq %d is buffered", durable.ErrInvalidArgument, source, key, next)
		}
		from, to = last+1, next-1
		durable.Observe(txCtx, i.cfg.Observer, durable.Event{Type: durable.EventInboxGap, Component: "inbox",
			Kind: source, Key: key, Seq: from, Err: fmt.Errorf("inbox: skipped seq %d..%d", from, to)})
		var failed *DrainError
		applied, failed, err = i.drainLocked(txCtx, source, key, to, apply)
		return failed, err
	})
	return from, to, applied, err
}

// Pending is a buffered sequenced message that has not been applied.
type Pending struct {
	Message
	Attempts   int
	LastError  string
	ReceivedAt time.Time
	ParkedAt   time.Time
}

// Parked lists parked buffered messages, oldest parked first. An empty
// source lists every source; limit defaults to 100.
func (i *Inbox) Parked(ctx context.Context, source string, limit int) ([]Pending, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := i.q(ctx).ListParked(ctx, postgres.ListParkedParams{Source: source, Lim: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Pending, len(rows))
	for n, r := range rows {
		out[n] = Pending{
			Message: Message{Source: r.Source, MessageID: r.MessageID, AggregateKey: r.AggregateKey, Seq: r.Seq,
				Type: r.Type, Payload: r.Payload.Raw(), IsGap: r.IsGap},
			Attempts: int(r.Attempts), LastError: r.LastError, ReceivedAt: r.ReceivedAt, ParkedAt: r.ParkedAt,
		}
	}
	return out, nil
}

// Unpark resets the attempts of buffered message seq (parked or not) and
// drains the stream, so seq gets MaxAttempts fresh attempts. It returns how
// many messages were applied and a *DrainError if the drain stopped.
func (i *Inbox) Unpark(ctx context.Context, source, key string, seq int64, apply ApplyFunc) (int, error) {
	if apply == nil {
		return 0, durable.ErrInvalidArgument
	}
	var n int
	err := i.drainTx(ctx, source, key, func(txCtx context.Context, last int64) (*DrainError, error) {
		rows, err := i.q(txCtx).UnparkBuffered(txCtx, postgres.UnparkBufferedParams{Source: source, AggregateKey: key, Seq: seq})
		if err != nil {
			return nil, err
		}
		if rows == 0 {
			return nil, fmt.Errorf("%w: no buffered seq %d in stream %s/%s", durable.ErrNotFound, seq, source, key)
		}
		var failed *DrainError
		n, failed, err = i.drainLocked(txCtx, source, key, last, apply)
		return failed, err
	})
	return n, err
}

// Discard gives up on buffered message seq, which must be next in line (a
// parked or failing message): it is removed without being applied, recorded
// as skipped, and the messages after it are drained. It returns how many
// were applied and a *DrainError if the drain stopped again.
func (i *Inbox) Discard(ctx context.Context, source, key string, seq int64, apply ApplyFunc) (int, error) {
	if apply == nil {
		return 0, durable.ErrInvalidArgument
	}
	var n int
	err := i.drainTx(ctx, source, key, func(txCtx context.Context, last int64) (*DrainError, error) {
		q := i.q(txCtx)
		if seq != last+1 {
			return nil, fmt.Errorf("%w: stream %s/%s is at seq %d; only seq %d can be discarded",
				durable.ErrInvalidArgument, source, key, last, last+1)
		}
		b, err := q.GetBuffered(txCtx, postgres.GetBufferedParams{Source: source, AggregateKey: key, Seq: seq})
		if isNoRows(err) {
			return nil, fmt.Errorf("%w: no buffered seq %d in stream %s/%s", durable.ErrNotFound, seq, source, key)
		}
		if err != nil {
			return nil, err
		}
		if err := q.DeleteBuffered(txCtx, postgres.DeleteBufferedParams{Source: source, AggregateKey: key, Seq: seq}); err != nil {
			return nil, err
		}
		m := Message{Source: source, AggregateKey: key, Seq: seq, MessageID: b.MessageID}
		if _, err := i.record(txCtx, m, "skipped"); err != nil {
			return nil, err
		}
		durable.Observe(txCtx, i.cfg.Observer, durable.Event{Type: durable.EventInboxGap, Component: "inbox",
			Kind: source, Key: key, Seq: seq, Attempt: int(b.Attempts),
			Err: fmt.Errorf("inbox: discarded seq %d (message %s): %s", seq, b.MessageID, b.LastError.String)})
		var failed *DrainError
		n, failed, err = i.drainLocked(txCtx, source, key, seq, apply)
		return failed, err
	})
	return n, err
}

// Job returns the job Enqueue created for a message (scope "inbox", key
// Source:MessageID). Use the jobs.Client for its retries and dead letters:
// ListFailed, Requeue, RequeueFailed. It needs Config.Jobs.
func (i *Inbox) Job(ctx context.Context, source, messageID string) (jobs.Job, error) {
	if i.cfg.Jobs == nil {
		return jobs.Job{}, fmt.Errorf("%w: Config.Jobs is required for Job", durable.ErrInvalidConfig)
	}
	return i.cfg.Jobs.GetByKey(ctx, "inbox", source+":"+messageID)
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
