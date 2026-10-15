# Durable module: failure modes

Companion to the [design](design.md). Each row is a required test in the
fault-injection suite (`FM-n` IDs are referenced from the
[TODO](../TODO.md)). [Test coverage](#test-coverage) at the end
lists which ones are automated. Tests assert on resource contents,
receipts and broker records, not only on metadata.

## The core rule

A check made inside the process is never enough. A pause between "check" and
"act" makes every local check stale, and the watchdog goroutine is paused too.
Only a check that the resource itself evaluates at write time is safe:

| Write target | Protected by | Still breaks when |
| --- | --- | --- |
| The same PostgreSQL database | Owner + token + unexpired lease in the statement's `WHERE`, or `Tx`-mode / `Finalize` transactions | A write path omits the predicate |
| Remote idempotent API | Same remote key and the same saved request | The pause outlasts the remote key's retention, or the request was rebuilt |
| Remote non-idempotent API, email, files, broker publish | Nothing | Always at least once; the receiver must deduplicate |

## Process and timing

| ID | Scenario | Expected behaviour | How to test |
| --- | --- | --- | --- |
| FM-1 | Stop-the-world pause longer than the lease (GC, CPU throttling, VM migration, `SIGSTOP`) | Successor admitted with a higher token; the paused worker's completion, checkpoint and fenced writes are rejected | Hook `BeforeComplete` blocks worker A; fake clock advances past TTL; worker B completes; release A; assert one outcome and B's output |
| FM-2 | Pause between a remote call and saving its response | Retry resends the saved request with the same remote key; the fake remote records one effect | Hook after call, before `FinishStep` |
| FM-3 | Heartbeat goroutine starved while work continues | Watchdog cancels the work context by the local deadline; late renewal cannot revive the lease | Block heartbeats via hook; assert context cause `ErrLeaseLost` |
| FM-4 | Handler ignores context cancellation | Worker waits for it to return, never reports success after loss, and completion is rejected | Handler sleeps past loss and returns nil |
| FM-5 | Process killed mid-step (`kill -9`) | `Tx`-mode step rolls back; work resumes from the last checkpoint after expiry | Subprocess test or connection kill |
| FM-6 | Poison job crashes the worker on every attempt | Attempts counted at claim; job fails after `max_attempts` | Handler panics or exits |

## Database and network

| ID | Scenario | Expected behaviour | How to test |
| --- | --- | --- | --- |
| FM-7 | COMMIT acknowledgement lost | `TxOutcomeError{Unknown}`; completion APIs are idempotent ("already succeeded with my token" is success); caller re-reads | Fault driver drops the COMMIT reply |
| FM-8 | Connection drops mid-heartbeat | Heartbeat outcome unknown; deadline not extended; lease lost if not renewed in time | Proxy (toxiproxy) or fault driver |
| FM-9 | PostgreSQL restart | Workers reconnect; in-flight leases expire; work retried; no duplicate local effects | Restart container |
| FM-10 | Pool exhausted | Heartbeats have their own bounded timeouts; leases are not silently extended; work is cancelled before expiry | Pool size 1 with a blocking holder |
| FM-11 | Database clock steps forward | All leases expire together; fencing keeps writes safe | Manipulate via test hook on the SQL clock function, not the host clock |
| FM-12 | Async replica failover loses recent commits | Documented: completions may be lost and work rerun; token epoch bump prevents token reuse | Simulated by restoring an older snapshot plus epoch bump |
| FM-13 | PgBouncer transaction pooling | Only transaction-scoped locks and `SET LOCAL` used; `LISTEN` disabled with polling fallback | Run suite behind PgBouncer in CI (optional job) |

## Idempotency

| ID | Scenario | Expected behaviour | How to test |
| --- | --- | --- | --- |
| FM-14 | Two concurrent requests with the same key | One `Acquired`, one `InFlight` | Parallel `Begin` |
| FM-15 | Same key, different body | `ErrRequestMismatch`, also after completion | |
| FM-16 | Synchronous request crashes while in flight | Key's short lease expires; a retry acquires it with a higher token; the old request's `Complete` is rejected | Hook between work and `Complete` |
| FM-17 | Key purged, then reused | Treated as new; token still higher than any earlier token | Purge then resubmit |
| FM-18 | Crash between key insert and execution insert | Impossible by construction (one transaction); test kills the connection between statements and asserts neither row exists | Fault driver |

## Outbox and inbox

| ID | Scenario | Expected behaviour | How to test |
| --- | --- | --- | --- |
| FM-19 | Two producers on one aggregate, the first commits late | Sequence order equals commit order; no gaps after rollback | Two transactions, controlled commit order |
| FM-20 | Relay crashes after publishing, before advancing | Suffix republished; sequenced inbox drops duplicates | Hook `AfterPublishBeforeAdvance` |
| FM-21 | Two relays on one stream (pause mid-batch) | Fenced pointer update rejects the stale relay; broker may hold duplicates and interleaving; sequenced inbox applies exactly 1..N | Recording publisher feeding a sequenced inbox |
| FM-22 | Head event fails forever | `Block`: stream blocked, others continue, alert fires. `Park`: gap marker, inbox advances. `Stop`: manual resume only | Per policy |
| FM-23 | Partial batch publish | Pointer advances exactly to the published prefix | Publisher fails at index k |
| FM-24 | Inbox receives `seq > last + 1` | Buffered; applied automatically when the gap fills; gap timeout moves it to dead letters | Deliver 1, 3, 2 |
| FM-25 | Versioned inbox receives an older version | Treated as duplicate, not applied | |
| FM-26 | Handler rollback after dedup insert | Dedup record rolls back too; redelivery is processed | Handler returns error |

## Scheduler and cache

| ID | Scenario | Expected behaviour | How to test |
| --- | --- | --- | --- |
| FM-27 | Several scheduler instances on one tick | One job per tick | Parallel tickers on fake clock |
| FM-28 | Scheduler down for several ticks | Behaviour follows the missed-run policy | Advance fake clock |
| FM-29 | Cache stampede | One compute; others served stale or wait per policy | 100 parallel callers |
| FM-30 | Slow compute finishes after a newer value was written | Stale write rejected by version | Hook delays first compute |

## Operations

| ID | Scenario | Expected behaviour | How to test |
| --- | --- | --- | --- |
| FM-31 | Deploy (SIGTERM) with in-flight work | Drain within the timeout, hand back the rest (`run_at = now`, lease released); no waiting for expiry | Cancel worker context |
| FM-32 | Retry storm after an outage | Jitter spreads `run_at`; per-kind concurrency caps hold | Many due jobs at once |
| FM-33 | Queue table bloat | Claim latency stays bounded with partial indexes and purging | Benchmark, not a unit test |
| FM-34 | Payload version drift | Old-version executions still decode; janitor reports oldest live version | Submit v1, register v2 handler |

## Test coverage

Tests run against a real PostgreSQL (`go test -race ./...` with
`DURABLE_TEST_DSN`). Pauses are created with `durable.Hooks` and
`durabletest.Gate`, plus short real TTLs.

| ID | Status | Test |
| --- | --- | --- |
| FM-1 | Covered | `lease.TestStalePausedOwnerCannotEnterFencedTx`, `jobs.TestPausedWorkerCannotComplete` |
| FM-2 | Covered | `jobs.TestRunStepSurvivesLostLeaseAfterRemoteCall` |
| FM-3 | Covered | `lease.TestActivityCancelledWhenHeartbeatsStall`, `keys.TestStalledRequestLosesKey`, `lease.TestHeartbeatInterval` |
| FM-4 | Partial | `jobs.TestShutdownKeepsSuccessfulResult` (handler ignores cancellation and finishes); loss while ignoring cancellation is not yet tested |
| FM-5 | Not yet | Needs a subprocess or connection-kill harness |
| FM-6 | Covered | `jobs.TestPoisonJobFailsAndCompensates`, `jobs.TestReapExpiredFinalAttempt` |
| FM-7 | Partial | Completion treats "already succeeded with my token" as success; no fault-driver test yet |
| FM-8 – FM-13 | Not yet | Need a fault-injecting driver or proxy, restarts, failover drills, PgBouncer |
| FM-14 – FM-17 | Covered | `keys.TestConcurrentBegin`, `TestDoRunsOnceAndReplays`, `TestStalledRequestLosesKey`, `TestPurgeAndReuse` |
| FM-18 | Partial | `jobs.TestSubmit` (rollback leaves neither key nor execution); no connection kill between statements |
| FM-19 | Covered | `outbox.TestSequenceFollowsCommitOrder` |
| FM-20, FM-21 | Covered | `outbox.TestPausedRelayCannotAdvance`, `inbox.TestOrderedOutboxIntoSequencedInbox` |
| FM-22 | Covered | `outbox.TestHeadPolicies`, `outbox.TestShutdownDoesNotParkHead` |
| FM-23 | Covered | `outbox.TestPartialBatch`, `outbox.TestUnorderedRelayRetriesAndDeadLetters` |
| FM-24 – FM-26 | Covered | `inbox.TestSequencedBuffersAndDrains`, `TestSequencedGapMarkerAndSkipGap`, `TestDrainFailureStaysBuffered`, `TestVersioned`, `TestDedup` |
| FM-27, FM-28 | Covered | `schedule.TestConcurrentSchedulers`, `TestMissedPolicies`, `TestMissedAllCatchesUpAcrossPasses` |
| FM-29, FM-30 | Covered | `cache.TestStampedeComputesOnce`, `TestSlowComputeDoesNotOverwriteNewer`, `TestInvalidateDuringCompute` |
| FM-31 | Covered | `jobs.TestDrainHandsBack`, `jobs.TestShutdownKeepsSuccessfulResult` |
| FM-32, FM-33 | Not yet | Load and bloat benchmarks |
| FM-34 | Partial | `jobs.TestAdmin` checks `OldestPayloadVersions`; decoding old versions is the handler's job |

Regression tests from the implementation review: `lease.TestHeartbeatInterval`,
`keys.TestDoPanicReleasesKey`, `jobs.TestCancelDuringRetryableFailure`,
`TestShutdownKeepsSuccessfulResult`, `TestReapOnlyOwnKinds`,
`TestStoredModeGovernsRetries`, `TestTxModeAttemptTimeoutRecorded`,
`outbox.TestShutdownDoesNotParkHead`, `TestSlowUnorderedPublishRecorded`,
`TestReplayFencesOutHolder`, `schedule.TestMissedAllCatchesUpAcrossPasses`,
`cache.TestInvalidateDuringCompute`.
