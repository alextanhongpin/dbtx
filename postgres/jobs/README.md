# PostgreSQL jobs

A durable background job queue with idempotent submission, concurrent workers,
leases, fencing tokens, retries, and a maintenance loop.

## Requirements and installation

Requires Go 1.27+ and PostgreSQL 18+ (`uuidv7()` is used by the schema). Use
`READ COMMITTED` isolation. Start a database with the
[root quick start](../../README.md#run-a-complete-example).

```bash
go get github.com/alextanhongpin/dbtx/postgres/jobs github.com/lib/pq
```

Apply [repository/schema.sql](repository/schema.sql), exported as
`repository.Schema`, through your migration system before submitting jobs:

```go
if _, err := db.ExecContext(ctx, repository.Schema); err != nil {
    return err
}
```

Here `db` is a connected `*sql.DB` and `ctx` is a context. The schema guards
repeat creation of its enum; existing installations still need migrations for
schema changes. Queries and worker operations use autocommit; do not run a
worker or janitor inside a `dbtx` transaction.

## Submit and process jobs

These fragments run inside an application after opening and pinging `db`,
registering `github.com/lib/pq`, and applying the schema. Import `context`,
`encoding/json`, `fmt`, and both `jobs` and `jobs/repository` from the installed
module. The [root example](../../README.md#run-a-complete-example) shows the
complete database connection and `go run .` workflow.

```go
repo := repository.New(db)
submitter := jobs.NewSubmitter(repo, jobs.SubmitConfig{})
result, err := submitter.Submit(ctx, jobs.SubmitInput{
    IdempotencyKey: "send-welcome:123",
    Request: json.RawMessage(`{"user_id":123}`),
})
if err != nil { return err }
fmt.Println(result.JobID, result.Status, result.Created)

worker := jobs.NewWorker(repo,
    func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
        // Perform idempotent work and respect ctx cancellation.
        return json.RawMessage(`{"sent":true}`), nil
    }, jobs.WorkerConfig{}, nil)
return worker.Run(ctx)
```

The first submit returns `Created=true` and `StatusQueued`. Repeating the key
with an equivalent JSON request returns the existing job and `Created=false`;
a different payload returns `ErrIdempotencyKeyReuse`. Empty requests become
`{}`. `Delay` schedules future work and `MaxAttempts` defaults to 10.

`Run` polls until its context is cancelled. Use a signal-aware context in your
application and cancel it on shutdown. Defaults are four concurrent jobs,
a 30-second lease, and a 30-second shutdown grace period. Handlers must honor
cancellation; otherwise shutdown can return `ErrShutdownTimeout`.

Delivery is **at least once**. The handler does not run inside a database
transaction that commits with completion. External effects and business writes
can repeat if completion is lost. Use `job.ID` as a downstream idempotency key,
or enforce `job.FencingToken` downstream.

Return `jobs.Permanent(err)` for a terminal failure or
`jobs.RetryAfter(err, duration)` to choose a delay. Other errors and recovered
panics retry with exponential backoff and jitter, until attempts are exhausted.
Return valid JSON for a successful response; an empty response becomes JSON
`null`, and invalid JSON fails permanently.

## Maintenance

Run a janitor alongside workers with the same shutdown context, handling its
returned error:

```go
janitor, err := jobs.NewJanitor(repo, jobs.JanitorConfig{}, nil)
if err != nil { return err }
return janitor.Run(ctx)
```

Defaults: one-minute maintenance interval, 24-hour finished-job retention, and
seven-day idempotency key retention. `KeyTTL` must exceed `Retention`.
`ReapGrace` defaults to one minute; keep it above the worker's finalization
timeout. Maintenance reaps exhausted expired leases, archives finished jobs,
and purges expired keys. After key retention expires, reusing a key can create
new work.

## Run tests

From the repository root:

```bash
cd postgres/jobs
go test -race -count=1 .
go test -race -count=1 ./...
go vet ./...
```

The first command runs worker/submitter tests with a fake repository and needs
no database. The full suite includes repository integration tests, requiring a
running Docker daemon (`docker info`) and network access on the first run.
Integration tests start `postgres:19beta3-alpine3.24`, apply their schema, and
clean up automatically. They do not use Compose or `DATABASE_URL`.
