# Architecture decisions

## One durable coordination store

PostgreSQL owns jobs, normalized-URL uniqueness, domain reservations and documents. Each claim locks one eligible origin with `FOR UPDATE SKIP LOCKED`, then leases one job and advances that origin's `next_at` in the same transaction. Contending workers can claim other origins without waiting on a locked origin. Database time controls eligibility, avoiding cross-process clock differences. Polling is every 100 ms when no claim is available.

The attempt number acts as the fencing generation. Completion/failure requires the same job ID, attempt, leased state and an unexpired lease. Completion locks the job before writing its document and discovered links, so another worker cannot reclaim it while this transaction commits. There is no heartbeat: fetch timeout plus database work must fit inside the lease. Expired final attempts become terminal failures during polling. Invalid worker bounds fail startup.

PostgreSQL explicitly identifies queue consumers as a use for [SKIP LOCKED](https://www.postgresql.org/docs/17/sql-select.html). This is a job reservation mechanism, not a general consistent snapshot.

## At-least-once fetching, atomic indexing

A worker killed after fetching but before commit may fetch again after lease expiry. Remote HTTP has no exactly-once guarantee. A unique document per job and a fenced transaction prevent stale workers from replacing the current document. Links, document and `done` state commit together; an interrupted transaction rolls back. Retry failures release leases with delayed eligibility, up to three attempts. Deduplication preserves the first stored depth and terminal status. There is no recrawl/reset API.

Normalization lowercases scheme/host, removes fragments/default ports and gives empty paths `/`. It preserves query ordering, escaped paths and trailing slash distinctions. A fixed depth/two-level expansion and per-page link cap bound each submitted seed's exploration, but repeated submissions of distinct seeds can grow the database without a global quota. The allowlist is an operator-controlled trust boundary, not protection against malicious DNS or a substitute for network egress controls.

## Actual extraction and indexing

The HTML parser produces plain title/body strings and absolute links. The fetcher rejects non-HTML, non-200, oversized and redirected responses. It never executes JavaScript or follows a document's base tag. Workers recheck the configured allowlist before fetching stored jobs. Extraction is intentionally small; language/encoding detection, semantic sections and binary formats are outside this slice.

Generated weighted title/body vectors plus a GIN index implement [PostgreSQL full-text search](https://www.postgresql.org/docs/17/textsearch-controls.html). Queries use `websearch_to_tsquery`, with English stemming. Results include stored text so the index is inspectable. This is a document search path, not an external search-engine integration or relevance benchmark.

## Processes and shutdown

The same binary provides API, worker, migration and fixture commands. The runtime image has no shell and runs as nonroot. Migrations serialize with a PostgreSQL transaction advisory lock; this initial schema uses idempotent creation, not a versioned production migration framework.

Each worker goroutine claims at most one job at a time. Cancellation stops its next claim; active fetch/commit uses an independent bounded context. API shutdown waits for [Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown), including its completion, before closing the database. Container termination grace exceeds the normal active-work deadline. Database outage or a forced kill can prevent clean completion; leases provide the fallback.

## Remaining limits

Origin reservations enforce claim spacing, not perfectly timed network starts. They allow overlapping slow requests. Different schemes or ports have distinct budgets. Worker fairness and large-queue query cost are not measured. Retry delay is fixed, including for terminal HTTP statuses; no Retry-After handling or robots policy exists. API access is trusted/local, without authentication, admission quotas or pagination. Search bodies and database size are bounded per page but not globally. Kubernetes resources are illustrative until cluster validation; GCP is undeployed.
