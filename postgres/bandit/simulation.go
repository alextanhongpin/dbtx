package bandit

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
	"uuid"
)

var Policies = []string{"uniform", "epsilon-greedy", "ucb1", "thompson"}

// Scenario returns a fresh slice, so callers cannot alter shipped scenarios.
func Scenario(name string) ([]float64, error) {
	switch name {
	case "clear-winner":
		return []float64{.1, .3, .6}, nil
	case "close-call":
		return []float64{.45, .50, .55}, nil
	case "equal-arm":
		return []float64{.5, .5, .5}, nil
	}
	return nil, fmt.Errorf("%w: unknown scenario", ErrInvalid)
}
func ValidateScenario(probabilities []float64, horizon int) error {
	if len(probabilities) < 2 || len(probabilities) > 100 || horizon < 1 || horizon > 1000000 {
		return fmt.Errorf("%w: simulation requires 2..100 arms and horizon 1..1000000", ErrInvalid)
	}
	for _, p := range probabilities {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return fmt.Errorf("%w: probabilities must be in [0,1]", ErrInvalid)
		}
	}
	return nil
}

type Simulation struct {
	Run       Run
	Decisions []Decision
	Arms      []ArmStats
}

// Simulate returns an in-memory repetition with immediate feedback. Each arm's
// reward stream depends only on the repetition seed and arm position, not policy.
func Simulate(ctx context.Context, run Run, arms []Arm) (Simulation, error) {
	run.Mode = "simulation"
	run.Policy = run.Policy.WithDefaults()
	if err := run.Validate(); err != nil {
		return Simulation{}, err
	}
	if len(arms) != len(run.Probabilities) {
		return Simulation{}, fmt.Errorf("%w: arm/probability mismatch", ErrInvalid)
	}
	policy, _ := NewPolicy(run.Policy)
	if err := (Experiment{Name: "Simulation", RewardDescription: "Binary reward", Arms: arms}).Validate(); err != nil {
		return Simulation{}, err
	}
	stats := make([]ArmStats, len(arms))
	rewards := make([]*rand.Rand, len(arms))
	for i, a := range arms {
		stats[i].Arm = a
		rewards[i] = DecisionRandom(run.Seed, 0, uint64(i+1))
	}
	run.ID = uuid.NewV7()
	run.Version = Version
	run.Status = "closed"
	run.Sequence = int64(run.Horizon)
	run.CreatedAt = time.Now().UTC()
	decisions := make([]Decision, 0, run.Horizon)
	for step := 1; step <= run.Horizon; step++ {
		if err := ctx.Err(); err != nil {
			return Simulation{}, err
		}
		choice, err := policy.Choose(stats, DecisionRandom(run.Seed, int64(step), 0))
		if err != nil {
			return Simulation{}, err
		}
		success := rewards[choice.Index].Float64() < run.Probabilities[choice.Index]
		now := time.Now().UTC()
		decisions = append(decisions, Decision{ID: uuid.NewV7(), RunID: run.ID, Sequence: int64(step), RequestKey: fmt.Sprintf("step:%d", step), ArmKey: arms[choice.Index].Key, Statistics: append([]ArmStats(nil), stats...), Scores: choice.Scores, CreatedAt: now, Feedback: &Feedback{Success: success, ObservedAt: now}})
		stats[choice.Index].Selections++
		stats[choice.Index].Observations++
		if success {
			stats[choice.Index].Successes++
		}
	}
	now := time.Now().UTC()
	run.ClosedAt = &now
	return Simulation{run, decisions, stats}, nil
}

// SaveSimulation computes one complete repetition before writing anything.
// Repository persistence is atomic and joins a caller-owned transaction.
func (b *Bandit) SaveSimulation(ctx context.Context, run Run, arms []Arm) (Run, error) {
	s, err := Simulate(ctx, run, arms)
	if err != nil {
		return Run{}, err
	}
	return b.repo.SaveSimulation(ctx, s.Run, s.Decisions, s.Arms)
}
