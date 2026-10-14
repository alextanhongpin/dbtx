package durable

import (
	"errors"
	"fmt"
)

// Errors shared by every subpackage. Match them with errors.Is.
var (
	ErrContended         = errors.New("durable: resource is held by another owner")
	ErrLeaseLost         = errors.New("durable: ownership lost or expired")
	ErrReleased          = errors.New("durable: released")
	ErrLeaseBusy         = errors.New("durable: lifecycle operation already in progress")
	ErrStaleToken        = errors.New("durable: stale fencing token")
	ErrTokenExhausted    = errors.New("durable: fencing token exhausted")
	ErrNotInTx           = errors.New("durable: transaction required in context")
	ErrTxInContext       = errors.New("durable: context carries a transaction")
	ErrInvalidConfig     = errors.New("durable: invalid configuration")
	ErrInvalidArgument   = errors.New("durable: invalid argument")
	ErrNotFound          = errors.New("durable: not found")
	ErrRequestMismatch   = errors.New("durable: idempotency key reused with a different request")
	ErrInFlight          = errors.New("durable: request with this key is in flight")
	ErrCancelled         = errors.New("durable: cancelled")
	ErrUnknownKind       = errors.New("durable: unregistered kind")
	ErrUnknownTopic      = errors.New("durable: unregistered topic")
	ErrAggregateRequired = errors.New("durable: ordered topic requires an aggregate key")
	ErrRemoteStepInTx    = errors.New("durable: remote steps are not allowed in tx mode")
)

// TxOutcome identifies the effect of one transaction the library committed on
// the caller's behalf.
type TxOutcome string

const (
	TxCommitted  TxOutcome = "committed"   // COMMIT confirmed; a later step failed
	TxRolledBack TxOutcome = "rolled_back" // nothing was committed
	TxUnknown    TxOutcome = "unknown"     // COMMIT was sent but not confirmed
)

// TxOutcomeError reports what happened to a transaction alongside its error.
// Do not retry work just because an error is non-nil: check the outcome.
type TxOutcomeError struct {
	Outcome TxOutcome
	Err     error
}

func (e *TxOutcomeError) Error() string {
	return fmt.Sprintf("durable: transaction %s: %v", e.Outcome, e.Err)
}

func (e *TxOutcomeError) Unwrap() error { return e.Err }
