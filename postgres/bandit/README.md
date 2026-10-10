# bandit

Binary-reward multi-armed bandit experiments with PostgreSQL storage, reproducible
simulations, and a local results dashboard. Requires Go 1.27+ and PostgreSQL 18+.
This is an experimental module; experiment arms and run configuration are immutable.

## Quick start

From this module directory, point at a development database:

```bash
export DATABASE_URL='postgres://john:123456@127.0.0.1:5432/dev?sslmode=disable'
go run ./cmd/bandit init
go run ./cmd/bandit simulate --scenario all --horizon 1000 --repetitions 3
go run ./cmd/bandit serve
```

Open **http://127.0.0.1:8080**. Select an experiment, open its runs, or select
completed simulations to compare. For the default 120-run comparison choose
200 runs per page on the experiment page. Each policy must have the same seed set,
scenario probabilities, horizon, experiment, and implementation version.

The dashboard is read-only, has no authentication, and the CLI only permits
loopback IP bindings. All charts, scripts, and styles are bundled; no CDN is
required. Keyboard navigation, scrollable tables, and responsive charts work on
small screens. Refresh pages to see new feedback. Stop with Ctrl-C for graceful
shutdown; restarting reconnects to persisted results.

Schema initialization is explicit and additive: `init` does not delete data.
Applications may apply `repository.Schema` themselves. Constructors never apply
the schema, open a connection, or start background work. There is no automatic
retention; use a dedicated database for disposable experiments.

## Policies and simulations

| Policy | Selection rule |
| --- | --- |
| `uniform` | Choose equally among arms; comparison baseline. |
| `epsilon-greedy` | Initialize unobserved arms, then explore uniformly with probability epsilon; otherwise maximize observed success rate. Default epsilon is 0.1; explicit zero is supported. |
| `ucb1` | Initialize unobserved arms, then maximize `mean + sqrt(2*log(total observations)/arm observations)`. |
| `thompson` | Draw each arm's success probability from `Beta(1+successes,1+failures)` and select the largest draw. |

