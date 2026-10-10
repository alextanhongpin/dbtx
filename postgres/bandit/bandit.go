package bandit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

var (
	ErrNotExist = errors.New("bandit: not exist")
	ErrClosed   = errors.New("bandit: run closed")
	ErrConflict = errors.New("bandit: conflicting feedback")
	ErrInvalid  = errors.New("bandit: invalid input")
)

const Version = "bernoulli-v1-pcg-sha256"

type Arm struct {
	Key   string
	Label string
}
type Experiment struct {
	ID                uuid.UUID
	Name              string
	RewardDescription string
	Arms              []Arm
	CreatedAt         time.Time
}
type Run struct {
	ID            uuid.UUID
	ExperimentID  uuid.UUID
	Mode          string
	Policy        PolicyConfig
	Version       string
	Seed          uint64
	Sequence      int64
	Status        string
	Probabilities []float64
	Horizon       int
	CreatedAt     time.Time
	ClosedAt      *time.Time
}
type ArmStats struct {
	Arm          Arm
	Selections   int64
	Observations int64
	Successes    int64
}

func (s ArmStats) Failures() int64 { return s.Observations - s.Successes }
func (s ArmStats) Pending() int64  { return s.Selections - s.Observations }
func (s ArmStats) Rate() *float64 {
	if s.Observations == 0 {
		return nil
	}
	v := float64(s.Successes) / float64(s.Observations)
	return &v
}

type Score struct {
	ArmKey string
	Value  *float64
	Reason string
}
type Decision struct {
	ID         uuid.UUID
	RunID      uuid.UUID
	Sequence   int64
	RequestKey string
	ArmKey     string
	Statistics []ArmStats
	Scores     []Score
	CreatedAt  time.Time
	Feedback   *Feedback
}
type Feedback struct {
	Success    bool
	ObservedAt time.Time
}
type Summary struct {
	Experiment Experiment
	Run        Run
	Arms       []ArmStats
}

func (s Summary) Observations() int64 {
	var n int64
	for _, a := range s.Arms {
		n += a.Observations
	}
	return n
}
func (s Summary) Pending() int64 {
	var n int64
	for _, a := range s.Arms {
		n += a.Pending()
	}
	return n
}
func (s Summary) FeedbackRate() *float64 {
	total := s.Observations() + s.Pending()
	if total == 0 {
		return nil
	}
	v := float64(s.Observations()) / float64(total)
	return &v
}

type Page struct {
	Limit  int
	Offset int
}

func (p Page) WithDefaults() Page {
	if p.Limit == 0 {
		p.Limit = 50
	}
	return p
}
func (p Page) Validate() error {
	if p.Limit < 1 || p.Limit > 200 || p.Offset < 0 || p.Offset > 2147483647 {
		return fmt.Errorf("%w: page limit must be 1..200 and offset nonnegative", ErrInvalid)
	}
	return nil
}

// Repository owns transactions and serializes writes to each run. Select invokes
// the pure built-in policy while holding the run lock. All operations must join
// caller-owned transactions. Reads return independently owned values.
type Repository interface {
	CreateExperiment(context.Context, Experiment) (Experiment, error)
	StartRun(context.Context, Run) (Run, error)
	Select(context.Context, uuid.UUID, string) (Decision, error)
	Observe(context.Context, uuid.UUID, bool) error
	CloseRun(context.Context, uuid.UUID) error
	ListExperiments(context.Context, Page) ([]Experiment, error)
	ListRuns(context.Context, uuid.UUID, Page) ([]Run, error)
	Summary(context.Context, uuid.UUID) (Summary, error)
	History(context.Context, uuid.UUID, Page) ([]Decision, error)
	Trajectory(context.Context, uuid.UUID) ([]Decision, error)
	SaveSimulation(context.Context, Run, []Decision, []ArmStats) (Run, error)
}
type Config struct{ Repository Repository }

// Bandit is concurrent-safe when its repository is. It owns no resources.
type Bandit struct{ repo Repository }

