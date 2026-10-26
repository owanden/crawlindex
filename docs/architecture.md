# Architecture decisions

## One durable coordination store

PostgreSQL owns jobs, normalized-URL uniqueness, admission limits, domain reservations and documents. Each claim locks one eligible origin with `FOR NO KEY UPDATE SKIP LOCKED`, then leases one job and advances that origin's `next_at` in the same transaction. This excludes competing origin claims while remaining compatible with foreign-key key-share locks during job admission. Contending workers can claim other origins without waiting on a locked origin. Database time controls eligibility, avoiding cross-process clock differences. Polling is every 100 ms when no claim is available.

The attempt number acts as the fencing generation. Completion/failure requires the same job ID, attempt, leased state and an unexpired lease. Completion locks the job before writing its document and discovered links, so another worker cannot reclaim it while this transaction commits. There is no heartbeat: fetch timeout plus database work must fit inside the lease. Expired final attempts become terminal failures during polling. Invalid worker bounds fail startup.

PostgreSQL explicitly identifies queue consumers as a use for [SKIP LOCKED](https://www.postgresql.org/docs/17/sql-select.html). This is a job reservation mechanism, not a general consistent snapshot.

## At-least-once fetching, atomic indexing

A worker killed after fetching but before commit may fetch again after lease expiry. Remote HTTP has no exactly-once guarantee. A unique document per job and a fenced transaction prevent stale workers from replacing the current document. Links, document and `done` state commit together; an interrupted transaction rolls back. Retry failures release leases with delayed eligibility, up to three attempts. Deduplication preserves the first stored depth and terminal status. There is no recrawl/reset API.

Normalization lowercases scheme/host, removes fragments/default ports and gives empty paths `/`. It preserves query ordering, escaped paths and trailing slash distinctions. A fixed depth/two-level expansion, per-page link cap and shared retained-URL budget bound admission. The allowlist is an operator-controlled trust boundary, not protection against malicious DNS or a substitute for network egress controls.

## Durable admission

One `admission_budget` row persists the database-wide limit, initially 1,000 retained jobs. Both seed submission and discovery completion explicitly use READ COMMITTED transactions. They lock this row before acquiring job-row locks, then count jobs in a separate statement after the lock is obtained. The separate read matters: [successive READ COMMITTED statements get fresh snapshots](https://www.postgresql.org/docs/17/transaction-iso.html), including rows committed by a transaction that held the budget lock first. All application admission writers follow this lock order. [PostgreSQL lock compatibility](https://www.postgresql.org/docs/17/explicit-locking.html) allows origin reservation without blocking foreign-key key-share checks.

The job table itself is the usage ledger; an in-transaction remaining-slot value avoids recounting for each link. No independently persisted counter needs repair after rollback. URL lookup precedes exhaustion checks, so an existing normalized URL always keeps its original ID/state/depth. Only insertion of a distinct URL consumes a slot. All job states count and there is no deletion API. Serialized admission and one count per transaction favor a small, inspectable bounded queue over maximum throughput; fairness and large-table performance are not measured.

A new seed at exhaustion returns HTTP 409 with `URL admission budget exhausted`. Discovery considers distinct normalized allowed links in extraction order. Once full it skips new URLs, records their count in the completed job's `discovery_rejected`, and still commits the parent document and accepted links. Existing URLs, denied origins and depth-limited links do not count as budget rejections. Budget exhaustion is an application result, not a SQL exception that would abort the transaction. Actual database errors roll back the entire completion: parent state, document and partially admitted links. A retry reconstructs usage from retained jobs, so it neither leaks slots nor double-counts links.

Completion checks lease expiry with `clock_timestamp()` after waiting for admission, rather than transaction-start `now()`. A lease that expires during contention cannot write a document. Claim/retry/fencing behavior remains independent of admission: exhausted capacity does not prevent already admitted jobs from running or recovering.

Migration initializes the limit to the larger of 1,000 and existing job count and preserves an existing limit. Older writers must be stopped before upgrade because they do not acquire the admission lock. Direct administrative SQL can bypass application invariants; raising the stored cap is deliberate, while unsupported manual deletion/lowering is not exposed as an API. This bounds URL count, not WAL, disk bytes, per-user activity or public-web safety.

## Actual extraction and indexing

The HTML parser produces plain title/body strings and absolute links. The fetcher rejects non-HTML, non-200, oversized and redirected responses. It never executes JavaScript or follows a document's base tag. Workers recheck the configured allowlist before fetching stored jobs. Extraction is intentionally small; language/encoding detection, semantic sections and binary formats are outside this slice.

Generated weighted title/body vectors plus a GIN index implement [PostgreSQL full-text search](https://www.postgresql.org/docs/17/textsearch-controls.html). Queries use `websearch_to_tsquery`, with English stemming. Results include stored text so the index is inspectable. This is a document search path, not an external search-engine integration or relevance benchmark.

## Processes and shutdown

The same binary provides API, worker, migration and fixture commands. The runtime image has no shell and runs as nonroot. Migrations serialize with a PostgreSQL transaction advisory lock; this initial schema uses idempotent creation, not a versioned production migration framework.

Each worker goroutine claims at most one job at a time. Cancellation stops its next claim; active fetch/commit uses an independent bounded context. API shutdown waits for [Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown), including its completion, before closing the database. Container termination grace exceeds the normal active-work deadline. Database outage or a forced kill can prevent clean completion; leases provide the fallback.

## Remaining limits

Origin reservations enforce claim spacing, not perfectly timed network starts. They allow overlapping slow requests. Different schemes or ports have distinct rate budgets. Worker fairness and large-queue query cost are not measured. Retry delay is fixed, including for terminal HTTP statuses; no Retry-After handling or robots policy exists. API access is trusted/local, without authentication, per-user quotas or pagination. The retained-URL cap is not a disk quota. Kubernetes behavior is verified in disposable single-node clusters; multi-node availability is untested and GCP is undeployed.
