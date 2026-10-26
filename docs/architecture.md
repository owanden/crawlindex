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

## Robots decisions

[RFC 9309](https://www.rfc-editor.org/rfc/rfc9309.html) supplies the grouping/matching, unavailable-versus-unreachable and cache guidance. Ordinary 4xx responses may permit crawling; server/network errors require a restrictive decision. The implementation uses a pinned [native Go port of Google's matcher](https://github.com/jimsmart/grobotstxt/tree/v1.0.3), rather than a new rule parser. Its license is included at `/licenses/grobotstxt-LICENSE` in the runtime image. Vectors cover case-insensitive product groups and merged groups, wildcard fallback, longest matching rule, allow-on-tie, wildcard/end anchors, Unicode and escaped paths. A wrapper normalizes percent-encoded unreserved bytes and hex case in paths/rules.

Transport and failure choices are explicit: 200 `text/plain` UTF-8 bodies and empty 204 responses yield rules; ordinary 4xx means unavailable/allow. Status 429, server/network errors, redirects, unexpected status/media type, invalid UTF-8/NUL, oversized bodies and excessive lines defer all page requests. Body limit is one MiB; line limit is 16,000 bytes both before and after encoding expansion, below the matcher's truncation threshold. Conservative rejection avoids applying silently truncated rules. Redirects stay disabled even though the RFC recommends following them, preserving the exact-origin network boundary. This implementation does not claim full RFC conformance. Crawl-delay, sitemap discovery, HTTP cache validators/Cache-Control and Retry-After are not implemented.

Cache fields live on `domains`, so no new origin can enter through policy lookup without prior admission. A READ COMMITTED transaction locks the origin row, reads freshness using database wall time and refreshes at most once across contending workers. The transaction deliberately holds its connection/row lock during the bounded HTTP fetch; other origins can still claim work. Successful/unavailable policy lasts one hour. Error policy lasts ten seconds and discards stale good rules; page fetching is deferred while it remains undefined. A stored error's remaining lifetime becomes a lower bound on the durable retry delay. The existing attempt cap eventually produces terminal failure. A matching disallow instead transitions immediately to failed with `robots disallowed`, fenced by lease generation and actual expiry.

The job's origin reservation covers its initial policy fetch. An allowed page needs an additional origin slot, including warm-cache cases. This conservative scheduling can reduce crawl rate; it is not a throughput improvement or precise network-start guarantee. Default 20-second leases/five-second per-request timeouts remain; configuration validation now budgets for both policy and page timeouts. Shared cache decisions and queued availability survive database/process restart. Terminal jobs keep their existing IDs/status and indexed content is not retroactively purged when policy changes.

Robots is a permission policy, not authentication or network protection. New cache rows cannot bypass URL admission, fetches cannot follow redirects off-origin, and rules never enqueue sitemaps or other targets. DNS rebinding and private/link-local destination restrictions remain a separate required milestone. Stop older worker binaries before upgrading: they do not apply these checks.

## Remaining limits

Origin reservations enforce claim spacing, not perfectly timed network starts. They allow overlapping slow requests. Different schemes or ports have distinct rate budgets. Worker fairness and large-queue query cost are not measured. Ordinary page retries keep a fixed delay; policy-error retries also respect the stored error-cache lifetime, without Retry-After handling. API access is trusted/local, without authentication, per-user quotas or pagination. The retained-URL cap is not a disk quota. Kubernetes behavior is verified in disposable single-node clusters; multi-node availability is untested and GCP is undeployed.
