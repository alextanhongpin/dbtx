# Lease composition checks

This separate test module verifies `postgres/lease` together with idempotent,
jobs, inbox, and outbox through their existing public APIs. It keeps those
cross-module dependencies out of the lease library's module graph.

See [composition guidance](../../../docs/postgres-lease-composition.md).
Tests start one PostgreSQL 19 container (the message/job schemas use `uuidv7()`).
Install Docker and run:

```sh
go test -race -count=1 -timeout 180s ./...
go vet ./...
```

By default these checks use the pinned published revisions in `go.mod`. Inbox
and outbox use commit-based versions because their latest existing tags lack
the current repository/RequireTx APIs; the guidance documents the exact versions. To check
local edits, create a temporary Go workspace containing the repository root,
`postgres/lease`, this module, `postgres/idempotent`, `postgres/jobs`,
`postgres/inbox`, `postgres/outbox`, `testing/dbtest`, and
`testing/testcontainer`, then set `GOWORK` to that workspace file for the commands.
The root `make test` discovers this nested module automatically.
