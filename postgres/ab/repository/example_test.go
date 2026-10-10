package repository_test

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

func Example() {
	// This pool is owned and closed by the application.
	db, err := sql.Open("postgres", "postgres://john:123456@127.0.0.1:5432/dev?sslmode=disable")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	summary, err := runExperiment(context.Background(), db)
	if err != nil {
		panic(err)
	}
	fmt.Println(summary.Conclusion)
}
