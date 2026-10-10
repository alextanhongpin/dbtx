package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/ab"
	"github.com/alextanhongpin/dbtx/postgres/ab/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/lib/pq"
)

func TestRepository(t *testing.T) {
	client := dbtest.New(t, dbtest.Options{Image: "postgres:19beta3-alpine3.24", Hook: func(dsn string) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.Exec(repository.Schema)
		return err
	}})
	repo := repository.New(client.DB(t))
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *repository.Repository)
	}{
		{"usage example", testUsageExample}, {"results", testResults}, {"missing", testMissing}, {"validation", testValidation},
		{"sticky", testSticky}, {"concurrent duplicates", testDuplicates}, {"rollback", testRollback},
		{"closure races", testClosureRaces}, {"errors", testErrors},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, repo) })
	}
}

func create(t *testing.T, r *repository.Repository) string {
	t.Helper()
	id := t.Name()
	must(t, r.Create(t.Context(), ab.Experiment{ID: id, Control: "control", Treatment: "treatment"}))
	return id
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wantError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func testResults(t *testing.T, r *repository.Repository) {
	ctx := t.Context()
	id := create(t, r)
	empty, err := r.Results(ctx, id)
	must(t, err)
	if empty.Control != (ab.Counts{}) || empty.Treatment != (ab.Counts{}) || empty.Experiment.ID != id {
		t.Fatalf("empty results: %+v", empty)
	}
	for _, a := range []struct {
		s, v    string
		convert bool
	}{{"a", "control", true}, {"b", "control", false}, {"c", "treatment", true}, {"d", "treatment", true}, {"e", "treatment", false}} {
		_, err := r.Assign(ctx, id, a.s, a.v)
		must(t, err)
		if a.convert {
			must(t, r.Record(ctx, id, a.s))
		}
	}
	out, err := r.Results(ctx, id)
	must(t, err)
	if out.Control != (ab.Counts{Subjects: 2, Conversions: 1}) || out.Treatment != (ab.Counts{Subjects: 3, Conversions: 2}) {
		t.Fatalf("counts: %+v", out)
	}
	loaded, err := r.Load(ctx, id)
	must(t, err)
	if loaded.CreatedAt.IsZero() || !loaded.CreatedAt.Equal(out.Experiment.CreatedAt) {
		t.Fatal("creation timestamp missing or changed")
	}
	wantError(t, r.Create(ctx, ab.Experiment{ID: id, Control: "other", Treatment: "new"}), ab.ErrExists)
	loaded, err = r.Load(ctx, id)
	must(t, err)
	if loaded.Control != "control" || loaded.Treatment != "treatment" {
		t.Fatal("duplicate create changed variants")
	}
}

func testMissing(t *testing.T, r *repository.Repository) {
	ctx := t.Context()
	id := create(t, r)
	_, err := r.Load(ctx, "missing")
	wantError(t, err, ab.ErrNotExist)
	_, err = r.Results(ctx, "missing")
	wantError(t, err, ab.ErrNotExist)
	_, err = r.Assign(ctx, "missing", "subject", "control")
	wantError(t, err, ab.ErrNotExist)
	wantError(t, r.Record(ctx, "missing", "subject"), ab.ErrNotExist)
	wantError(t, r.Close(ctx, "missing"), ab.ErrNotExist)
	wantError(t, r.Record(ctx, id, "subject"), ab.ErrNotAssigned)
}

func testValidation(t *testing.T, r *repository.Repository) {
	ctx := t.Context()
	svc := ab.New(r)
	for _, blank := range []string{"", " \t\n", "\u2003"} {
		for _, api := range []interface {
			Create(context.Context, ab.Experiment) error
			Load(context.Context, string) (ab.Experiment, error)
			Results(context.Context, string) (ab.Results, error)
			Close(context.Context, string) error
			Record(context.Context, string, string) error
		}{r, svc} {
			if err := api.Create(ctx, ab.Experiment{ID: blank, Control: "a", Treatment: "b"}); err == nil {
				t.Fatal("blank create")
			}
			if _, err := api.Load(ctx, blank); err == nil {
				t.Fatal("blank load")
			}
			if _, err := api.Results(ctx, blank); err == nil {
				t.Fatal("blank results")
			}
			if err := api.Close(ctx, blank); err == nil {
				t.Fatal("blank close")
			}
			if err := api.Record(ctx, "valid", blank); err == nil {
				t.Fatal("blank subject")
			}
			if err := api.Record(ctx, blank, "valid"); err == nil {
				t.Fatal("blank experiment")
			}
		}
		if _, err := svc.Assign(ctx, "valid", blank); err == nil {
			t.Fatal("blank service subject")
		}
		if _, err := svc.Assign(ctx, blank, "valid"); err == nil {
			t.Fatal("blank service experiment")
		}
		if _, err := r.Assign(ctx, "valid", blank, "control"); err == nil {
			t.Fatal("blank repository subject")
		}
		if _, err := r.Assign(ctx, blank, "valid", "control"); err == nil {
			t.Fatal("blank repository experiment")
		}
	}
	id := " " + t.Name() + " "
	must(t, svc.Create(ctx, ab.Experiment{ID: id, Control: "control", Treatment: "treatment"}))
	a, err := svc.Assign(ctx, id, " subject ")
	must(t, err)
	if a.ExperimentID != id || a.SubjectID != " subject " {
		t.Fatal("identities normalized")
	}
	_, err = svc.Load(ctx, strings.TrimSpace(id))
	wantError(t, err, ab.ErrNotExist)
	if _, err = r.Assign(ctx, id, "new", "unknown"); err == nil {
		t.Fatal("unknown variant accepted")
	}
}

func testSticky(t *testing.T, r *repository.Repository) {
	ctx := t.Context()
	id := create(t, r)
	first, err := r.Assign(ctx, id, "subject", "control")
	must(t, err)
	again, err := r.Assign(ctx, id, "subject", "unknown")
	must(t, err)
	if first.Variant != again.Variant || !first.AssignedAt.Equal(again.AssignedAt) {
		t.Fatal("assignment changed")
	}
	must(t, r.Record(ctx, id, "subject"))
	converted, err := r.Assign(ctx, id, "subject", "treatment")
	must(t, err)
	must(t, r.Record(ctx, id, "subject"))
	retry, err := r.Assign(ctx, id, "subject", "control")
	must(t, err)
	if converted.ConvertedAt == nil || retry.ConvertedAt == nil || !converted.ConvertedAt.Equal(*retry.ConvertedAt) {
		t.Fatal("conversion timestamp changed")
	}
	_, err = r.Assign(ctx, id, "unconverted", "treatment")
	must(t, err)
	must(t, r.Close(ctx, id))
	closed, err := r.Load(ctx, id)
	must(t, err)
	must(t, r.Close(ctx, id))
	closedAgain, err := r.Load(ctx, id)
	must(t, err)
	if closed.ClosedAt == nil || closedAgain.ClosedAt == nil || !closed.ClosedAt.Equal(*closedAgain.ClosedAt) {
		t.Fatal("closure timestamp changed")
	}
	must(t, r.Record(ctx, id, "subject"))
	_, err = r.Assign(ctx, id, "subject", "unknown")
	must(t, err)
	_, err = r.Assign(ctx, id, "new", "control")
	wantError(t, err, ab.ErrClosed)
	wantError(t, r.Record(ctx, id, "unconverted"), ab.ErrClosed)
}

func testDuplicates(t *testing.T, r *repository.Repository) {
	ctx := t.Context()
	id := create(t, r)
	svc := ab.New(r)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			s := fmt.Sprintf("subject-%d", i%8)
			_, err := svc.Assign(ctx, id, s)
			if err == nil {
				err = svc.Record(ctx, id, s)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	out, err := r.Results(ctx, id)
	must(t, err)
	if out.Control.Subjects+out.Treatment.Subjects != 8 || out.Control.Conversions+out.Treatment.Conversions != 8 {
		t.Fatalf("duplicates: %+v", out)
	}
}

func testRollback(t *testing.T, r *repository.Repository) {
	ctx := t.Context()
	id := create(t, r)
	rollback := errors.New("rollback")
	run := func(fn func(context.Context) error) {
		t.Helper()
		err := r.RunInTx(ctx, func(ctx context.Context) error {
			if err := fn(ctx); err != nil {
				return err
			}
			return rollback
		})
		wantError(t, err, rollback)
	}
	run(func(ctx context.Context) error {
		return r.Create(ctx, ab.Experiment{ID: id + "-new", Control: "a", Treatment: "b"})
	})
	_, err := r.Load(ctx, id+"-new")
	wantError(t, err, ab.ErrNotExist)
	run(func(ctx context.Context) error { _, err := r.Assign(ctx, id, "subject", "control"); return err })
	wantError(t, r.Record(ctx, id, "subject"), ab.ErrNotAssigned)
	_, err = r.Assign(ctx, id, "subject", "treatment")
	must(t, err)
	run(func(ctx context.Context) error { return r.Record(ctx, id, "subject") })
	a, err := r.Assign(ctx, id, "subject", "control")
	must(t, err)
	if a.ConvertedAt != nil {
		t.Fatal("conversion survived rollback")
	}
	run(func(ctx context.Context) error { return r.Close(ctx, id) })
	e, err := r.Load(ctx, id)
	must(t, err)
	if e.ClosedAt != nil {
		t.Fatal("closure survived rollback")
	}
	_, err = r.Assign(ctx, id, "new", "control")
	must(t, err)
}

func testErrors(t *testing.T, r *repository.Repository) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := r.Load(ctx, "valid")
	wantError(t, err, context.Canceled)
	if !strings.Contains(err.Error(), "repository load") {
		t.Fatal("missing operation context")
	}
	ctx = dbtx.WithTxOptions(t.Context(), &sql.TxOptions{ReadOnly: true})
	err = r.RunInTx(ctx, func(ctx context.Context) error {
		return r.Create(ctx, ab.Experiment{ID: "readonly", Control: "a", Treatment: "b"})
	})
	var pgErr *pq.Error
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" || !strings.Contains(err.Error(), "repository create") {
		t.Fatalf("database error lost: %v", err)
	}
}

