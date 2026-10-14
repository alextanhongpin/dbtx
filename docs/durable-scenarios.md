# Durable module: usage scenarios

Companion to the [design](durable-design.md). These scenarios came from writing
client code against an early interface sketch and recording every missing API
and unanswered question. Each row names the API that covers it in the design
and the decision or failure-mode test that pins its behaviour. The main ones
are written out in
[example_test.go](../postgres/durable/example_test.go), which compiles with
the module.

## Locks and leases

| # | Scenario | Design coverage | Pinned by |
| --- | --- | --- | --- |
| S1 | Reserve the last seat without double-booking (short critical section) | `txlock.WithLock` / `TryWithLock` | — |
| S2 | Transfer between two wallets without deadlocking | `txlock.WithLocks` sorts and de-duplicates keys | — |
| S3 | Rebuild a large report page by page under a lease, rejecting stale writes | `lease.Activity`, resource token column, `lease.CheckFenced` | FM-1, FM-3 |
| S4 | Leader election for a single background loop | `lease.Acquire` with long wait, Observer `OnLeaseLost` | FM-3 |
| S5 | External charge, then fenced database finalization | `lease.Activity` + `held.RunInFencedTx` | FM-1, FM-7 |

Resource token convention: column `lock_token bigint not null default 0`, write
with `... AND lock_token <= $token` and set `lock_token = $token`. `<=` lets the
same holder write repeatedly.

## Request idempotency

| # | Scenario | Design coverage | Pinned by |
| --- | --- | --- | --- |
| S6 | `POST /payments` with an `Idempotency-Key` header, work done in the request | `keys.Begin` → `Complete(txCtx)` / `Fail` / `Release`; response envelope with status, headers, body | FM-14 – FM-16, Q1 |
| S7 | Internal caller with no client key (monthly invoice per customer) | `keys.Derive(scope, parts...)` + `jobs.Submit` | FM-17 |
| S8 | Client retries after the retention window | Treated as new; documented | FM-17 |
| S9 | Two tenants send the same key | Mandatory `Scope` | — |

## Executions

| # | Scenario | Design coverage | Pinned by |
| --- | --- | --- | --- |
| S10 | Submit async, return 202, client polls | `jobs.Submit` returns status and replay flag | — |
| S11 | Submit and wait briefly for the result | `jobs.Await` with timeout, distinguishes timeout from failure | Q10 |
| S12 | Create an order and its job atomically | `Submit` joins the `dbtx` transaction in `ctx`; `RequireTx` | FM-18 |
| S13 | Local-only handler completing with its business writes | `Tx` mode, or `Activity` + `exec.Finalize` | FM-5, FM-7 |
| S14 | Handler calling two remote idempotent APIs (charge, ship) | `jobs.RunStep` with saved request and remote key | FM-2 |
| S15 | Remote adapter mapping 409/429/422/5xx | `RetryAfter`, `Permanent`, default retryable | — |
| S16 | Delayed reminder cancelled after an upgrade | `RunAt`, `jobs.Cancel` | — |
| S17 | Job touching a wallet that other jobs also touch | `txlock` inside a `Tx`-mode step (same transaction as completion) | — |
| S18 | Ship fails permanently after charge succeeded | `OnFinalFailure` compensation execution | Q9 |
| S19 | Per-kind timeout, concurrency, retry policy | Registration options | — |

## Outbox

| # | Scenario | Design coverage | Pinned by |
| --- | --- | --- | --- |
| S20 | Order status events, consumers need them in order | `outbox.Publish` to an ordered topic with `AggregateKey` | FM-19 |
| S21 | Analytics events, any order | Unordered topic | — |
| S22 | Run the relay to Kafka | `Relay` with per-topic config and a `Publisher` returning the published prefix | FM-20, FM-23 |
| S23 | A stream's head event keeps failing | Head-of-line policy, stream admin (resume, skip with gap marker) | FM-22 |
| S24 | Consumer lost state and needs a replay | Retention window plus `Replay(stream, fromSeq)` | Q7 |

## Inbox

| # | Scenario | Design coverage | Pinned by |
| --- | --- | --- | --- |
| S25 | Payment provider webhooks, unique event ID | `Dedup` mode | FM-26 |
| S26 | Order events from our ordered outbox via Kafka | `Sequenced` mode with pending buffer | FM-21, FM-24 |
| S27 | Slow handler: persist, ack, process later | `inbox.Enqueue` (dedup + job submission in one transaction) | — |
| S28 | Third-party webhook with timestamps but no sequence | `Versioned` mode | FM-25 |
| S29 | Housekeeping | Separate retention for message IDs and stream positions | — |

## Scheduler and cache

| # | Scenario | Design coverage | Pinned by |
| --- | --- | --- | --- |
| S30 | Nightly report in `Asia/Singapore`, many instances | Schedule keyed by tick; missed-run and overlap policies | FM-27, FM-28, Q5 |
| S31 | Expensive top-50 chart read very often | `cache.GetOrCompute` with stale-while-revalidate | FM-29, FM-30, Q6 |
| S32 | Invalidate the chart when stats change | `cache.InvalidateTx` in the inbox handler's transaction | — |

## Operations and testing

| # | Scenario | Design coverage | Pinned by |
| --- | --- | --- | --- |
| S33 | Worker shutdown on deploy | Drain timeout, hand-back, readiness | FM-31 |
| S34 | Retry failed exports in bulk | Admin list/filter by kind and error, bulk requeue | Q2 |
| S35 | On-call visibility | Observer events and gauges | — |
| S36 | Prove a paused worker cannot complete | `durabletest` fake clock + `Hooks.BeforeComplete` | FM-1 |
| S37 | Prove ordered delivery survives two relays | `durabletest` recording publisher + sequenced inbox | FM-21 |
