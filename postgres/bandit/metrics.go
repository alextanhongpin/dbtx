package bandit

import (
	"fmt"
	"math"
	"sort"
)

type Posterior struct {
	Mean  float64
	Lower float64
	Upper float64
}

// CredibleInterval reports an equal-tailed 95% Beta(1,1) posterior interval.
// It is model-based uncertainty, not a frequentist confidence interval.
func CredibleInterval(s ArmStats) Posterior {
	a, b := float64(s.Successes+1), float64(s.Failures()+1)
	return Posterior{a / (a + b), betaQuantile(.025, a, b), betaQuantile(.975, a, b)}
}
func betaQuantile(p, a, b float64) float64 {
	lo, hi := 0., 1.
	for range 64 {
		mid := (lo + hi) / 2
		if betaCDF(mid, a, b) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}
func betaCDF(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	bt := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return bt * betaFraction(x, a, b) / a
	}
	return 1 - bt*betaFraction(1-x, b, a)/b
}
func betaFraction(x, a, b float64) float64 {
	const tiny = 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c := 1.
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 1000; m++ {
		mf := float64(m)
		m2 := 2 * mf
		aa := mf * (b - mf) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + mf) * (qab + mf) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		delta := d * c
		h *= delta
		if math.Abs(delta-1) < 3e-14 {
			break
		}
	}
	return h
}

type Point struct {
	Step       int64
	Reward     float64
	Rate       *float64
	Regret     *float64
	Allocation []float64
}

func Curve(s Summary, decisions []Decision) []Point {
	counts := make([]float64, len(s.Arms))
	indices := map[string]int{}
	for i, a := range s.Arms {
		indices[a.Arm.Key] = i
	}
	best := 0.
	for _, p := range s.Run.Probabilities {
		best = max(best, p)
	}
	var successes, observations, regret float64
	points := make([]Point, 0, len(decisions))
	for _, d := range decisions {
		i := indices[d.ArmKey]
		counts[i]++
		if d.Feedback != nil {
			observations++
			if d.Feedback.Success {
				successes++
			}
		}
		p := Point{Step: d.Sequence, Reward: successes, Allocation: make([]float64, len(counts))}
		for j, n := range counts {
			p.Allocation[j] = n / float64(d.Sequence)
		}
		if observations > 0 {
			v := successes / observations
			p.Rate = &v
		}
		if s.Run.Mode == "simulation" {
			regret += best - s.Run.Probabilities[i]
			v := regret
			p.Regret = &v
		}
		points = append(points, p)
	}
	return points
}

type Band struct {
	Mean  float64
	Lower float64
	Upper float64
}
type AggregatePoint struct {
	Step       int64
	Reward     Band
	Rate       Band
	Regret     Band
	Allocation []Band
}
type Comparison struct {
	Policy PolicyConfig
	Seeds  []uint64
	Points []AggregatePoint
}
type RunData struct {
	Summary   Summary
	Decisions []Decision
}

// Compare requires completed simulations from one experiment with identical
// probabilities, horizon, implementation version and distinct seed sets per policy.
func Compare(data []RunData) ([]Comparison, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: no runs to compare", ErrInvalid)
	}
	base := data[0].Summary
	groups := map[string][]RunData{}
	order := []string{}
	for _, d := range data {
		r := d.Summary.Run
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if len(d.Summary.Arms) != len(base.Arms) || len(d.Summary.Arms) != len(r.Probabilities) {
			return nil, fmt.Errorf("%w: mismatched arms", ErrInvalid)
		}
		keys := map[string]bool{}
		for i, a := range d.Summary.Arms {
			if a.Arm != base.Arms[i].Arm || keys[a.Arm.Key] {
				return nil, fmt.Errorf("%w: mismatched arms", ErrInvalid)
			}
			keys[a.Arm.Key] = true
		}
		for _, dec := range d.Decisions {
			if !keys[dec.ArmKey] {
				return nil, fmt.Errorf("%w: unknown decision arm", ErrInvalid)
			}
		}
		if r.Mode != "simulation" || r.Status != "closed" || r.ExperimentID != base.Run.ExperimentID || r.Horizon != base.Run.Horizon || r.Version != base.Run.Version || len(r.Probabilities) != len(base.Run.Probabilities) || len(d.Decisions) != r.Horizon {
			return nil, fmt.Errorf("%w: incompatible runs", ErrInvalid)
		}
		for i, p := range r.Probabilities {
			if p != base.Run.Probabilities[i] {
				return nil, fmt.Errorf("%w: mismatched scenario", ErrInvalid)
			}
		}
		key := r.Policy.Name
		if r.Policy.Epsilon != nil {
			key += fmt.Sprintf(":%.17g", *r.Policy.Epsilon)
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], d)
	}
	var reference []uint64
	result := []Comparison{}
	for _, key := range order {
		g := groups[key]
		sort.Slice(g, func(i, j int) bool { return g[i].Summary.Run.Seed < g[j].Summary.Run.Seed })
		seeds := make([]uint64, len(g))
		curves := make([][]Point, len(g))
		for i, d := range g {
			seeds[i] = d.Summary.Run.Seed
			if i > 0 && seeds[i] == seeds[i-1] {
				return nil, fmt.Errorf("%w: duplicate policy/seed", ErrInvalid)
			}
			for j, dec := range d.Decisions {
				if dec.Sequence != int64(j+1) || dec.Feedback == nil {
					return nil, fmt.Errorf("%w: incomplete ledger", ErrInvalid)
				}
			}
			curves[i] = Curve(d.Summary, d.Decisions)
		}
		if reference == nil {
			reference = seeds
		} else {
			if len(reference) != len(seeds) {
				return nil, fmt.Errorf("%w: different seed sets", ErrInvalid)
			}
			for i, s := range seeds {
				if s != reference[i] {
					return nil, fmt.Errorf("%w: different seed sets", ErrInvalid)
				}
			}
		}
		c := Comparison{Policy: g[0].Summary.Run.Policy, Seeds: seeds, Points: make([]AggregatePoint, base.Run.Horizon)}
		for step := range c.Points {
			reward, rate, regret := []float64{}, []float64{}, []float64{}
			alloc := make([][]float64, len(base.Arms))
			for _, curve := range curves {
				p := curve[step]
				reward = append(reward, p.Reward)
				rate = append(rate, *p.Rate)
				regret = append(regret, *p.Regret)
				for i, v := range p.Allocation {
					alloc[i] = append(alloc[i], v)
				}
			}
			p := AggregatePoint{Step: int64(step + 1), Reward: distribution(reward), Rate: distribution(rate), Regret: distribution(regret), Allocation: make([]Band, len(alloc))}
			for i, v := range alloc {
				p.Allocation[i] = distribution(v)
			}
			c.Points[step] = p
		}
		result = append(result, c)
	}
	return result, nil
}
func distribution(v []float64) Band {
	sort.Float64s(v)
	sum := 0.
	for _, x := range v {
		sum += x
	}
	return Band{sum / float64(len(v)), percentile(v, .1), percentile(v, .9)}
}
func percentile(v []float64, p float64) float64 {
	x := p * float64(len(v)-1)
	i := int(x)
	return v[i] + (v[min(i+1, len(v)-1)]-v[i])*(x-float64(i))
}
