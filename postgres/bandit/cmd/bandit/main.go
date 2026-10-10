package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/bandit"
	"github.com/alextanhongpin/dbtx/postgres/bandit/dashboard"
	"github.com/alextanhongpin/dbtx/postgres/bandit/repository"
	_ "github.com/lib/pq"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func execute(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, "Usage: bandit <init|simulate|report|serve> [flags]\nSet DATABASE_URL. Use <command> -h for flags. Schema initialization is explicit.")
		return nil
	}
	command := args[0]
	switch command {
	case "init", "simulate", "report", "serve":
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(errOut)
	scenario := f.String("scenario", "clear-winner", "clear-winner, close-call, equal-arm, or all (simulate)")
	probabilities := f.String("probabilities", "", "comma-separated custom arm probabilities (simulate)")
	horizon := f.Int("horizon", 10000, "decisions per repetition (simulate)")
	repetitions := f.Int("repetitions", 30, "repetitions per policy, 1..1000 (simulate)")
	seed := f.Uint64("seed", 1, "first repetition seed (simulate)")
	policies := f.String("policies", strings.Join(bandit.Policies, ","), "comma-separated policy names (simulate)")
	epsilon := f.Float64("epsilon", .1, "epsilon-greedy exploration probability (simulate)")
	runID := f.String("run", "", "run UUID (report)")
	experimentID := f.String("experiment", "", "experiment UUID to list runs (report)")
	address := f.String("address", "127.0.0.1:8080", "loopback host:port (serve)")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "serve" {
		host, _, err := net.SplitHostPort(*address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return errors.New("serve requires a loopback IP address")
		}
	}
	var specs []struct {
		name  string
		probs []float64
	}
	var configs []bandit.PolicyConfig
	if command == "simulate" {
		if *repetitions < 1 || *repetitions > 1000 || uint64(*repetitions-1) > ^uint64(0)-*seed {
			return errors.New("invalid repetition count or seed range")
		}
		names := []string{*scenario}
		if *scenario == "all" {
			names = []string{"clear-winner", "close-call", "equal-arm"}
		}
		if *probabilities != "" {
			p := []float64{}
			for _, s := range strings.Split(*probabilities, ",") {
				v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil {
					return errors.New("invalid probability")
				}
				p = append(p, v)
			}
			specs = append(specs, struct {
				name  string
				probs []float64
			}{"custom", p})
		} else {
			for _, name := range names {
				p, err := bandit.Scenario(name)
				if err != nil {
					return err
				}
				specs = append(specs, struct {
					name  string
					probs []float64
				}{name, p})
			}
		}
		for _, s := range specs {
			if err := bandit.ValidateScenario(s.probs, *horizon); err != nil {
				return err
			}
		}
		seen := map[string]bool{}
		for _, name := range strings.Split(*policies, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				return errors.New("policy name must be nonempty")
			}
			if seen[name] {
				return errors.New("duplicate policy")
			}
			seen[name] = true
			c := bandit.PolicyConfig{Name: name}
			if name == "epsilon-greedy" {
				c.Epsilon = epsilon
			}
			if _, err := bandit.NewPolicy(c); err != nil {
				return err
			}
			configs = append(configs, c.WithDefaults())
		}
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return errors.New("database configuration failed")
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		return errors.New("database connection failed; check DATABASE_URL and PostgreSQL availability")
	}
	repo := repository.New(db)
	b, _ := bandit.New(bandit.Config{Repository: repo})
	switch command {
	case "init":
		if _, err := db.ExecContext(ctx, repository.Schema); err != nil {
			return err
		}
		fmt.Fprintln(out, "Bandit schema initialized.")
		return nil
	case "simulate":
		for _, spec := range specs {
			arms := make([]bandit.Arm, len(spec.probs))
			for i := range arms {
				arms[i] = bandit.Arm{Key: fmt.Sprintf("arm-%d", i+1), Label: fmt.Sprintf("Arm %d", i+1)}
			}
			e, err := b.CreateExperiment(ctx, bandit.Experiment{Name: spec.name, RewardDescription: "Synthetic independent Bernoulli success", Arms: arms})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Experiment %s (%s)\n", e.ID, spec.name)
			for repetition := range *repetitions {
				for _, cfg := range configs {
					r, err := b.SaveSimulation(ctx, bandit.Run{ExperimentID: e.ID, Policy: cfg, Seed: *seed + uint64(repetition), Probabilities: spec.probs, Horizon: *horizon}, e.Arms)
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "  %s seed=%d run=%s decisions=%d\n", cfg.Name, r.Seed, r.ID, r.Sequence)
				}
			}
		}
		return nil
	case "report":
		if *runID != "" && *experimentID != "" {
			return errors.New("choose --run or --experiment")
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if *runID != "" {
			id, err := uuid.Parse(*runID)
			if err != nil {
				return errors.New("invalid run UUID")
			}
			s, err := b.Summary(ctx, id)
			if err != nil {
				return err
			}
			posterior := map[string]bandit.Posterior{}
			for _, a := range s.Arms {
				posterior[a.Arm.Key] = bandit.CredibleInterval(a)
			}
			return enc.Encode(struct {
				Summary   bandit.Summary
				Posterior map[string]bandit.Posterior
			}{s, posterior})
		}
		if *experimentID != "" {
			id, err := uuid.Parse(*experimentID)
			if err != nil {
				return errors.New("invalid experiment UUID")
			}
			for offset := 0; ; offset += 200 {
				runs, err := b.ListRuns(ctx, id, bandit.Page{Limit: 200, Offset: offset})
				if err != nil {
					return err
				}
				if err := enc.Encode(runs); err != nil {
					return err
				}
				if len(runs) < 200 {
					break
				}
			}
			return nil
		}
		for offset := 0; ; offset += 200 {
			e, err := b.ListExperiments(ctx, bandit.Page{Limit: 200, Offset: offset})
			if err != nil {
				return err
			}
			if err := enc.Encode(e); err != nil {
				return err
			}
			if len(e) < 200 {
				break
			}
		}
		return nil
	case "serve":
		server := &http.Server{Addr: *address, Handler: dashboard.New(b), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
		listener, err := net.Listen("tcp", *address)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Bandit dashboard: http://%s\n", listener.Addr())
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		select {
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
				return err
			}
			<-done
			return nil
		}
	}
	return nil
}
