package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/bandit"
)

type reader struct {
	s bandit.Summary
	h []bandit.Decision
}

func (f reader) ListExperiments(context.Context, bandit.Page) ([]bandit.Experiment, error) {
	return []bandit.Experiment{f.s.Experiment}, nil
}
func (f reader) ListRuns(context.Context, uuid.UUID, bandit.Page) ([]bandit.Run, error) {
	return []bandit.Run{f.s.Run}, nil
}
func (f reader) Summary(context.Context, uuid.UUID) (bandit.Summary, error) { return f.s, nil }
func (f reader) History(_ context.Context, _ uuid.UUID, p bandit.Page) ([]bandit.Decision, error) {
	lo := min(p.Offset, len(f.h))
	hi := min(lo+p.Limit, len(f.h))
	return f.h[lo:hi], nil
}
func (f reader) Trajectory(context.Context, uuid.UUID) ([]bandit.Decision, error) { return f.h, nil }
func fixture() reader {
	r := bandit.Run{ID: uuid.NewV7(), ExperimentID: uuid.NewV7(), Mode: "live", Policy: bandit.PolicyConfig{Name: "thompson"}, Version: bandit.Version}
	return reader{s: bandit.Summary{Run: r, Experiment: bandit.Experiment{ID: r.ExperimentID, Name: "<script>alert(1)</script>", RewardDescription: "Click", Arms: []bandit.Arm{{Key: "=evil", Label: "A"}, {Key: "b", Label: "B"}}}, Arms: []bandit.ArmStats{{Arm: bandit.Arm{Key: "=evil", Label: "A"}, Selections: 1}, {Arm: bandit.Arm{Key: "b", Label: "B"}}}}, h: []bandit.Decision{{Sequence: 1, ArmKey: "=evil", RequestKey: "=formula", RunID: r.ID}}}
}
func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}
func TestPagesChartsAndExports(t *testing.T) {
	f := fixture()
	h := New(f)
	base := "/runs/" + f.s.Run.ID.String()
	for _, path := range []string{"/", "/experiments/" + f.s.Run.ExperimentID.String(), base, "/assets/charts.js", "/assets/style.css"} {
		w := get(t, h, path)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "<script>alert(1)</script>") {
			t.Fatal("unescaped name")
		}
	}
	w := get(t, h, base)
	body := w.Body.String()
	for _, want := range []string{"unavailable", "Pending", "95% credible", "Regret is unavailable", "View snapshot"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
	w = get(t, h, base+"/curve")
	var data struct{ Points []bandit.Point }
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Points[0].Rate != nil || data.Points[0].Regret != nil || data.Points[0].Reward != 0 {
		t.Fatal(data)
	}
	w = get(t, h, base+"/export.csv")
	if !strings.Contains(w.Body.String(), "'=evil") || !strings.Contains(w.Body.String(), "'=formula") {
		t.Fatal(w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal(w.Header())
	}
	w = get(t, h, base+"?limit=1")
	if !strings.Contains(w.Body.String(), "offset=1") {
		t.Fatal("missing next link")
	}
	w = get(t, h, base+"?offset=1&limit=1")
	if !strings.Contains(w.Body.String(), "No decisions on this page") || !strings.Contains(w.Body.String(), "Previous") {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{base + "?limit=201", base + "?offset=-1", "/runs/invalid", "/compare?run=" + f.s.Run.ID.String()} {
		if w := get(t, h, path); w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest("POST", base, nil))
	if post.Code != 405 {
		t.Fatal("write route exposed")
	}
}
func TestEmptyAndComparison(t *testing.T) {
	f := fixture()
	f.h = nil
	f.s.Arms[0].Selections = 0
	if w := get(t, New(f), "/runs/"+f.s.Run.ID.String()); w.Code != 200 || !strings.Contains(w.Body.String(), "No decisions") {
		t.Fatal(w.Code, w.Body.String())
	}
	f.s.Run.Mode = "simulation"
	f.s.Run.Status = "closed"
	f.s.Run.Horizon = 1
	f.s.Run.Probabilities = []float64{.2, .6}
	f.h = []bandit.Decision{{Sequence: 1, ArmKey: "=evil", Feedback: &bandit.Feedback{Success: true}}}
	path := "/compare?run=" + f.s.Run.ID.String()
	w := get(t, New(f), path)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "10th–90th") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = get(t, New(f), path+"&format=json")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Regret":{"Mean":0.399`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := get(t, New(f), path+"&run="+f.s.Run.ID.String()); w.Code != 400 {
		t.Fatal("duplicate run accepted")
	}
}