func testClosureRaces(t *testing.T, r *repository.Repository) {
	for _, kind := range []string{"assign", "record"} {
		for _, closeFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/close-first=%t", kind, closeFirst), func(t *testing.T) {
				id := create(t, r)
				if kind == "record" {
					_, err := r.Assign(t.Context(), id, "subject", "control")
					must(t, err)
				}
				mutate := func(ctx context.Context) error {
					if kind == "record" {
						return r.Record(ctx, id, "subject")
					}
					_, err := r.Assign(ctx, id, "subject", "control")
					return err
				}
				closeExperiment := func(ctx context.Context) error { return r.Close(ctx, id) }
				first, second := mutate, closeExperiment
				if closeFirst {
					first, second = closeExperiment, mutate
				}
				err := orderedTransactions(t, r, first, second)
				if closeFirst {
					wantError(t, err, ab.ErrClosed)
				} else {
					must(t, err)
				}
				out, err := r.Results(t.Context(), id)
				must(t, err)
				if out.Experiment.ClosedAt == nil {
					t.Fatal("experiment not closed")
				}
				if kind == "assign" {
					want := int64(1)
					if closeFirst {
						want = 0
					}
					if out.Control.Subjects != want {
						t.Fatalf("enrollment crossed closure: %+v", out)
					}
				} else {
					want := int64(1)
					if closeFirst {
						want = 0
					}
					if out.Control.Conversions != want {
						t.Fatalf("conversion crossed closure: %+v", out)
					}
				}
			})
		}
	}
}

