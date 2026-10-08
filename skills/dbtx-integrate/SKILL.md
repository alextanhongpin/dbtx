---
name: dbtx-integrate
description: Integrate github.com/alextanhongpin/dbtx into a Go application using database/sql, including repository wiring, transaction boundaries, sqlc queries, savepoints, and database tests. Use when adopting dbtx or fixing its integration in a consuming project.
---

# Integrate dbtx

Inspect the consuming project's driver, repository constructors, transaction owner, generated query interfaces, go.mod, and Go toolchain. Preserve its architecture and configuration ownership. Read [references/integration.md](references/integration.md) for wiring examples and testing guidance.

## Resolve the dependency

- Core import: `github.com/alextanhongpin/dbtx`. PostgreSQL utilities and testing helpers have separate go.mod files; add only modules needed by the request.
- Inspect the selected version with `go list -m -json`, `go doc`, or its local source. This checkout declares Go 1.27.0; check compatibility with the consuming toolchain rather than silently upgrading it.
- The root README contains older APIs. This checkout has no pgxtx, buntx, or sqlxtx directories. Verify any adapter in the selected release before importing it. Core dbtx accepts `*sql.DB`, not native pgx connections or pools; resolve that compatibility choice before replacing a driver stack.
- Follow the project's version policy. Do not infer the latest release from a README or introduce absolute local-path replacements into a reusable integration.

## Preserve transaction boundaries

Construct dbtx at application wiring time and inject it into participating repositories. Put `RunInTx` around the operation that owns atomicity. Every database call inside the callback must receive its callback context and resolve `DBTx(ctx)` at call time.

`DB()` accesses the pool. `DBTx(ctx)` uses the context transaction, falling back to the pool. `Tx(ctx)` panics with `ErrNotTransaction` when the transaction is absent; use only for an intentional transaction-required contract.

Nested `RunInTx` reuses the transaction under the same ID; it does not create savepoints or independently commit. Propagate nested errors unless partial recovery is explicitly implemented with `RunInSubTx`. Swallowing a nested error can commit earlier writes or leave PostgreSQL's transaction aborted.

`RunInSubTx` requires an existing transaction and database savepoint support; outside one it returns `ErrOutOfTx`. It is a concrete DB method absent from `UnitOfWork`; extend the consuming interface only when needed. Returning a savepoint error from the outer callback rolls back the whole transaction; handling it can preserve outer writes.

Separate pools default to the same context ID (`dbtx.ID`). For multiple databases, set distinct IDs during wiring using `SetID`. Do not change IDs during active use or share transaction contexts across independent pools with colliding IDs.

Use `WithTxOptions` before starting a transaction, or `WithNamedTxOptions` for custom IDs. Joining an existing transaction does not apply new options. Let dbtx commit and roll back: callback errors trigger rollback; panics roll back and are rethrown. Return commit failures.

## Verify

Test observable commit and rollback across participating repositories, callback context propagation, and any savepoint or option behavior introduced. Use real connections for locking and concurrency checks. Run target-project formatting, relevant tests, and vet. Report unavailable database or toolchain checks separately from successful validation.
