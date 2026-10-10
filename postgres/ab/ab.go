// Package ab stores two-variant experiments with one binary conversion per subject.
package ab

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/ab/internal/validate"
)

var (
	ErrNotExist    = errors.New("ab: not exist")
	ErrExists      = errors.New("ab: experiment exists")
	ErrClosed      = errors.New("ab: experiment closed")
	ErrNotAssigned = errors.New("ab: subject not assigned")
)

type Experiment struct {
	ID        string
	Control   string
	Treatment string
	CreatedAt time.Time
	ClosedAt  *time.Time
}

func (e Experiment) Validate() error {
	if err := validate.ID("experiment", e.ID); err != nil {
		return err
	}
	if strings.TrimSpace(e.Control) == "" || strings.TrimSpace(e.Treatment) == "" || e.Control == e.Treatment {
		return fmt.Errorf("ab: experiment requires an ID and two distinct nonempty variants")
	}
	return nil
}

type Assignment struct {
	ExperimentID string
	SubjectID    string
	Variant      string
	AssignedAt   time.Time
	ConvertedAt  *time.Time
}

type Counts struct {
	Subjects    int64
	Conversions int64
}

type Results struct {
	Experiment Experiment
	Control    Counts
	Treatment  Counts
}

// Repository implementations must enforce immutable experiments, sticky assignments,
// and idempotent conversions. Close must serialize with assignment and recording.
type Repository interface {
	Create(context.Context, Experiment) error
	Load(context.Context, string) (Experiment, error)
	Assign(context.Context, string, string, string) (Assignment, error)
	Record(context.Context, string, string) error
	Close(context.Context, string) error
	Results(context.Context, string) (Results, error)
}

// AB is safe for concurrent use when its repository is. It owns no resources.
type AB struct{ repo Repository }

func New(repo Repository) *AB { return &AB{repo: repo} }

// Create stores an immutable experiment. Use a fresh ID for every new experiment.
func (a *AB) Create(ctx context.Context, e Experiment) error {
	if err := e.Validate(); err != nil {
		return err
	}
	return a.repo.Create(ctx, e)
}
func (a *AB) Load(ctx context.Context, id string) (Experiment, error) {
	if err := validate.ID("experiment", id); err != nil {
		return Experiment{}, err
	}
	return a.repo.Load(ctx, id)
}

// Assign enrolls a subject on exposure. SHA-256 of length-prefixed experiment and
// subject IDs chooses equal-probability variants. Existing assignments are sticky,
// including after closure; new assignments after closure return ErrClosed.
func (a *AB) Assign(ctx context.Context, id, subject string) (Assignment, error) {
	if err := validate.Subject(id, subject); err != nil {
		return Assignment{}, err
	}
	e, err := a.repo.Load(ctx, id)
	if err != nil {
		return Assignment{}, err
	}
	h := sha256.New()
	var n [8]byte
	for _, s := range []string{id, subject} {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	variant := e.Control
	if h.Sum(nil)[0]&1 == 1 {
		variant = e.Treatment
	}
	return a.repo.Assign(ctx, id, subject, variant)
}

// Record marks an enrolled subject converted once. Retries preserve the original
// timestamp. Call only for the experiment's defined conversion within its window.
func (a *AB) Record(ctx context.Context, id, subject string) error {
	if err := validate.Subject(id, subject); err != nil {
		return err
	}
	return a.repo.Record(ctx, id, subject)
}

// Close freezes enrollment and conversions; repeated calls succeed. When used
// inside a caller transaction, closure takes effect on commit. Assignment and
// recording locks are retained until that transaction ends.
func (a *AB) Close(ctx context.Context, id string) error {
	if err := validate.ID("experiment", id); err != nil {
		return err
	}
	return a.repo.Close(ctx, id)
}
func (a *AB) Results(ctx context.Context, id string) (Results, error) {
	if err := validate.ID("experiment", id); err != nil {
		return Results{}, err
	}
	return a.repo.Results(ctx, id)
}

type Conclusion string

const (
	InsufficientData Conclusion = "insufficient_data"
	Inconclusive     Conclusion = "inconclusive"
	TreatmentBetter  Conclusion = "treatment_better"
	ControlBetter    Conclusion = "control_better"
)

type Interpretation struct {
	Conclusion    Conclusion
	Confidence    float64
	ControlRate   float64
	TreatmentRate float64
	Difference    float64  // Treatment minus control, in absolute proportion units.
	RelativeLift  *float64 // Nil when the control rate is zero.
	Lower         *float64 // Confidence limits absent when sample conditions are unmet.
	Upper         *float64
}

// Interpret computes a two-sided unpooled normal confidence interval for the
// difference in conversion rates. Zero confidence selects 0.95. Each arm requires
// at least ten conversions and ten nonconversions. Higher conversion is better.
// This fixed-horizon method assumes independent subjects, randomized enrollment,
// and a prespecified analysis time. Repeated peeking invalidates nominal coverage.
func Interpret(r Results, confidence float64) (Interpretation, error) {
	if confidence == 0 {
		confidence = .95
	}
	if math.IsNaN(confidence) || confidence <= 0 || confidence >= 1 {
		return Interpretation{}, fmt.Errorf("ab: confidence must be between zero and one")
	}
	out := Interpretation{Confidence: confidence, Conclusion: InsufficientData}
	for _, c := range []Counts{r.Control, r.Treatment} {
		if c.Subjects < 0 || c.Conversions < 0 || c.Conversions > c.Subjects {
			return Interpretation{}, fmt.Errorf("ab: invalid counts")
		}
	}
	if r.Control.Subjects > 0 {
		out.ControlRate = float64(r.Control.Conversions) / float64(r.Control.Subjects)
	}
	if r.Treatment.Subjects > 0 {
		out.TreatmentRate = float64(r.Treatment.Conversions) / float64(r.Treatment.Subjects)
	}
	out.Difference = out.TreatmentRate - out.ControlRate
	if out.ControlRate > 0 && r.Treatment.Subjects > 0 {
		lift := out.Difference / out.ControlRate
		out.RelativeLift = &lift
	}
	for _, c := range []Counts{r.Control, r.Treatment} {
		if c.Conversions < 10 || c.Subjects-c.Conversions < 10 {
			return out, nil
		}
	}
	se := math.Sqrt(out.ControlRate*(1-out.ControlRate)/float64(r.Control.Subjects) + out.TreatmentRate*(1-out.TreatmentRate)/float64(r.Treatment.Subjects))
	z := math.Sqrt2 * math.Erfcinv(1-confidence)
	lower, upper := max(-1, out.Difference-z*se), min(1, out.Difference+z*se)
	out.Lower, out.Upper = &lower, &upper
	out.Conclusion = Inconclusive
	if lower > 0 {
		out.Conclusion = TreatmentBetter
	} else if upper < 0 {
		out.Conclusion = ControlBetter
	}
	return out, nil
}