func New(c Config) (*Bandit, error) {
	if c.Repository == nil {
		return nil, fmt.Errorf("%w: repository required", ErrInvalid)
	}
	return &Bandit{repo: c.Repository}, nil
}
func validText(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func (e Experiment) Validate() error {
	if !validText(e.Name) || !validText(e.RewardDescription) || len(e.Arms) < 2 || len(e.Arms) > 100 {
		return fmt.Errorf("%w: name, reward description and 2..100 arms required", ErrInvalid)
	}
	keys := map[string]bool{}
	for _, a := range e.Arms {
		if !validText(a.Key) || !validText(a.Label) || keys[a.Key] {
			return fmt.Errorf("%w: distinct nonempty arm keys and labels required", ErrInvalid)
		}
		keys[a.Key] = true
	}
	return nil
}
func (r Run) Validate() error {
	if r.ExperimentID == (uuid.UUID{}) {
		return fmt.Errorf("%w: experiment ID required", ErrInvalid)
	}
	if r.Mode != "live" && r.Mode != "simulation" {
		return fmt.Errorf("%w: mode must be live or simulation", ErrInvalid)
	}
	if err := r.Policy.Validate(); err != nil {
		return err
	}
	if r.Mode == "live" && (r.Horizon != 0 || len(r.Probabilities) != 0) {
		return fmt.Errorf("%w: live runs cannot have simulated probabilities", ErrInvalid)
	}
	if r.Mode == "simulation" {
		return ValidateScenario(r.Probabilities, r.Horizon)
	}
	return nil
}
func (b *Bandit) CreateExperiment(ctx context.Context, e Experiment) (Experiment, error) {
	if err := e.Validate(); err != nil {
		return Experiment{}, err
	}
	e.Arms = append([]Arm(nil), e.Arms...)
	return b.repo.CreateExperiment(ctx, e)
}
func (b *Bandit) StartRun(ctx context.Context, r Run) (Run, error) {
	if r.Mode == "" {
		r.Mode = "live"
	}
	r.Policy = r.Policy.WithDefaults()
	if err := r.Validate(); err != nil {
		return Run{}, err
	}
	if r.Mode != "live" {
		return Run{}, fmt.Errorf("%w: use SaveSimulation for simulations", ErrInvalid)
	}
	return b.repo.StartRun(ctx, r)
}
func (b *Bandit) Select(ctx context.Context, id uuid.UUID, key string) (Decision, error) {
	if !validText(key) || len(key) > 1024 {
		return Decision{}, fmt.Errorf("%w: request key must be nonempty and at most 1024 bytes", ErrInvalid)
	}
	return b.repo.Select(ctx, id, key)
}
func (b *Bandit) Observe(ctx context.Context, id uuid.UUID, success bool) error {
	return b.repo.Observe(ctx, id, success)
}
func (b *Bandit) CloseRun(ctx context.Context, id uuid.UUID) error { return b.repo.CloseRun(ctx, id) }
func (b *Bandit) ListExperiments(ctx context.Context, p Page) ([]Experiment, error) {
	p = p.WithDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return b.repo.ListExperiments(ctx, p)
}
func (b *Bandit) ListRuns(ctx context.Context, id uuid.UUID, p Page) ([]Run, error) {
	p = p.WithDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return b.repo.ListRuns(ctx, id, p)
}
func (b *Bandit) Summary(ctx context.Context, id uuid.UUID) (Summary, error) {
	return b.repo.Summary(ctx, id)
}
func (b *Bandit) History(ctx context.Context, id uuid.UUID, p Page) ([]Decision, error) {
	p = p.WithDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return b.repo.History(ctx, id, p)
}

// Trajectory returns the ordered ledger without selection diagnostics, for
// analysis and exports. Each run is limited to one million decisions.
func (b *Bandit) Trajectory(ctx context.Context, id uuid.UUID) ([]Decision, error) {
	return b.repo.Trajectory(ctx, id)
}