// orderedTransactions holds the first operation's transaction until PostgreSQL
// reports the second connection waiting on it. No elapsed-time assumption orders
// the operations; a deadline bounds failures and cancellation cleans up waiters.
func orderedTransactions(t *testing.T, r *repository.Repository, first, second func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ready := make(chan int, 1)
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	defer func() { cancel(); unblock(); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstDone <- r.RunInTx(ctx, func(ctx context.Context) error {
			var pid int
			if err := r.DBTx(ctx).QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			if err := first(ctx); err != nil {
				return err
			}
			ready <- pid
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var firstPID int
	select {
	case firstPID = <-ready:
	case err := <-firstDone:
		t.Fatalf("first operation failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	secondReady := make(chan int, 1)
	secondDone := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		secondDone <- r.RunInTx(ctx, func(ctx context.Context) error {
			var pid int
			if err := r.DBTx(ctx).QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			secondReady <- pid
			return second(ctx)
		})
	}()
	var secondPID int
	select {
	case secondPID = <-secondReady:
	case err := <-secondDone:
		t.Fatalf("second transaction failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		must(t, r.DB.DB().QueryRowContext(ctx, `SELECT $1::int = ANY(pg_blocking_pids($2::int))`, firstPID, secondPID).Scan(&blocked))
		if blocked {
			break
		}
		select {
		case err := <-secondDone:
			t.Fatalf("operation completed without waiting: %v", err)
		case <-ctx.Done():
			t.Fatal("did not observe closure lock wait")
		case <-ticker.C:
		}
	}
	unblock()
	select {
	case err := <-firstDone:
		must(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-secondDone:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

func testUsageExample(t *testing.T, r *repository.Repository) {
	db := r.DB.DB().(*sql.DB)
	out, err := runExperiment(t.Context(), db)
	must(t, err)
	if out.Conclusion != ab.InsufficientData {
		t.Fatalf("one-subject example: %+v", out)
	}
}
