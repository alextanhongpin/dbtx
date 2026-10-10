# ab

PostgreSQL storage for two-variant A/B experiments with a binary conversion metric.
Each experiment has immutable control and treatment names. Subjects receive stable
50/50 hash assignments, and each subject contributes at most one conversion.

```go
package example

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/alextanhongpin/dbtx/postgres/ab"
	"github.com/alextanhongpin/dbtx/postgres/ab/repository"
)

// runExperiment demonstrates the API lifecycle. In an application, exposure,
// conversion, and the planned analysis horizon occur at separate times.
func runExperiment(ctx context.Context, db *sql.DB) (ab.Interpretation, error) {
	// Apply repository.Schema through migrations before calling this function.
	experiments := ab.New(repository.New(db))
	if err := experiments.Create(ctx, ab.Experiment{ID: "checkout-v1", Control: "original", Treatment: "simplified"}); err != nil {
		return ab.Interpretation{}, err
	}
	assignment, err := experiments.Assign(ctx, "checkout-v1", "user-123")
	if err != nil {
		return ab.Interpretation{}, err
	}
	fmt.Printf("Show variant %s to user-123\n", assignment.Variant)
	// When this exposed subject converts:
	if err := experiments.Record(ctx, "checkout-v1", "user-123"); err != nil {
		return ab.Interpretation{}, err
	}
	// At the prespecified end of the experiment:
	if err := experiments.Close(ctx, "checkout-v1"); err != nil {
		return ab.Interpretation{}, err
	}
	results, err := experiments.Results(ctx, "checkout-v1")
	if err != nil {
		return ab.Interpretation{}, err
	}
	return ab.Interpret(results, 0) // Zero selects 95% confidence.
}
```

The lifecycle example is compiled and exercised by the [repository tests](repository/example_test.go).
Exposure, conversion, and closure happen at separate times in an application; this
one-subject example demonstrates the calls and returns `insufficient_data`.

## Persistence contracts

- Service and repository calls reject blank experiment and subject IDs, including
  whitespace-only IDs. Valid identities are stored exactly without normalization.
  Repository errors include the operation and preserve the underlying error for
  `errors.Is` and `errors.As`.
- `Create` returns `ErrExists` for an existing ID. Use a fresh ID to change variants
  or start another experiment. Creation timestamps are assigned by PostgreSQL.
- `Assign` stores enrollment and returns the stored assignment on retries. Use one
  stable subject ID per independent experimental unit. Do not assign on conversion
  alone: the denominator must include exposed subjects who did not convert.
- `Record` returns `ErrNotAssigned` before enrollment. It marks conversion once;
  retries preserve the first timestamp. Define the conversion and observation window
  in the application. No revenue, event counts, or multiple metrics are supported.
- `Close` freezes new assignments and conversions. Existing assignments remain
  readable; already recorded conversions can be retried after closure. Closure is
  idempotent and coordinates with writes through database row locks.
- `Results` aggregates both variants in one database snapshot. An active experiment's
  results are provisional. Shared locks allow different subjects to be recorded
  concurrently. PostgreSQL isolation/serialization errors are returned to callers.
- The repository joins the caller's `dbtx` transaction. Configure matching context
  IDs and use the same database for atomic business writes and conversion recording.
  Closure becomes visible to other transactions on commit. Assignment and recording
  hold their experiment locks until the caller's transaction ends. Avoid upgrading
  shared locks by closing an experiment inside transactions that also enroll or
  record subjects concurrently: PostgreSQL can detect a deadlock. Deadlock and
  serialization errors are returned without automatic retries; the caller must
  decide whether to retry the whole transaction.
  The constructor does not apply schema or close the pool. Schema requires two tables
  under `dbtx`; assignment identities are retained for the experiment's lifetime.

## Interpretation

`Interpret` returns conversion rates, treatment-minus-control absolute difference,
relative lift (nil for a zero control rate), and a two-sided confidence interval.
Positive lift favors treatment; higher conversion is assumed better. The conclusion
is `treatment_better`, `control_better`, or `inconclusive` according to whether the
interval lies above zero, below zero, or contains zero. Inconclusive does not establish
that the variants are equivalent.

The interval uses the [unpooled normal approximation documented by NIST](https://www.itl.nist.gov/div898/software/dataplot/refman1/auxillar/diffprop.htm).
Each arm must contain at least ten conversions and ten nonconversions; otherwise
`insufficient_data` is returned with no interval. Rates are in proportion units,
so 0.02 means two percentage points and relative lift 0.2 means twenty percent.

Plan sample size, exposure eligibility, and analysis time before starting. The
method assumes independent randomized subjects at a fixed analysis horizon. It
provides no sequential testing, sample-ratio mismatch detection, multiple-testing
adjustment, power analysis, or causal protection against biased enrollment. Do not
stop an experiment merely because a repeatedly checked interval excludes zero.

## Verification

Run `go test -race ./...` and `go vet ./...` from this module. Repository tests use
the shared Docker-backed `dbtest` utility with PostgreSQL and cover persistence,
exact per-variant counts, concurrent duplicate enrollment/recording, both closure
lock orderings, transaction rollback, validation, and error propagation.
