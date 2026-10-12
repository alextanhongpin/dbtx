package dashboard_test

import (
	"context"
	"errors"
	"html"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alextanhongpin/dbtx/postgres/ab"
	"github.com/alextanhongpin/dbtx/postgres/ab/dashboard"
)

type readerFunc func(context.Context, string) (ab.Results, error)

func (f readerFunc) Results(ctx context.Context, id string) (ab.Results, error) { return f(ctx, id) }

func TestDashboard(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		counts    ab.Results
		err       error
		status    int
		want      string
	}{
		{name: "home", url: "/", status: 200, want: "Start with an experiment"},
		{name: "better", url: "/?experiment=x", counts: ab.Results{Control: ab.Counts{Subjects: 1000, Conversions: 100}, Treatment: ab.Counts{Subjects: 1000, Conversions: 200}}, status: 200, want: "Treatment leads"},
		{name: "worse", url: "/?experiment=x", counts: ab.Results{Control: ab.Counts{Subjects: 1000, Conversions: 200}, Treatment: ab.Counts{Subjects: 1000, Conversions: 100}}, status: 200, want: "Control leads"},
		{name: "equal", url: "/?experiment=x&confidence=99", counts: ab.Results{Control: ab.Counts{Subjects: 1000, Conversions: 100}, Treatment: ab.Counts{Subjects: 1000, Conversions: 100}}, status: 200, want: "No clear difference"},
		{name: "empty", url: "/?experiment=x", status: 200, want: "More data needed"},
		{name: "invalid confidence", url: "/?experiment=x&confidence=nan", status: 400, want: "Choose a confidence"},
		{name: "blank ID", url: "/?experiment=+", status: 400, want: "Enter an experiment"},
		{name: "missing", url: "/?experiment=x", err: ab.ErrNotExist, status: 404, want: "Experiment not found"},
		{name: "backend failure", url: "/?experiment=x", err: errors.New("private DSN secret"), status: 500, want: "Unable to load results"},
		{name: "invalid counts", url: "/?experiment=x", counts: ab.Results{Control: ab.Counts{Subjects: 1, Conversions: 2}}, status: 500, want: "could not be interpreted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := dashboard.New(readerFunc(func(ctx context.Context, id string) (ab.Results, error) {
				if id != "x" {
					t.Fatalf("unexpected id %q", id)
				}
				tc.counts.Experiment = ab.Experiment{ID: "<script>alert(1)</script>", Control: "Original", Treatment: "New"}
				return tc.counts, tc.err
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", tc.url, nil))
			body := html.UnescapeString(w.Body.String())
			if w.Code != tc.status || !strings.Contains(body, tc.want) {
				t.Fatalf("status %d, body %s", w.Code, body)
			}
			if strings.Contains(w.Body.String(), "<script>") || strings.Contains(body, "private DSN secret") {
				t.Fatal("unescaped data or leaked error")
			}
			if tc.name == "empty" && (strings.Contains(body, "<circle") || !strings.Contains(body, "Unavailable")) {
				t.Fatal("empty result falsely plotted")
			}
			if tc.name == "better" && (!strings.Contains(body, "+100.00%") || !strings.Contains(body, "+10.00 pp") || !strings.Contains(body, "Active · provisional")) {
				t.Fatal("wrong metrics or missing provisional label")
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing headers")
			}
		})
	}
}
func TestRoutes(t *testing.T) {
	h := dashboard.New(readerFunc(func(context.Context, string) (ab.Results, error) {
		t.Fatal("unexpected database read")
		return ab.Results{}, nil
	}))
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"GET", "/assets/style.css", 200}, {"POST", "/", 405}, {"GET", "/missing", 404}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}
