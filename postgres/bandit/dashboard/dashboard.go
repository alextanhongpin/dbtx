// Package dashboard provides a read-only, local bandit results browser.
package dashboard

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/bandit"
)

//go:embed templates/*.html assets/*
var files embed.FS

type Reader interface {
	ListExperiments(context.Context, bandit.Page) ([]bandit.Experiment, error)
	ListRuns(context.Context, uuid.UUID, bandit.Page) ([]bandit.Run, error)
	Summary(context.Context, uuid.UUID) (bandit.Summary, error)
	History(context.Context, uuid.UUID, bandit.Page) ([]bandit.Decision, error)
	Trajectory(context.Context, uuid.UUID) ([]bandit.Decision, error)
}
type dashboard struct {
	reader    Reader
	templates *template.Template
}
type page struct {
	Title       string
	Experiments []bandit.Experiment
	Runs        []bandit.Run
	Summary     *bandit.Summary
	History     []bandit.Decision
	Previous    string
	Next        string
	Limit       int
	ChartURL    string
	Comparisons []bandit.Comparison
}

func New(r Reader) http.Handler {
	d := &dashboard{reader: r, templates: template.Must(template.New("page.html").Funcs(template.FuncMap{
		"rate": func(v *float64) string {
			if v == nil {
				return "unavailable"
			}
			return fmt.Sprintf("%.2f%%", 100**v)
		},
		"posterior": func(s bandit.ArmStats) string {
			p := bandit.CredibleInterval(s)
			return fmt.Sprintf("%.2f%% [%.2f%%, %.2f%%]", 100*p.Mean, 100*p.Lower, 100*p.Upper)
		},
		"json": func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) },
		"epsilon": func(c bandit.PolicyConfig) string {
			if c.Epsilon == nil {
				return ""
			}
			return fmt.Sprintf(" (epsilon %.3g)", *c.Epsilon)
		},
	}).ParseFS(files, "templates/*.html"))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.home)
	mux.HandleFunc("GET /experiments/{id}", d.experiment)
	mux.HandleFunc("GET /runs/{id}", d.run)
	mux.HandleFunc("GET /runs/{id}/curve", d.curve)
	mux.HandleFunc("GET /runs/{id}/export.csv", d.export)
	mux.HandleFunc("GET /compare", d.compare)
	mux.Handle("GET /assets/", http.FileServerFS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
func id(r *http.Request) (uuid.UUID, error) {
	v, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return v, bandit.ErrInvalid
	}
	return v, nil
}
func pagination(r *http.Request) (bandit.Page, error) {
	p := bandit.Page{Limit: 50}
	var err error
	if s := r.URL.Query().Get("offset"); s != "" {
		p.Offset, err = strconv.Atoi(s)
		if err != nil {
			return p, bandit.ErrInvalid
		}
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		p.Limit, err = strconv.Atoi(s)
		if err != nil {
			return p, bandit.ErrInvalid
		}
	}
	return p, p.Validate()
}
func links(r *http.Request, p bandit.Page, n int) (string, string) {
	q := r.URL.Query()
	q.Set("limit", strconv.Itoa(p.Limit))
	prev, next := "", ""
	if p.Offset > 0 {
		q.Set("offset", strconv.Itoa(max(0, p.Offset-p.Limit)))
		prev = r.URL.Path + "?" + q.Encode()
	}
	if n == p.Limit {
		q.Set("offset", strconv.Itoa(p.Offset+p.Limit))
		next = r.URL.Path + "?" + q.Encode()
	}
	return prev, next
}
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bandit.ErrInvalid):
		http.Error(w, err.Error(), 400)
	case errors.Is(err, bandit.ErrNotExist):
		http.Error(w, "Run or experiment not found", 404)
	default:
		http.Error(w, "Unable to read results", 500)
	}
}
func (d *dashboard) render(w http.ResponseWriter, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.templates.ExecuteTemplate(w, "page.html", p); err != nil {
		return
	}
}
func (d *dashboard) home(w http.ResponseWriter, r *http.Request) {
	p, err := pagination(r)
	if err != nil {
		fail(w, err)
		return
	}
	e, err := d.reader.ListExperiments(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	prev, next := links(r, p, len(e))
	d.render(w, page{Title: "Bandit experiments", Experiments: e, Previous: prev, Next: next})
}
func (d *dashboard) experiment(w http.ResponseWriter, r *http.Request) {
	i, err := id(r)
	if err != nil {
		fail(w, err)
		return
	}
	p, err := pagination(r)
	if err != nil {
		fail(w, err)
		return
	}
	runs, err := d.reader.ListRuns(r.Context(), i, p)
	if err != nil {
		fail(w, err)
		return
	}
	prev, next := links(r, p, len(runs))
	d.render(w, page{Title: "Experiment runs", Runs: runs, Previous: prev, Next: next, Limit: p.Limit})
}
func (d *dashboard) run(w http.ResponseWriter, r *http.Request) {
	i, err := id(r)
	if err != nil {
		fail(w, err)
		return
	}
	p, err := pagination(r)
	if err != nil {
		fail(w, err)
		return
	}
	s, err := d.reader.Summary(r.Context(), i)
	if err != nil {
		fail(w, err)
		return
	}
	h, err := d.reader.History(r.Context(), i, p)
	if err != nil {
		fail(w, err)
		return
	}
	prev, next := links(r, p, len(h))
	d.render(w, page{Title: s.Experiment.Name + " / " + s.Run.Policy.Name, Summary: &s, History: h, Previous: prev, Next: next, ChartURL: "/runs/" + i.String() + "/curve"})
}
func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func sample[T any](v []T) []T {
	if len(v) <= 500 {
		return v
	}
	out := make([]T, 500)
	for i := range out {
		out[i] = v[i*(len(v)-1)/(len(out)-1)]
	}
	return out
}
func (d *dashboard) curve(w http.ResponseWriter, r *http.Request) {
	i, err := id(r)
	if err != nil {
		fail(w, err)
		return
	}
	s, err := d.reader.Summary(r.Context(), i)
	if err != nil {
		fail(w, err)
		return
	}
	h, err := d.reader.Trajectory(r.Context(), i)
	if err != nil {
		fail(w, err)
		return
	}
	respondJSON(w, struct {
		Arms   []bandit.ArmStats
		Points []bandit.Point
	}{s.Arms, sample(bandit.Curve(s, h))})
}
func (d *dashboard) export(w http.ResponseWriter, r *http.Request) {
	i, err := id(r)
	if err != nil {
		fail(w, err)
		return
	}
	s, err := d.reader.Summary(r.Context(), i)
	if err != nil {
		fail(w, err)
		return
	}
	h, err := d.reader.Trajectory(r.Context(), i)
	if err != nil {
		fail(w, err)
		return
	}
	curve := bandit.Curve(s, h)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="bandit-`+i.String()+`.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"run_id", "policy", "seed", "sequence", "arm", "request_key", "success", "selected_at", "observed_at", "cumulative_reward", "observed_reward_rate", "cumulative_pseudo_regret"})
	for j, dec := range h {
		success, observed, rate, regret := "", "", "", ""
		if dec.Feedback != nil {
			success = strconv.FormatBool(dec.Feedback.Success)
			observed = dec.Feedback.ObservedAt.Format("2006-01-02T15:04:05.999999999Z07:00")
		}
		if curve[j].Rate != nil {
			rate = strconv.FormatFloat(*curve[j].Rate, 'g', -1, 64)
		}
		if curve[j].Regret != nil {
			regret = strconv.FormatFloat(*curve[j].Regret, 'g', -1, 64)
		}
		_ = cw.Write([]string{i.String(), s.Run.Policy.Name, strconv.FormatUint(s.Run.Seed, 10), strconv.FormatInt(dec.Sequence, 10), csvText(dec.ArmKey), csvText(dec.RequestKey), success, dec.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"), observed, strconv.FormatFloat(curve[j].Reward, 'g', -1, 64), rate, regret})
	}
}

// Neutralize spreadsheet formulas in user-supplied CSV cells.
func csvText(s string) string {
	trim := strings.TrimLeft(s, " \t\r\n")
	if trim != "" && strings.ContainsRune("=+-@", rune(trim[0])) {
		return "'" + s
	}
	return s
}
func (d *dashboard) compare(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["run"]
	if len(ids) < 1 || len(ids) > 200 {
		fail(w, fmt.Errorf("%w: select 1..200 runs", bandit.ErrInvalid))
		return
	}
	data := make([]bandit.RunData, len(ids))
	seen := map[uuid.UUID]bool{}
	for j, s := range ids {
		i, err := uuid.Parse(s)
		if err != nil || seen[i] {
			fail(w, bandit.ErrInvalid)
			return
		}
		seen[i] = true
		summary, err := d.reader.Summary(r.Context(), i)
		if err != nil {
			fail(w, err)
			return
		}
		if summary.Run.Mode != "simulation" || summary.Run.Status != "closed" {
			fail(w, fmt.Errorf("%w: select completed simulations", bandit.ErrInvalid))
			return
		}
		h, err := d.reader.Trajectory(r.Context(), i)
		if err != nil {
			fail(w, err)
			return
		}
		data[j] = bandit.RunData{Summary: summary, Decisions: h}
	}
	cmp, err := bandit.Compare(data)
	if err != nil {
		fail(w, err)
		return
	}
	for i := range cmp {
		cmp[i].Points = sample(cmp[i].Points)
	}
	if r.URL.Query().Get("format") == "json" {
		respondJSON(w, struct {
			Arms        []bandit.ArmStats
			Comparisons []bandit.Comparison
		}{data[0].Summary.Arms, cmp})
		return
	}
	q := r.URL.Query()
	q.Set("format", "json")
	d.render(w, page{Title: "Simulation comparison", Comparisons: cmp, ChartURL: "/compare?" + q.Encode()})
}
