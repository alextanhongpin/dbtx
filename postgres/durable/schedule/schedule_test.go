package schedule_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
	"github.com/alextanhongpin/dbtx/postgres/durable/schedule"
)

func TestParse(t *testing.T) {
	sgt, _ := time.LoadLocation("Asia/Singapore")
	cases := []struct {
		spec string
		from string
		want string
		loc  *time.Location
	}{
		{"*/15 9-17 * * 1-5", "2026-10-16T17:50:00Z", "2026-10-19T09:00:00Z", time.UTC}, // Fri 17:50 -> Mon 09:00
		{"0 2 * * *", "2026-10-14T17:30:00Z", "2026-10-15T02:00:00+08:00", sgt},         // 01:30 SGT
		{"0 2 * * *", "2026-10-14T18:30:00Z", "2026-10-16T02:00:00+08:00", sgt},         // 02:30 SGT
		{"0 0 13 * 5", "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", time.UTC},        // Friday OR the 13th
		{"@monthly", "2026-10-14T00:00:00Z", "2026-11-01T00:00:00Z", time.UTC},
		{"30 1 * * 7", "2026-10-14T00:00:00Z", "2026-10-18T01:30:00Z", time.UTC}, // 7 = Sunday
		{"@every 90m", "2026-10-14T00:00:00Z", "2026-10-14T01:30:00Z", time.UTC},
	}
	for _, c := range cases {
		sp, err := schedule.Parse(c.spec, c.loc)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		from, _ := time.Parse(time.RFC3339, c.from)
		want, _ := time.Parse(time.RFC3339, c.want)
		if got := sp.Next(from); !got.Equal(want) {
			t.Errorf("%s from %s: got %s, want %s", c.spec, c.from, got, want)
		}
	}
	for _, bad := range []string{"* * * *", "60 * * * *", "*/0 * * * *", "5-1 * * * *", "@every 1ms"} {
		if _, err := schedule.Parse(bad, nil); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func setup(t *testing.T) (*schedule.Scheduler, *jobs.Client, *dbtx.DB) {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	jc, _ := jobs.NewClient(jobs.Config{DB: db, Kinds: []string{"report"}})
	s, err := schedule.New(schedule.Config{DB: db, Jobs: jc})
	if err != nil {
		t.Fatal(err)
	}
	return s, jc, db
}

func countJobs(t *testing.T, db *dbtx.DB) int {
	var n int
	if err := db.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM durable.executions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func backdate(t *testing.T, db *dbtx.DB, name string, d time.Duration) {
	if _, err := db.DB().ExecContext(t.Context(), `UPDATE durable.schedules SET next_run_at = now() - $2 * interval '1 second' WHERE name = $1`, name, d.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// FM-28: missed-run policies.
func TestMissedPolicies(t *testing.T) {
	for policy, want := range map[schedule.Missed]int{schedule.MissedAll: 4, schedule.MissedLatest: 1, schedule.MissedNone: 0} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := t.Context()
			s, _, db := setup(t)
			if err := s.Register(ctx, schedule.Schedule{Name: "r", Spec: "@every 1h", Kind: "report", Missed: policy}); err != nil {
				t.Fatal(err)
			}
			backdate(t, db, "r", 3*time.Hour+10*time.Minute)
			n, err := s.RunOnce(ctx)
			if err != nil || n != want || countJobs(t, db) != want {
				t.Fatalf("submitted %d (%v), want %d", n, err, want)
			}
			if n, _ := s.RunOnce(ctx); n != 0 {
				t.Fatalf("second pass submitted %d", n)
			}
		})
	}
}

// FM-27: many schedulers on the same tick submit one job.
func TestConcurrentSchedulers(t *testing.T) {
	ctx := t.Context()
	s, jc, db := setup(t)
	_ = s.Register(ctx, schedule.Schedule{Name: "nightly", Spec: "0 2 * * *", TimeZone: "Asia/Singapore", Kind: "report"})
	backdate(t, db, "nightly", time.Minute)
	var wg sync.WaitGroup
	for range 8 {
		other, _ := schedule.New(schedule.Config{DB: db, Jobs: jc})
		wg.Go(func() { _, _ = other.RunOnce(ctx) })
	}
	wg.Wait()
	if n := countJobs(t, db); n != 1 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestOverlap(t *testing.T) {
	for policy, want := range map[schedule.Overlap]int{schedule.OverlapSkip: 1, schedule.OverlapQueue: 1, schedule.OverlapAllow: 2} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := t.Context()
			s, _, db := setup(t)
			_ = s.Register(ctx, schedule.Schedule{Name: "r", Spec: "@every 1h", Kind: "report", Overlap: policy})
			backdate(t, db, "r", time.Minute)
			_, _ = s.RunOnce(ctx) // first job stays pending
			backdate(t, db, "r", 2*time.Minute)
			_, _ = s.RunOnce(ctx)
			if n := countJobs(t, db); n != want {
				t.Fatalf("jobs = %d, want %d", n, want)
			}
			if policy == schedule.OverlapQueue {
				_, _ = db.DB().ExecContext(ctx, `UPDATE durable.executions SET status = 'succeeded', finished_at = now()`)
				_, _ = s.RunOnce(ctx)
				if n := countJobs(t, db); n != 2 {
					t.Fatalf("queued tick did not run after the previous finished: %d", n)
				}
			}
		})
	}
}

func TestPauseResume(t *testing.T) {
	ctx := t.Context()
	s, _, db := setup(t)
	_ = s.Register(ctx, schedule.Schedule{Name: "r", Spec: "@every 1h", Kind: "report"})
	_ = s.Pause(ctx, "r")
	backdate(t, db, "r", time.Minute)
	if n, _ := s.RunOnce(ctx); n != 0 {
		t.Fatalf("paused schedule fired")
	}
	if err := s.Resume(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.RunOnce(ctx); n != 0 {
		t.Fatalf("resume caught up a paused tick")
	}
	_ = context.Background
}

// Regression: MissedAll catches up across passes instead of dropping ticks
// beyond MaxCatchUp.
func TestMissedAllCatchesUpAcrossPasses(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(durabletest.NewDB(t))
	jc, _ := jobs.NewClient(jobs.Config{DB: db, Kinds: []string{"report"}})
	s, _ := schedule.New(schedule.Config{DB: db, Jobs: jc, MaxCatchUp: 2})
	// OverlapAllow isolates catch-up: with the default OverlapSkip, ticks are
	// dropped while the previous tick's job (never run here) is unfinished.
	_ = s.Register(ctx, schedule.Schedule{Name: "r", Spec: "@every 1h", Kind: "report",
		Missed: schedule.MissedAll, Overlap: schedule.OverlapAllow})
	backdate(t, db, "r", 4*time.Hour+10*time.Minute) // 5 ticks due
	var per []int
	for range 4 {
		n, err := s.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		per = append(per, n)
	}
	if fmt.Sprint(per) != "[2 2 1 0]" || countJobs(t, db) != 5 {
		t.Fatalf("per pass %v, jobs %d", per, countJobs(t, db))
	}
}
