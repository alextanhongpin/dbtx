package bandit

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
)

type PolicyConfig struct {
	Name    string
	Epsilon *float64
}

func (c PolicyConfig) WithDefaults() PolicyConfig {
	if c.Name == "" {
		c.Name = "thompson"
	}
	if c.Name == "epsilon-greedy" && c.Epsilon == nil {
		v := 0.1
		c.Epsilon = &v
	} else if c.Epsilon != nil {
		v := *c.Epsilon
		c.Epsilon = &v
	}
	return c
}
func (c PolicyConfig) Validate() error {
	switch c.Name {
	case "uniform", "ucb1", "thompson":
		if c.Epsilon != nil {
			return fmt.Errorf("%w: epsilon applies only to epsilon-greedy", ErrInvalid)
		}
	case "epsilon-greedy":
		if c.Epsilon == nil || math.IsNaN(*c.Epsilon) || *c.Epsilon < 0 || *c.Epsilon > 1 {
			return fmt.Errorf("%w: epsilon must be in [0,1]", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown policy %q", ErrInvalid, c.Name)
	}
	return nil
}

type Choice struct {
	Index  int
	Scores []Score
}

// Policy consumes completed observations only and does not mutate the input.
// The caller owns the random source and must not share it concurrently.
type Policy interface {
	Choose([]ArmStats, *rand.Rand) (Choice, error)
}
type bernoulliPolicy struct{ cfg PolicyConfig }

func NewPolicy(c PolicyConfig) (Policy, error) {
	c = c.WithDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return bernoulliPolicy{c}, nil
}

// DecisionRandom uses SHA-256 of a domain tag and big-endian seed/sequence/stream
// to initialize PCG. Version records this scheme; streams isolate rewards.
func DecisionRandom(seed uint64, sequence int64, stream uint64) *rand.Rand {
	b := make([]byte, 24)
	binary.BigEndian.PutUint64(b, seed)
	binary.BigEndian.PutUint64(b[8:], uint64(sequence))
	binary.BigEndian.PutUint64(b[16:], stream)
	h := sha256.Sum256(append([]byte(Version), b...))
	return rand.New(rand.NewPCG(binary.BigEndian.Uint64(h[:8]), binary.BigEndian.Uint64(h[8:16])))
}
func (p bernoulliPolicy) Choose(stats []ArmStats, rng *rand.Rand) (Choice, error) {
	if len(stats) == 0 || rng == nil {
		return Choice{}, fmt.Errorf("%w: arms and random source required", ErrInvalid)
	}
	var total int64
	unseen := []int{}
	scores := make([]Score, len(stats))
	for i, s := range stats {
		if s.Selections < 0 || s.Observations < 0 || s.Observations > s.Selections || s.Successes < 0 || s.Successes > s.Observations {
			return Choice{}, fmt.Errorf("%w: invalid arm statistics", ErrInvalid)
		}
		total += s.Observations
		if s.Observations == 0 {
			unseen = append(unseen, i)
		}
		scores[i].ArmKey = s.Arm.Key
	}
	if (p.cfg.Name == "epsilon-greedy" || p.cfg.Name == "ucb1") && len(unseen) > 0 {
		for i, s := range stats {
			if s.Observations == 0 {
				scores[i].Reason = "unobserved"
			} else {
				v := float64(s.Successes) / float64(s.Observations)
				scores[i].Value = &v
				scores[i].Reason = "observed mean"
			}
		}
		return Choice{unseen[rng.IntN(len(unseen))], scores}, nil
	}
	for i, s := range stats {
		v := 0.0
		switch p.cfg.Name {
		case "uniform":
			v = 1 / float64(len(stats))
			scores[i].Reason = "selection probability"
		case "epsilon-greedy":
			v = float64(s.Successes) / float64(s.Observations)
			scores[i].Reason = "observed mean"
		case "ucb1":
			v = float64(s.Successes)/float64(s.Observations) + math.Sqrt(2*math.Log(float64(total))/float64(s.Observations))
			scores[i].Reason = "UCB index"
		case "thompson":
			a := gamma(float64(1+s.Successes), rng)
			b := gamma(float64(1+s.Failures()), rng)
			v = a / (a + b)
			scores[i].Reason = "posterior draw"
		}
		scores[i].Value = &v
	}
	if p.cfg.Name == "uniform" {
		return Choice{rng.IntN(len(stats)), scores}, nil
	}
	if p.cfg.Name == "epsilon-greedy" && rng.Float64() < *p.cfg.Epsilon {
		return Choice{rng.IntN(len(stats)), scores}, nil
	}
	best := math.Inf(-1)
	ties := []int{}
	for i, s := range scores {
		if *s.Value > best {
			best = *s.Value
			ties = []int{i}
		} else if *s.Value == best {
			ties = append(ties, i)
		}
	}
	return Choice{ties[rng.IntN(len(ties))], scores}, nil
}

// Marsaglia-Tsang gamma sampler; Bernoulli posterior shapes are always >= 1.
func gamma(shape float64, r *rand.Rand) float64 {
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := r.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
