# Native PostgreSQL retry transitions

Inbox, outbox, jobs, and idempotent now use static SQL in each module's
`repository/queries` directory. Claims and outcomes update each work row once,
including responses, attempt counters, status, and ownership. Generate checked-in
Go code with `go tool sqlc generate -f postgres/<module>/repository/sqlc.yaml`.
Resource leases retain native acquire/renew functions so expiry is checked after
acquiring the row lock. No active module installs generic ownership functions,
JSONB state policies, or lifecycle views.

## Upgrade

Stop older workers before applying each durable module's `repository.Migrations`
in one transaction. It drops that module's typed adapter and lifecycle view,
without changing tables or persisted claims. Apply the lease module's
`repository.OwnershipMigrations` to replace acquire/renew functions and remove
its old resource view. These migrations are repeatable. Deploy the new workers
afterward; existing table permissions suffice.

The generic retry and ownership packages, SQL implementations, bundle generators,
and historical fixtures have been removed. Cleanup migrations leave global shared
functions installed because older independently deployed modules may still use
them; applications can remove those database objects after all consumers migrate.
Rolling back to old workers requires restoring their schema from the old release.

## Runtime sharing

`internal/renew` supplies tick scheduling, conservative deadlines, and an
independent deadline watcher. Jobs retain transient-error retry and detached
finalization. Leases retain manual renewal, terminal ownership, release, and
transaction-protected deadlines. Late responses cannot revive expired local
ownership. Queue ownership remains permissive after database expiry when the
owner/token still match; standalone leases require unexpired ownership at
renewal and transaction admission.

Use the root Go workspace for local development. Before independent jobs/lease
releases, publish root dbtx with `internal/renew` and update their dependency pins.

## Verification

Run race tests and vet in each affected nested module and for `internal/renew`.
Migration tests create inert obsolete-object stubs, preserve live claims through
repeated cleanup, and exercise stale-owner rejection. `TestNativeTransitionWriteCost` in
the jobs repository verifies claim/heartbeat/complete each update the row once:
transaction-local tuple updates are asserted, while jobs/second and WAL/job are
reported as observations. WAL can include background activity; throughput depends
on the host and workload. Neither observation is a general performance promise.

Before removing the historical implementation, a local PostgreSQL `19beta3-alpine3.24` run on 2026-10-15, processing 100 jobs
per path, observed:

| Claim + heartbeat + completion | Tuple updates/job | WAL bytes/job | Jobs/second |
| --- | ---: | ---: | ---: |
| Native | 3 | 1,316 | 917 |
| Legacy | 5 | 2,202 | 492 |

This sample reduced tuple updates by 40% and observed about 40% less WAL.
It uses a single transaction per batch and rolls back afterward; it does not
measure commit latency, steady-state vacuum, or production concurrency. Measure the current native path
with `go test -race -run TestNativeTransitionWriteCost -v ./repository` from
`postgres/jobs`.
