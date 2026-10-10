package bandit

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"uuid"
)

func TestPolicyContracts(t *testing.T) {
	stats := []ArmStats{{Arm: Arm{Key: "a"}, Selections: 10, Observations: 10, Successes: 8}, {Arm: Arm{Key: "b"}, Selections: 20, Observations: 20, Successes: 2}}
	zero := 0.
	p, err := NewPolicy(PolicyConfig{Name: "epsilon-greedy", Epsilon: &zero})
	if err != nil {
		t.Fatal(err)
	}
	choice, err := p.Choose(stats, DecisionRandom(1, 1, 0))
	if err != nil || choice.Index != 0 {
		t.Fatalf("greedy: %+v %v", choice, err)
	}
	p, _ = NewPolicy(PolicyConfig{Name: "ucb1"})
	choice, _ = p.Choose(stats, DecisionRandom(1, 1, 0))
	want := .8 + math.Sqrt(2*math.Log(30)/10)
	if math.Abs(*choice.Scores[0].Value-want) > 1e-12 {
		t.Fatal(choice)
	}
	for _, name := range []string{"epsilon-greedy", "ucb1"} {
		p, _ = NewPolicy(PolicyConfig{Name: name})
		pending := append([]ArmStats(nil), stats...)
		pending[1].Observations = 0
		pending[1].Successes = 0
		choice, _ = p.Choose(pending, DecisionRandom(1, 1, 0))
		if choice.Index != 1 || choice.Scores[1].Value != nil {
			t.Fatal("unobserved arm must initialize", choice)
		}
	}
	for _, eps := range []float64{-1, 1.1, math.NaN()} {
		if _, err := NewPolicy(PolicyConfig{Name: "epsilon-greedy", Epsilon: &eps}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := NewPolicy(PolicyConfig{Name: "unknown"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if *(PolicyConfig{Name: "epsilon-greedy"}.WithDefaults().Epsilon) != .1 {
		t.Fatal("default epsilon")
	}
	if *(PolicyConfig{Name: "epsilon-greedy", Epsilon: &zero}.WithDefaults().Epsilon) != 0 {
		t.Fatal("explicit zero")
	}
	for _, name := range Policies {
		p, _ = NewPolicy(PolicyConfig{Name: name})
		a, _ := p.Choose(stats, DecisionRandom(42, 3, 0))
		b, _ := p.Choose(stats, DecisionRandom(42, 3, 0))
		if !reflect.DeepEqual(a, b) {
			t.Fatal("not reproducible")
		}
	}
	if stats[0].Selections != 10 {
		t.Fatal("mutated input")
	}
}
func TestRandomTiesAndExploration(t *testing.T) {
	stats := []ArmStats{{Arm: Arm{Key: "a"}, Selections: 2, Observations: 2, Successes: 1}, {Arm: Arm{Key: "b"}, Selections: 2, Observations: 2, Successes: 1}}
	for _, eps := range []float64{0, 1} {
		p, _ := NewPolicy(PolicyConfig{Name: "epsilon-greedy", Epsilon: &eps})
		counts := [2]int{}
		for i := 1; i <= 1000; i++ {
			c, _ := p.Choose(stats, DecisionRandom(1, int64(i), 0))
			counts[c.Index]++
		}
		if counts[0] < 400 || counts[1] < 400 {
			t.Fatal("biased ties/exploration", counts)
		}
	}
	p, _ := NewPolicy(PolicyConfig{Name: "thompson"})
	stats[0].Successes = 2
	stats[1].Successes = 0
	n := 0
	for i := range 1000 {
		c, _ := p.Choose(stats, DecisionRandom(9, int64(i), 0))
		if c.Index == 0 {
			n++
		}
	}
	if n < 850 {
		t.Fatal("posterior not favoring successful arm", n)
	}
}
func TestCredibleIntervals(t *testing.T) {
	p := CredibleInterval(ArmStats{})
	if math.Abs(p.Mean-.5) > 1e-12 || math.Abs(p.Lower-.025) > 1e-10 || math.Abs(p.Upper-.975) > 1e-10 {
		t.Fatal(p)
	}
	p = CredibleInterval(ArmStats{Selections: 10, Observations: 10, Successes: 10})
	if math.Abs(p.Mean-11.0/12) > 1e-12 || math.Abs(p.Lower-math.Pow(.025, 1.0/11)) > 1e-10 {
		t.Fatal(p)
	}
	p = CredibleInterval(ArmStats{Selections: 10000, Observations: 10000, Successes: 5000})
	if !(p.Lower < .5 && p.Upper > .5 && p.Upper-p.Lower < .03) {
		t.Fatal(p)
	}
}
func simulation(t *testing.T, name string, seed uint64) Simulation {
	t.Helper()
	probs, _ := Scenario("clear-winner")
	s, err := Simulate(t.Context(), Run{ExperimentID: uuid.NewV7(), Policy: PolicyConfig{Name: name}, Seed: seed, Probabilities: probs, Horizon: 500}, []Arm{{"a", "A"}, {"b", "B"}, {"c", "C"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestSimulationReproducibilityAndRewardStreams(t *testing.T) {
	a := simulation(t, "uniform", 42)
	b := simulation(t, "uniform", 42)
	c := simulation(t, "ucb1", 42)
	for i, d := range a.Decisions {
		e := b.Decisions[i]
		if d.ArmKey != e.ArmKey || d.Feedback.Success != e.Feedback.Success || !reflect.DeepEqual(d.Scores, e.Scores) {
			t.Fatal("not reproducible")
		}
	}
	rewards := func(s Simulation) map[string][]bool {
		m := map[string][]bool{}
		for _, d := range s.Decisions {
			m[d.ArmKey] = append(m[d.ArmKey], d.Feedback.Success)
		}
		return m
	}
	ar, cr := rewards(a), rewards(c)
	for key, v := range ar {
		n := min(len(v), len(cr[key]))
		if !reflect.DeepEqual(v[:n], cr[key][:n]) {
			t.Fatal("reward stream depends on policy")
		}
	}
	for _, s := range []Simulation{a, b, c} {
		var n int64
		for _, arm := range s.Arms {
			n += arm.Observations
			if arm.Pending() != 0 {
				t.Fatal("pending simulation feedback")
			}
		}
		if n != 500 {
			t.Fatal(n)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Simulate(ctx, a.Run, []Arm{{"a", "A"}, {"b", "B"}, {"c", "C"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestMetricsAndComparison(t *testing.T) {
	s := simulation(t, "uniform", 1)
	s.Run.Probabilities = []float64{.5, .5, .5}
	summary := Summary{Run: s.Run, Arms: s.Arms}
	curve := Curve(summary, s.Decisions)
	if *curve[len(curve)-1].Regret != 0 {
		t.Fatal("equal-arm regret")
	}
	data := []RunData{{summary, s.Decisions}}
	cmp, err := Compare(data)
	if err != nil || len(cmp[0].Points) != 500 {
		t.Fatal(cmp, err)
	}
	b := simulation(t, "ucb1", 2)
	b.Run.ExperimentID = s.Run.ExperimentID
	b.Run.Probabilities = s.Run.Probabilities
	data = append(data, RunData{Summary{Run: b.Run, Arms: b.Arms}, b.Decisions})
	if _, err := Compare(data); !errors.Is(err, ErrInvalid) {
		t.Fatal("different seeds accepted", err)
	}
	live := summary
	live.Run.Mode = "live"
	if Curve(live, s.Decisions)[0].Regret != nil {
		t.Fatal("live regret")
	}
	d := s.Decisions[0]
	d.Feedback = nil
	if Curve(live, []Decision{d})[0].Rate != nil {
		t.Fatal("unobserved rate")
	}
	for _, probs := range [][]float64{{.1}, {.1, math.NaN()}, {.1, 2}} {
		if !errors.Is(ValidateScenario(probs, 1), ErrInvalid) {
			t.Fatal(probs)
		}
	}
}

func TestComparisonCompatibilityAndBands(t *testing.T) {
	s := simulation(t, "uniform", 1)
	other := simulation(t, "thompson", 1)
	other.Run.ExperimentID = s.Run.ExperimentID
	data := []RunData{{Summary{Run: s.Run, Arms: s.Arms}, s.Decisions}, {Summary{Run: other.Run, Arms: other.Arms}, other.Decisions}}
	c, err := Compare(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 2 || c[0].Points[499].Reward.Mean != c[0].Points[499].Reward.Lower {
		t.Fatal("one repetition must have zero-width band")
	}
	for _, change := range []func(*RunData){func(d *RunData) { d.Summary.Run.Horizon++ }, func(d *RunData) { d.Summary.Run.Probabilities = []float64{.1, .3, .7} }, func(d *RunData) { d.Summary.Run.Version = "future" }, func(d *RunData) {
		d.Summary.Arms = append([]ArmStats(nil), d.Summary.Arms...)
		d.Summary.Arms[0].Arm.Key = "other"
	}} {
		copyData := append([]RunData(nil), data...)
		change(&copyData[1])
		if _, err := Compare(copyData); !errors.Is(err, ErrInvalid) {
			t.Fatal("incompatible comparison accepted", err)
		}
	}
	band := distribution([]float64{0, 10, 20})
	if band.Mean != 10 || band.Lower != 2 || band.Upper != 18 {
		t.Fatal(band)
	}
	if _, err := New(Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if !(Page{Limit: 1, Offset: 2147483648}.Validate() != nil) {
		t.Fatal("overflowing page offset")
	}
}
