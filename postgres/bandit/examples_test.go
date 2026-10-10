package bandit_test

import (
	"context"
	"database/sql"
	"github.com/alextanhongpin/dbtx/postgres/bandit"
	"github.com/alextanhongpin/dbtx/postgres/bandit/repository"
)

func ExampleBandit_Select() {
	var db *sql.DB // Application-owned PostgreSQL pool with schema applied.
	repo := repository.New(db)
	b, _ := bandit.New(bandit.Config{Repository: repo})
	ctx := context.Background()
	experiment, err := b.CreateExperiment(ctx, bandit.Experiment{Name: "Message subject", RewardDescription: "User clicked within the application's observation window", Arms: []bandit.Arm{{Key: "short", Label: "Short subject"}, {Key: "descriptive", Label: "Descriptive subject"}}})
	if err != nil {
		return
	}
	run, err := b.StartRun(ctx, bandit.Run{ExperimentID: experiment.ID, Policy: bandit.PolicyConfig{Name: "thompson"}, Seed: 42})
	if err != nil {
		return
	}
	decision, err := b.Select(ctx, run.ID, "delivery:123")
	if err != nil {
		return
	}
	_ = decision.ArmKey                   // Use the chosen subject; persist decision.ID for feedback.
	_ = b.Observe(ctx, decision.ID, true) // A repeated identical outcome is a no-op.
	_ = b.CloseRun(ctx, run.ID)           // Late feedback is still accepted.
}
