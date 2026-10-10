package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/bandit"
	"github.com/alextanhongpin/dbtx/postgres/bandit/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
)

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtest.Options{Image: "postgres:19beta3-alpine3.24", Hook: func(dsn string) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.Exec(repository.Schema)
		return err
	}})
	code := m.Run()
	_ = stop()
	os.Exit(code)
}
func setup(t *testing.T) (*bandit.Bandit, *repository.Repository, bandit.Experiment, bandit.Run) {
	t.Helper()
	repo := repository.New(dbtest.DB(t))
	b, err := bandit.New(bandit.Config{Repository: repo})
	check(t, err)
	e, err := b.CreateExperiment(t.Context(), bandit.Experiment{Name: "Test", RewardDescription: "Success", Arms: []bandit.Arm{{Key: "a", Label: "A"}, {Key: "b", Label: "B"}}})
	check(t, err)
	r, err := b.StartRun(t.Context(), bandit.Run{ExperimentID: e.ID, Seed: 42})
	check(t, err)
	return b, repo, e, r
}
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func invariant(t *testing.T, b *bandit.Bandit, id uuid.UUID) {
	t.Helper()
	s, err := b.Summary(t.Context(), id)
	check(t, err)
	h, err := b.Trajectory(t.Context(), id)
	check(t, err)
	counts := map[string]bandit.ArmStats{}
	for _, a := range s.Arms {
		counts[a.Arm.Key] = bandit.ArmStats{Arm: a.Arm}
	}
	for i, d := range h {
		if d.Sequence != int64(i+1) {
			t.Fatal("sequence gap")
		}
		a := counts[d.ArmKey]
		a.Selections++
		if d.Feedback != nil {
			a.Observations++
			if d.Feedback.Success {
				a.Successes++
			}
		}
		counts[d.ArmKey] = a
	}
	if s.Run.Sequence != int64(len(h)) {
		t.Fatal("run sequence disagrees with ledger")
	}
	for _, a := range s.Arms {
		if counts[a.Arm.Key] != a {
			t.Fatalf("ledger disagrees: %+v %+v", counts[a.Arm.Key], a)
		}
	}
}
func TestLiveLifecycle(t *testing.T) {
	b, _, _, r := setup(t)
	d, err := b.Select(t.Context(), r.ID, "request")
	check(t, err)
	if len(d.Statistics) != 2 || len(d.Scores) != 2 {
		t.Fatal("missing diagnostics")
	}
	again, err := b.Select(t.Context(), r.ID, "request")
	check(t, err)
	if d.ID != again.ID {
		t.Fatal("duplicate selection")
	}
	s, err := b.Summary(t.Context(), r.ID)
	check(t, err)
	if s.Run.Sequence != 1 {
		t.Fatal(s)
	}
	check(t, b.CloseRun(t.Context(), r.ID))
	check(t, b.CloseRun(t.Context(), r.ID))
	_, err = b.Select(t.Context(), r.ID, "new")
	if !errors.Is(err, bandit.ErrClosed) {
		t.Fatal(err)
	}
	again, err = b.Select(t.Context(), r.ID, "request")
	check(t, err)
	if again.ID != d.ID {
		t.Fatal("closed retry")
	}
	check(t, b.Observe(t.Context(), d.ID, true))
	check(t, b.Observe(t.Context(), d.ID, true))
	if !errors.Is(b.Observe(t.Context(), d.ID, false), bandit.ErrConflict) {
		t.Fatal("conflicting feedback accepted")
	}
	h, err := b.History(t.Context(), r.ID, bandit.Page{})
	check(t, err)
	if h[0].Feedback == nil || !h[0].Feedback.Success || h[0].Feedback.ObservedAt.IsZero() {
		t.Fatal(h)
	}
	invariant(t, b, r.ID)
}
func TestConcurrentSelectionAndFeedback(t *testing.T) {
	b, _, _, r := setup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 50 {
		wg.Go(func() {
			d, err := b.Select(t.Context(), r.ID, fmt.Sprintf("key-%d", i%25))
			if err == nil {
				err = b.Observe(t.Context(), d.ID, i%2 == 0)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	// Different outcomes for duplicate keys may conflict; each key still has
	// exactly one decision and at most one reward update.
	for err := range errs {
		if err != nil && !errors.Is(err, bandit.ErrConflict) {
			t.Fatal(err)
		}
	}
	s, err := b.Summary(t.Context(), r.ID)
	check(t, err)
	if s.Run.Sequence != 25 {
		t.Fatal(s.Run.Sequence)
	}
	invariant(t, b, r.ID)
	d, err := b.Select(t.Context(), r.ID, "same-feedback")
	check(t, err)
	errs = make(chan error, 30)
	for range 30 {
		wg.Go(func() { errs <- b.Observe(t.Context(), d.ID, true) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		check(t, err)
	}
	invariant(t, b, r.ID)
}
func TestTransactionsAndCancellation(t *testing.T) {
	b, repo, e, r := setup(t)
	rollback := errors.New("rollback")
	err := repo.RunInTx(t.Context(), func(ctx context.Context) error {
		d, err := b.Select(ctx, r.ID, "rollback")
		if err != nil {
			return err
		}
		if err := b.Observe(ctx, d.ID, true); err != nil {
			return err
		}
		if err := b.CloseRun(ctx, r.ID); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	s, err := b.Summary(t.Context(), r.ID)
	check(t, err)
	if s.Run.Sequence != 0 || s.Run.Status != "open" {
		t.Fatal(s)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = b.Select(ctx, r.ID, "cancel")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	err = repo.RunInTx(t.Context(), func(ctx context.Context) error {
		_, err := b.SaveSimulation(ctx, bandit.Run{ExperimentID: e.ID, Seed: 1, Policy: bandit.PolicyConfig{Name: "ucb1"}, Probabilities: []float64{.1, .8}, Horizon: 20}, e.Arms)
		if err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	runs, err := b.ListRuns(t.Context(), e.ID, bandit.Page{})
	check(t, err)
	if len(runs) != 1 {
		t.Fatal("simulation escaped caller transaction")
	}
	invariant(t, b, r.ID)
}
func TestSimulationPersistenceAndPagination(t *testing.T) {
	b, repo, e, _ := setup(t)
	saved, err := b.SaveSimulation(t.Context(), bandit.Run{ExperimentID: e.ID, Seed: 3, Policy: bandit.PolicyConfig{Name: "thompson"}, Probabilities: []float64{.2, .7}, Horizon: 100}, e.Arms)
	check(t, err)
	if saved.Sequence != 100 || saved.Status != "closed" {
		t.Fatal(saved)
	}
	invariant(t, b, saved.ID)
	h, err := b.History(t.Context(), saved.ID, bandit.Page{Limit: 10, Offset: 10})
	check(t, err)
	if len(h) != 10 || h[0].Sequence != 11 {
		t.Fatal(h)
	}
	fresh, _ := bandit.New(bandit.Config{Repository: repository.New(repo.Unwrap())})
	s, err := fresh.Summary(t.Context(), saved.ID)
	check(t, err)
	if s.Run.Policy.Name != "thompson" || s.Run.Version != bandit.Version || s.Run.Seed != 3 {
		t.Fatal("not recovered", s)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = b.SaveSimulation(ctx, bandit.Run{ExperimentID: e.ID, Seed: 4, Probabilities: []float64{.2, .7}, Horizon: 100}, e.Arms)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	runs, err := b.ListRuns(t.Context(), e.ID, bandit.Page{})
	check(t, err)
	if len(runs) != 2 {
		t.Fatal("partial repetition persisted")
	}
}
func TestMissingAndInvalid(t *testing.T) {
	b, _, _, _ := setup(t)
	id := uuid.NewV7()
	_, err := b.Summary(t.Context(), id)
	if !errors.Is(err, bandit.ErrNotExist) {
		t.Fatal(err)
	}
	_, err = b.Select(t.Context(), id, "x")
	if !errors.Is(err, bandit.ErrNotExist) {
		t.Fatal(err)
	}
	if !errors.Is(b.Observe(t.Context(), id, true), bandit.ErrNotExist) {
		t.Fatal("missing feedback decision")
	}
	if !errors.Is(b.CloseRun(t.Context(), id), bandit.ErrNotExist) {
		t.Fatal("missing close")
	}
	_, err = b.ListRuns(t.Context(), id, bandit.Page{})
	if !errors.Is(err, bandit.ErrNotExist) {
		t.Fatal(err)
	}
	_, err = b.History(t.Context(), id, bandit.Page{})
	if !errors.Is(err, bandit.ErrNotExist) {
		t.Fatal(err)
	}
	_, err = b.CreateExperiment(t.Context(), bandit.Experiment{})
	if !errors.Is(err, bandit.ErrInvalid) {
		t.Fatal(err)
	}
}
func TestStorageOwnershipConstraints(t *testing.T) {
	b, repo, _, r := setup(t)
	_, _, other, _ := setup(t)
	_, err := repo.Unwrap().ExecContext(t.Context(), `INSERT INTO dbtx.bandit_statistics(run_id,experiment_id,arm_key) VALUES($1,$2,'a')`, r.ID, other.ID)
	if err == nil {
		t.Fatal("cross-experiment arm accepted")
	}
	_, err = repo.Unwrap().ExecContext(t.Context(), `UPDATE dbtx.bandit_statistics SET observations=1 WHERE run_id=$1`, r.ID)
	if err == nil {
		t.Fatal("impossible counts accepted")
	}
	invariant(t, b, r.ID)
}

func TestIndependentRunLocks(t *testing.T) {
	b, repo, e, r := setup(t)
	other, err := b.StartRun(t.Context(), bandit.Run{ExperimentID: e.ID, Seed: 43})
	check(t, err)
	check(t, repo.RunInTx(t.Context(), func(ctx context.Context) error {
		if _, err := b.Select(ctx, r.ID, "held-lock"); err != nil {
			return err
		}
		independent, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := b.Select(independent, other.ID, "other-run"); done <- err }()
		return <-done
	}))
	invariant(t, b, r.ID)
	invariant(t, b, other.ID)
}

func TestCreateAndStartJoinTransaction(t *testing.T) {
	b, repo, _, _ := setup(t)
	rollback := errors.New("rollback")
	var id uuid.UUID
	err := repo.RunInTx(t.Context(), func(ctx context.Context) error {
		e, err := b.CreateExperiment(ctx, bandit.Experiment{Name: "Rolled back", RewardDescription: "Success", Arms: []bandit.Arm{{Key: "a", Label: "A"}, {Key: "b", Label: "B"}}})
		if err != nil {
			return err
		}
		r, err := b.StartRun(ctx, bandit.Run{ExperimentID: e.ID})
		if err != nil {
			return err
		}
		id = r.ID
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	_, err = b.Summary(t.Context(), id)
	if !errors.Is(err, bandit.ErrNotExist) {
		t.Fatal("run escaped rollback", err)
	}
}
