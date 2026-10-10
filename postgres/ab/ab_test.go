package ab_test

import (
	"context"
	"github.com/alextanhongpin/dbtx/postgres/ab"
	"math"
	"testing"
)

func TestInterpret(t *testing.T) {
	for _, tc := range []struct {
		name string
		c, b ab.Counts
		want ab.Conclusion
	}{
		{"empty", ab.Counts{}, ab.Counts{}, ab.InsufficientData},
		{"extreme", ab.Counts{100, 0}, ab.Counts{100, 100}, ab.InsufficientData},
		{"equal", ab.Counts{1000, 100}, ab.Counts{1000, 100}, ab.Inconclusive},
		{"better", ab.Counts{1000, 100}, ab.Counts{1000, 200}, ab.TreatmentBetter},
		{"worse", ab.Counts{1000, 200}, ab.Counts{1000, 100}, ab.ControlBetter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ab.Interpret(ab.Results{Control: tc.c, Treatment: tc.b}, 0)
			if err != nil || out.Conclusion != tc.want {
				t.Fatalf("%+v %v", out, err)
			}
			if tc.want == ab.InsufficientData && out.Lower != nil {
				t.Fatal("unexpected interval")
			}
			if tc.name == "better" {
				margin := 1.959963984540054 * math.Sqrt(.00025)
				if math.Abs(*out.Lower-(.1-margin)) > 1e-12 || math.Abs(*out.Upper-(.1+margin)) > 1e-12 || math.Abs(*out.RelativeLift-1) > 1e-12 {
					t.Fatalf("unexpected interval: %+v", out)
				}
			}
		})
	}
	for _, c := range []float64{-1, 1, math.NaN(), math.Inf(1)} {
		if _, err := ab.Interpret(ab.Results{}, c); err == nil {
			t.Fatal("expected invalid confidence")
		}
	}
	for _, c := range []ab.Counts{{-1, 0}, {1, -1}, {1, 2}} {
		if _, err := ab.Interpret(ab.Results{Control: c}, 0); err == nil {
			t.Fatal("expected invalid counts")
		}
	}
}
func TestValidate(t *testing.T) {
	for _, e := range []ab.Experiment{{}, {ID: "x", Control: "a", Treatment: "a"}, {ID: " ", Control: "a", Treatment: "b"}} {
		if e.Validate() == nil {
			t.Fatal("expected error")
		}
	}
	if err := (ab.Experiment{ID: "x", Control: "a", Treatment: "b"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// assignmentRepository captures choices without coupling deterministic assignment
// fixtures to persistence or reproducing the production hash algorithm.
type assignmentRepository struct {
	ab.Repository
	experiment ab.Experiment
}

func (r *assignmentRepository) Load(context.Context, string) (ab.Experiment, error) {
	return r.experiment, nil
}
func (r *assignmentRepository) Assign(_ context.Context, id, subject, variant string) (ab.Assignment, error) {
	return ab.Assignment{ExperimentID: id, SubjectID: subject, Variant: variant}, nil
}

func TestAssignmentFixtures(t *testing.T) {
	for _, tc := range []struct{ experiment, subject, want string }{
		{"checkout", "alice", "control"},
		{"checkout-v2", "alice", "treatment"},
		{"ab", "c", "control"},
		{"a", "bc", "treatment"},
		{" checkout ", " alice ", "control"},
	} {
		t.Run(tc.experiment+"/"+tc.subject, func(t *testing.T) {
			svc := ab.New(&assignmentRepository{experiment: ab.Experiment{ID: tc.experiment, Control: "control", Treatment: "treatment"}})
			a, err := svc.Assign(t.Context(), tc.experiment, tc.subject)
			if err != nil || a.Variant != tc.want || a.SubjectID != tc.subject || a.ExperimentID != tc.experiment {
				t.Fatalf("assignment: %+v %v", a, err)
			}
		})
	}
}

func TestInterpretSampleConditions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		c, b     ab.Counts
		interval bool
	}{
		{"exact boundary", ab.Counts{20, 10}, ab.Counts{20, 10}, true},
		{"nine successes", ab.Counts{20, 9}, ab.Counts{20, 10}, false},
		{"nine failures", ab.Counts{20, 11}, ab.Counts{20, 10}, false},
		{"treatment too small", ab.Counts{100, 50}, ab.Counts{19, 10}, false},
		{"no control", ab.Counts{}, ab.Counts{100, 20}, false},
		{"no treatment", ab.Counts{100, 20}, ab.Counts{}, false},
		{"zero control rate", ab.Counts{100, 0}, ab.Counts{100, 20}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ab.Interpret(ab.Results{Control: tc.c, Treatment: tc.b}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if (out.Lower != nil) != tc.interval || (out.Upper != nil) != tc.interval {
				t.Fatalf("interval availability: %+v", out)
			}
			if !tc.interval && out.Conclusion != ab.InsufficientData {
				t.Fatal(out.Conclusion)
			}
			if (tc.c.Conversions == 0 || tc.b.Subjects == 0) && out.RelativeLift != nil {
				t.Fatal("undefined lift")
			}
		})
	}
}

func TestInterpretUnequalSamplesAndConfidence(t *testing.T) {
	r := ab.Results{Control: ab.Counts{Subjects: 100, Conversions: 20}, Treatment: ab.Counts{Subjects: 400, Conversions: 120}}
	out, err := ab.Interpret(r, .95)
	if err != nil {
		t.Fatal(err)
	}
	// Independent reference: pB-pA=0.1, variance=0.2*0.8/100+0.3*0.7/400.
	margin := 1.959963984540054 * math.Sqrt(.002125)
	if math.Abs(out.ControlRate-.2) > 1e-12 || math.Abs(out.TreatmentRate-.3) > 1e-12 || math.Abs(out.Difference-.1) > 1e-12 || math.Abs(*out.Lower-(.1-margin)) > 1e-12 || math.Abs(*out.Upper-(.1+margin)) > 1e-12 || math.Abs(*out.RelativeLift-.5) > 1e-12 {
		t.Fatalf("unexpected unequal sample result: %+v", out)
	}
	wider, err := ab.Interpret(r, .99)
	if err != nil {
		t.Fatal(err)
	}
	if *wider.Lower >= *out.Lower || *wider.Upper <= *out.Upper {
		t.Fatal("higher confidence did not widen interval")
	}
	for _, counts := range []ab.Counts{{Subjects: -1}, {Subjects: 1, Conversions: -1}, {Subjects: 1, Conversions: 2}} {
		if _, err := ab.Interpret(ab.Results{Control: ab.Counts{20, 10}, Treatment: counts}, .95); err == nil {
			t.Fatal("invalid treatment counts")
		}
	}
	for _, c := range []float64{math.Inf(-1), .000001, .999999} {
		result, err := ab.Interpret(r, c)
		if math.IsInf(c, -1) {
			if err == nil {
				t.Fatal("accepted infinite confidence")
			}
			continue
		}
		if err != nil || result.Lower == nil || math.IsNaN(*result.Lower) || math.IsNaN(*result.Upper) {
			t.Fatalf("valid extreme confidence: %+v %v", result, err)
		}
	}
}
