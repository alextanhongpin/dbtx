# postgres/durable TODO

Plan for the module described in [docs/durable-design.md](../../docs/durable-design.md).
Decisions are `D*`/`Q*` in [durable-decisions.md](../../docs/durable-decisions.md),
failure tests are `FM-*` in [durable-failure-modes.md](../../docs/durable-failure-modes.md),
scenarios are `S*` in [durable-scenarios.md](../../docs/durable-scenarios.md).

Status: phases 0–8 implemented and tested (`go test -race ./...` against
PostgreSQL 16; 9 packages, 68 test functions). Remaining work is under
[Follow-ups](#follow-ups).

## Done

### Phase 0: decisions and scaffolding

- [x] Resolve every open decision with its proposed default (D6, Q1–Q11)
- [x] Module `github.com/alextanhongpin/dbtx/postgres/durable` (Go 1.27) on root `dbtx` v0.1.0 and `lib/pq`
- [x] `repository/schema.sql`, `migrations/0001_init.sql`, exported `Schema` and `Migrations`
- [x] Schema `durable`, `fencing_seq`, `text` + `CHECK` statuses; schema applies twice
- [x] Package skeletons (D4)

### Phase 1: core engine and test harness

- [x] Error vocabulary, `TxOutcomeError`, SQLSTATE through `SQLState()` (D3)
- [x] `Permanent`, `RetryAfter`, `ExponentialBackoff`, `NextDelay`
- [x] `internal/engine.Keeper`: heartbeat every TTL/3, local deadline, terminal causes, `Supervise`, `Protect`, `CancelWork`
- [x] `Observer` (no-op, `SlogObserver`) and `Hooks`
- [x] `durabletest`: per-test database, `Gate`, `Recorder`

### Phase 2: locks and leases

- [x] `txlock.WithLock`, `WithLocks` (sorted, de-duplicated), `TryWithLock(s)`, `lock_timeout` (S1, S2)
- [x] `lease`: Acquire/TryAcquire, Activity/TryActivity, RunInFencedTx/TryRunInFencedTx, `held.RunInFencedTx`, `CheckFenced` (S3–S5)
- [x] `durable.advance_fencing` for failover and restore (D6)

### Phase 3: request idempotency

- [x] `keys.Begin` / `Complete` (in the caller's transaction, fenced) / `Fail` / `Release`; heartbeated in-flight lease (S6)
- [x] `keys.Do` with the Q1 failure policy; releases on panic
- [x] `keys.Derive` (S7), `Purge` (S8)

### Phase 4: executions

- [x] `Submit` joining the caller's transaction, `RequireTx`, key + execution in one transaction (S10, S12)
- [x] Kind registration: mode, timeout, concurrency, lease, backoff, compensation (S19)
- [x] `ModeTx` step transactions with checkpoints; `ModeActivity` with heartbeats and `Finalize` (S13)
- [x] `RunStep` with saved request, stable remote key, saved response (S14, S15)
- [x] `Cancel`, `Await` (polling), `Get`, `GetByKey` (S11, S16)
- [x] Compensation on final failure, including failures found by the reaper (S18)
- [x] Worker drain with hand-back, `Ready`, `RunOnce`; reaper scoped to registered kinds (S33)
- [x] Admin: `ListFailed`, `Requeue`, `RequeueFailed`, `OldestPayloadVersions`, `Purge`, `PurgeKeys`, `Janitor` (S34)

### Phase 5: outbox

- [x] Topic registration; `Publish` requires a transaction by default; `ErrUnknownTopic`, `ErrAggregateRequired`
- [x] Unordered relay; one rejection costs one attempt (D16)
- [x] Ordered streams: gapless sequence from `outbox_heads`, leased `outbox_streams`, fenced advance (D11) (S20, S22)
- [x] Head policies `HeadBlock`, `HeadPark` (gap marker), `HeadStop`; shutdown never spends attempts (S23)
- [x] Admin: `Stream`, `Resume`, `Skip`, `Replay`, `PurgePublished`, `DeadLetters`, `Requeue`; admin changes fence out the relay (D18) (S24)

### Phase 6: inbox

- [x] `ModeDedup`, `ModeSequenced` with pending buffer and in-order drain, `ModeVersioned` (D10) (S25, S26, S28)
- [x] `Enqueue` store-and-forward into a job (S27)
- [x] `Drain`, `Gaps`, `SkipGap`, `PurgeMessages` (S29)
- [x] End-to-end: two relays (one paused) into a sequenced inbox apply exactly 1..N (FM-21)

### Phase 7: scheduler

- [x] Five-field cron, descriptors, `@every`; IANA time zones
- [x] One job per tick via `schedule:<name>@<tick>` keys; missed-run and overlap policies; catch-up across passes (S30)
- [x] Pause, resume, delete

### Phase 8: cache

- [x] `Get`, `Set`, `GetOrCompute` with compute lease, stale-while-revalidate, `Wait`/`ComputeAnyway` (S31)
- [x] Versioned writes; invalidation by tombstone (D17); `InvalidateTx` (S32); `Purge`

### Queries and shared lease functions

- [x] All SQL in `repository/queries/*.sql`, generated with sqlc v1.31.1; generation verified reproducible
- [x] `durable.lease_grant/renew/lock/finish/expire` used by executions, outbox events, outbox streams and idempotency keys (D20)
- [x] Contract test: every adopting table has the lease columns; transitions tested on a throwaway table
- [x] NULL means "no lease"; `pg.JSON` for nullable `jsonb` (D21)

### Review

- [x] Independent review of fencing, SQL and concurrency; all 12 findings fixed with regression tests (D15–D19)

## Follow-ups

### Testing

- [ ] Run the suite against PostgreSQL 17+ and the repository's 19 beta (`compose.yaml`)
- [ ] Fault-injecting `database/sql` driver: lost COMMIT acknowledgement (FM-7), dropped heartbeat connection (FM-8), connection kill between submit statements (FM-18)
- [ ] Process kill mid-step via subprocess (FM-5); handler that ignores cancellation after lease loss (FM-4)
- [ ] PostgreSQL restart and failover drill with `advance_fencing` (FM-9, FM-12); optional PgBouncer job (FM-13)
- [ ] Load and bloat benchmarks for claim latency and retry storms (FM-32, FM-33)
- [ ] Port the existing modules' test suites as a parity bar (Q11)
- [ ] Optionally switch `durabletest` to `testing/dbtest` containers (D13)

### Features

- [ ] `LISTEN/NOTIFY` wakeup for workers, relays and `Await`, with polling fallback (Q10)
- [x] Generate queries with sqlc (D12); shared `durable.lease_*` functions for every ownership change (D20)
- [ ] Switch tests and examples to `pgx/v5/stdlib` when ready (D3)
- [ ] Per-kind metrics (queue depth, oldest pending age) on top of `Observer`

### Release

- [x] Root README module table entry (marked experimental)
- [ ] Update `skills/dbtx-postgres-patterns` with the new module
- [ ] Parity notes and a migration outline for each existing module
- [ ] Tag the first version