Equal scores break randomly. UCB scores are selection indices, not posterior
estimates or confidence intervals. Thompson sampling follows the
[Bernoulli model in Russo et al.](https://arxiv.org/abs/1707.02038); the UCB1 rule
is described in this [algorithm reference](https://cmudeeprl.github.io/703website_f21/assets/lectures/f21/banditsexploreF21.pdf).

Shipped scenarios: `clear-winner` `[0.1,0.3,0.6]`, `close-call`
`[0.45,0.50,0.55]`, and `equal-arm` `[0.5,0.5,0.5]`. `all` runs each scenario in
its own experiment. Defaults are clear-winner, 10,000 decisions, 30 repetitions,
seed 1, and all four policies. Repetition seeds increment from the first seed.

```bash
go run ./cmd/bandit simulate --probabilities .2,.4,.8 \
  --horizon 10000 --repetitions 10 --seed 42 \
  --policies uniform,epsilon-greedy,ucb1,thompson --epsilon 0
go run ./cmd/bandit report
go run ./cmd/bandit report --experiment EXPERIMENT_UUID
go run ./cmd/bandit report --run RUN_UUID
```

`report` writes JSON summaries; run reports also include posterior intervals.
Experiment/run lists use newline-separated JSON pages of up to 200 records.
Use the dashboard's CSV export for every decision, reward, timestamp, running
reward rate, and simulation regret. Blank reward cells mean pending feedback.
CSV cells that could be spreadsheet formulas are prefixed with an apostrophe.

Policies and `Simulate` need no database. Each decision's PCG source is derived
from SHA-256 of the version tag and big-endian seed, sequence, and stream ID.
Policy randomness uses stream zero; arm reward streams use ordinal plus one.
Matched policies consume the same reward sequence for each arm, indexed by how
many times that arm was selected. Results reproduce arm choices, scores and
rewards for the same version/configuration/seed; UUIDs and timestamps differ.

Each repetition runs in memory with immediate feedback, then is written using
COPY inside one transaction. Interrupted simulations leave previously committed
repetitions intact. Horizon is limited to 1,000,000 decisions and arms to 100;
large experiments retain full diagnostics and can use substantial memory/disk.

## Live application usage

```go
repo := repository.New(db) // Application-owned *sql.DB.
b, err := bandit.New(bandit.Config{Repository: repo})
if err != nil { return err }
experiment, err := b.CreateExperiment(ctx, bandit.Experiment{
    Name: "Subject line",
    RewardDescription: "Clicked within the application's observation window",
    Arms: []bandit.Arm{
        {Key: "short", Label: "Short subject"},
        {Key: "descriptive", Label: "Descriptive subject"},
    },
})
if err != nil { return err }
run, err := b.StartRun(ctx, bandit.Run{
    ExperimentID: experiment.ID,
    Policy: bandit.PolicyConfig{Name: "thompson"}, Seed: 42,
})
if err != nil { return err }
decision, err := b.Select(ctx, run.ID, "delivery:123")
if err != nil { return err }
// Deliver decision.ArmKey; retain decision.ID for eventual feedback.
if err := b.Observe(ctx, decision.ID, true); err != nil { return err }
return b.CloseRun(ctx, run.ID)
```

See [the compilable example](examples_test.go). Reward is `true` for success or
`false` for an explicitly observed failure. The application defines the observation
window and when a negative outcome is known; missing feedback remains pending.
Pending decisions do not enter learning estimates. Epsilon-greedy and UCB1 may
therefore repeatedly choose arms waiting for their first completed observation.

`Select` retries with the same run/request key return the original decision,
including after closure. Identical `Observe` retries preserve the first feedback
timestamp and counters; contradictory outcomes return `ErrConflict`. `CloseRun`
is idempotent, rejects new decisions, and permits late feedback. Start a fresh run
to change configuration; start a fresh experiment to change arms. Live runs are
limited to 1,000,000 decisions; continue with a fresh run after that limit.

All writes serialize on a PostgreSQL run row; different runs remain concurrent.
The ledger, feedback, statistics, and sequence update together. Methods join an
existing `dbtx` transaction in context; only its owner commits. Use the callback's
context for every operation. If multiple databases share a context, set distinct
transaction IDs through `repo.SetID`. Handle operation errors by rolling back the
outer transaction. Avoid expensive work inside a transaction holding a run lock.

Inspect errors with `errors.Is`: `ErrInvalid`, `ErrNotExist`, `ErrClosed`, and
`ErrConflict`. SQL/cancellation errors propagate. `Bandit` is concurrency-safe
when its repository is. Pure policies do not mutate input, but their caller-owned
random sources must not be shared concurrently. `StartRun` is for live runs;
`SaveSimulation` computes and saves a complete repetition.

## Reading and interpreting results

- **Reward:** total recorded successes. Compare over the same horizon and reward
  definition. Running reward rate divides successes by completed observations;
  pending feedback is excluded.
- **Allocation:** fraction of decisions assigned to each arm. Spread indicates
  exploration; concentration indicates preference, not proof of superiority.
- **Pseudo-regret:** in simulations only, sum of `best true probability - selected
  arm probability`. Lower is better. It is unavailable for live traffic.
- **Posterior:** every arm shows the Beta(1,1) posterior mean and equal-tailed 95%
  credible interval. These are model-based, not frequentist confidence intervals.
  An unobserved arm has unavailable observed rate and a prior mean of 50%.
- **Comparison bands:** mean curves with 10th–90th percentile bands across matched
  repetitions, using linearly interpolated percentiles. They describe run-to-run
  variation, not confidence in a causal effect. Charts sample at most 500 points;
  exports retain every decision.

Example: choosing probability 0.3 when the best arm has probability 0.6 adds 0.3
pseudo-regret regardless of that decision's realized reward. Eight successes from
ten observed decisions yield an 80% observed rate and Beta(9,3) posterior mean of
75%. Five additional pending decisions do not enter either estimate.

Live curves group current feedback by original decision sequence; they do not
reconstruct what was known at each historical instant. Missing or selectively
reported outcomes can bias estimates. Adaptive allocation does not automatically
establish causal uplift, and the dashboard never declares a statistically proven
winner. V1 assumes stationary independent Bernoulli rewards; contextual policies,
changing probabilities, automatic stopping, remote hosting/authentication and
high-throughput allocation are outside this version.

## Development

```bash
go test -race -count=1 ./...
go vet ./...
make sqlc
```

Policy, simulation, metric, CLI validation, and dashboard tests are database-free.
Repository tests start a real PostgreSQL container with the repository's `dbtest`
helper and cover transaction joining, rollback, concurrency, idempotency,
constraints, complete simulation persistence and ledger/aggregate agreement.
Docker must be running. Generated sqlc code is checked in; regeneration uses the
root module's pinned tool. No root-module tests are needed for this isolated module.
