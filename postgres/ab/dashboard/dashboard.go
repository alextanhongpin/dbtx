// Package dashboard provides an embedded, read-only A/B results dashboard.
// Mount behind the application's access controls when serving private results.
package dashboard

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/alextanhongpin/dbtx/postgres/ab"
)

//go:embed page.html assets/*
var files embed.FS

// Reader retrieves a consistent experiment snapshot. Both ab.AB and the
// PostgreSQL repository implement this interface.
type Reader interface {
	Results(context.Context, string) (ab.Results, error)
}

type page struct {
	ID, Error, Conclusion, Explanation string
	Results                            *ab.Results
	Summary                            ab.Interpretation
	HasControl, HasTreatment           bool
	Low, High, Zero, Point             float64
}

// New returns a concurrent-safe read-only handler. Confidence defaults to 95%;
// users may select 90%, 95%, or 99%. The reader retains ownership of its resources.
func New(reader Reader) http.Handler {
	tmpl := template.Must(template.New("page.html").Funcs(template.FuncMap{
		"bar":    func(v float64) string { return strconv.FormatFloat(600*v, 'f', 4, 64) },
		"pct":    func(v float64) string { return fmt.Sprintf("%.2f%%", 100*v) },
		"pp":     func(v float64) string { return fmt.Sprintf("%+.2f pp", 100*v) },
		"number": func(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) },
		"optionalPct": func(v *float64) string {
			if v == nil {
				return "Unavailable"
			}
			return fmt.Sprintf("%+.2f%%", 100**v)
		},
		"optionalPP": func(v *float64) string {
			if v == nil {
				return "Unavailable"
			}
			return fmt.Sprintf("%+.2f pp", 100**v)
		},
	}).ParseFS(files, "page.html"))
	assets, _ := fs.Sub(files, "assets")
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		p := page{ID: r.URL.Query().Get("experiment")}
		status := http.StatusOK
		confidence := .95
		if raw := r.URL.Query().Get("confidence"); raw != "" {
			switch raw {
			case "90":
				confidence = .90
			case "95":
			case "99":
				confidence = .99
			default:
				p.Error = "Choose a confidence level of 90%, 95%, or 99%."
				status = 400
			}
		}
		p.Summary.Confidence = confidence
		if _, provided := r.URL.Query()["experiment"]; provided && strings.TrimSpace(p.ID) == "" {
			p.Error = "Enter an experiment ID."
			status = 400
		}
		if p.ID != "" && status == 200 {
			results, err := reader.Results(r.Context(), p.ID)
			if err != nil {
				status = 500
				p.Error = "Unable to load results. Try again."
				if errors.Is(err, ab.ErrNotExist) {
					status = 404
					p.Error = "Experiment not found. Check its ID."
				}
			} else {
				summary, err := ab.Interpret(results, confidence)
				if err != nil {
					status = 500
					p.Error = "These results could not be interpreted."
				} else {
					p.Results = &results
					p.Summary = summary
					p.HasControl = results.Control.Subjects > 0
					p.HasTreatment = results.Treatment.Subjects > 0
					switch summary.Conclusion {
					case ab.TreatmentBetter:
						p.Conclusion = "Treatment leads"
						p.Explanation = "The confidence interval is above zero, favoring the treatment's conversion rate."
					case ab.ControlBetter:
						p.Conclusion = "Control leads"
						p.Explanation = "The confidence interval is below zero, favoring the control's conversion rate."
					case ab.Inconclusive:
						p.Conclusion = "No clear difference"
						p.Explanation = "The confidence interval includes zero. This does not establish that the variants are equivalent."
					default:
						p.Conclusion = "More data needed"
						p.Explanation = "Each variant needs at least 10 conversions and 10 nonconversions before an interval can be calculated."
					}
					if summary.Lower != nil {
						bound := math.Max(math.Abs(*summary.Lower), math.Abs(*summary.Upper)) * 1.2
						if bound == 0 {
							bound = .01
						}
						position := func(v float64) float64 { return 50 + v/bound*40 }
						p.Low = position(*summary.Lower)
						p.High = position(*summary.Upper)
						p.Point = position(summary.Difference)
						p.Zero = 50
					}
				}
			}
		}
		var b bytes.Buffer
		if err := tmpl.Execute(&b, p); err != nil {
			http.Error(w, "Unable to display results", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(b.Bytes())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}
