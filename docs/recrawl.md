# Explicit recrawl

Read `GET /jobs/{id}`, then send its `generation` to `POST /jobs/{id}/recrawl`. Generation starts at one. A terminal `done` or `failed` job advances exactly one generation, queues immediately, clears its current error/discovery count and resets attempts to zero. The response is HTTP 202 with the current job object. The same ID, normalized URL, origin, stored depth and admission slot are reused, even at an exhausted URL budget.

```sh
curl http://127.0.0.1:8080/jobs/1
curl -X POST http://127.0.0.1:8080/jobs/1/recrawl \
  -H 'Content-Type: application/json' -d '{"generation":1}'
```

Use the actual ID and observed generation, rather than assuming these example values. The request is one JSON object, at most 4,096 bytes, with only a positive integer `generation` below 9223372036854775807. Invalid input returns 400, unknown IDs 404, and an origin removed from the allowlist 400. The worker rechecks its own allowed-origin policy as usual.

| Stored generation/state | Request generation | Result |
|---|---|---|
| N, done or failed | N | Queue N+1, attempts zero; 202 |
| N, queued or leased | N | 409; no lease cancellation or retry-delay reset |
| N+1, any state | N | Idempotent duplicate; 202 with current N+1, no mutation |
| Any other mismatch | N | 409; read the current job before another decision |

This conditional request prevents concurrent callers from creating multiple refreshes even if the first refresh finishes before later duplicates arrive. A duplicate can return an already done or failed job: 202 acknowledges the existing request, not a successful fetch or a fresh retry. The immediately previous generation remains recognizable; this is not an unlimited idempotency-key history. To refresh again after a terminal result, submit that result's current generation. Repeated seed submission through `POST /urls` continues to deduplicate without refreshing.

## Atomic state and content

A READ COMMITTED transaction locks the existing job row with `SELECT FOR UPDATE`, then compares the generation/state and either returns the existing result or changes the row. Concurrent refresh requests serialize on this row; [PostgreSQL returns the updated locked version](https://www.postgresql.org/docs/17/transaction-iso.html#XACT-READ-COMMITTED). Recrawl does not acquire admission/domain locks or insert a URL. Existing completion keeps its admission-before-job lock order; the refresh transaction needs no later admission lock.

Claims carry both generation and attempt. Completion, failure/retry and robots denial require matching job ID, generation, attempt, leased state and a lease still valid at the actual update time. This extra generation guard matters because attempt one repeats on each refresh: an old attempt-one worker must not write into a new generation's attempt-one lease. A stale completion rolls back before document/discovery writes. Fencing prevents stale database commits; it does not undo remote requests already made by a lost worker. Expired leases recover within their current generation through the existing claim path.

The last successfully indexed document remains visible in document retrieval and search during queueing, an active refresh, retries, and terminal failure, including a later robots denial. A document's `generation` identifies the successful generation that produced its text; compare it with the job's current generation to detect older content. A URL that never indexed still has no document. Successful completion atomically replaces text, generated search vectors, document generation and job state. Old terms disappear from the index at that commit. There is one current document per URL, not a revision archive or automatic purge policy.

Depth remains the first stored depth. Discovery on successful refresh still considers at most 32 links, respects depth/origin policy and the shared remaining admission budget, and records newly rejected links. Previously admitted linked jobs remain retained and deduplicated; they are not recursively refreshed or removed when a link disappears. Robots cache TTLs, origin reservations, connection-time destination checks, five-second request bounds, body limits and three attempts per generation remain unchanged. Explicit future refreshes can issue more requests; the URL cap is not a lifetime request quota.

Stop all older API/worker binaries before upgrading. Migration adds generation one to existing job/document rows and preserves existing state/content/budget. Old workers lack the generation fence and must not run concurrently with refresh requests. Startup's advisory migration lock does not replace this operator requirement.

## Reproduce

```sh
python scripts/recrawl_smoke.py
```

The isolated two-worker fixture uses one exhausted admission slot. Sixteen concurrent requests coalesce into one queued generation, which survives a database/API restart while workers are stopped. Changed fixture text replaces the prior search result. A later page-error generation reaches failed after three real requests, survives another database/API restart and leaves prior indexed content visible. Expiring the test policy cache then denying robots produces no extra page request; an explicit later refresh recovers. The harness removes its owned stack/volume and retains source-stamped report/log files. Test-only `/refresh-control` and `/refresh-status` exist solely in the fixture command. Actual PostgreSQL tests separately exercise 24 concurrent HTTP requests, active/queued conflicts, stale success/failure/denial fences, reconnected state, full-budget discovery and refresh-specific egress denial.

There is no recrawl scheduler, cancellation endpoint, document-history store, authentication or per-client refresh quota. Keep the API local and public-web targets out of scope. This milestone is behavior verification; it is not a throughput, search-quality or production-use claim.
