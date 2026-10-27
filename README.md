# CrawlIndex

A Go crawler and PostgreSQL indexer for controlled HTML origins. Submit a URL, inspect its durable job, retrieve extracted text, and search a real full-text index. API and worker processes share PostgreSQL; workers coordinate leases and per-origin rate reservations through database transactions.

## Run locally

Requires Docker Compose and Python 3 for the verification script. No host Go or PostgreSQL installation is needed.

```sh
docker compose up --build -d --scale worker=2 api worker fixture
python scripts/demo.py
```

The API binds to `127.0.0.1:8080`. The demo submits `http://fixture:8081/`, verifies fragment deduplication, waits for three indexed pages, retrieves the root document, and checks two PostgreSQL search results. It also verifies script text is absent from the index. Running it again reuses stored jobs and documents.

```sh
curl -X POST http://127.0.0.1:8080/urls -H 'Content-Type: application/json' -d '{"url":"http://fixture:8081/"}'
curl http://127.0.0.1:8080/jobs/1
curl http://127.0.0.1:8080/documents/1
curl 'http://127.0.0.1:8080/search?q=PostgreSQL'
```

Use the ID returned by submission. Search uses PostgreSQL's English web-search syntax and returns at most 20 documents ordered by rank, then job ID. Health is at `/healthz`.

```sh
docker compose restart api worker
python scripts/demo.py
docker compose --profile test run --build --rm test
docker compose down
```

`down` retains the database volume. Only use `docker compose down -v` when intentionally resetting local crawl data. Tests use temporary schemas in the Compose database and remove them afterward.

API and worker startup retry database connections within a 15-second deadline, including during a simultaneous database/application restart. A longer outage still requires restarting failed processes.

## Behavior

- Four fixed goroutines per worker by default; `WORKER_CONCURRENCY` accepts 1..64. No in-memory unbounded job channel.
- Unique normalized URL records, 20-second fenced leases, three attempts, and one-second retry delay. An expired lease can be reclaimed by another process. Completed document storage and discovered links commit with the job transition.
- Database-coordinated reservations at least 500 ms apart for each origin across all workers. An origin means scheme, hostname and port, rather than registrable domain. Network scheduling can shift actual request-start spacing.
- Five-second HTTP timeout; one MiB body cap; no redirects; explicit allowed origins. Crawl depth is capped at two, with at most 32 discovered links per page.
- A persisted database-wide budget defaults to 1,000 retained URLs across every job state. Seeds and discovered links share it. Duplicates retain their IDs and consume no additional capacity. New seeds at exhaustion return HTTP 409; indexing of admitted pages still completes, with `discovery_rejected` counting distinct normalized links rejected by the budget.
- Workers consult shared, persisted robots.txt rules before page requests. A denied job becomes terminal without fetching or indexing its page; temporary policy errors defer jobs through the durable retry queue.
- HTML title/body extraction omits script, style, template and noscript subtrees. PostgreSQL stores weighted title/body `tsvector` values and a GIN index.
- SIGTERM stops new claims and lets active work finish within its deadline. Hard failure leaves the lease for recovery; fetches can repeat, while fenced transactions prevent stale document commits.

`DATABASE_URL` and comma-separated `ALLOWED_ORIGINS` are required for API/workers. Compose permits only its synthetic fixture. Origins must use canonical spelling, with no trailing slash. Changing allowed origins does not remove existing indexed documents.

## Admission budget

`GET /admission` returns `limit`, `admitted` and `remaining`. Completed/failed jobs keep their slots; restarts, retries and duplicate submissions do not reset usage. There is no deletion/reset API. The job table is the durable usage ledger, so rollback cannot leak a separate counter. Slot winners across concurrent transactions depend on database scheduling; within a page, links are considered in extraction order.

For an existing installation, stop older API/worker binaries before upgrading: older writers do not participate in admission locking. Migration preserves all jobs, initializes the cap to at least their count, and never replaces an already stored limit. If more than 1,000 jobs already exist, the initial cap equals that count and blocks new URLs. An operator can explicitly raise the stored limit; service environment variables cannot silently override it. For example, in the local demo:

```sh
docker compose exec -T db psql -U crawler -d crawler -c 'UPDATE admission_budget SET max_urls=2000 WHERE id=1 AND max_urls<2000'
```

To reproduce exhaustion and a database/API/worker restart without changing the ordinary demo database:

```sh
python scripts/admission_smoke.py
```

This creates a fresh, uniquely named Compose project with a two-URL cap, two workers and a random loopback API port. The root document indexes with one discovered URL rejected; concurrent duplicates remain accepted and new seeds receive 409. The exhausted state and document/search results must survive restart. Reports and bounded logs stay under `.work`; cleanup removes only the owned disposable stack and its volume. Supply `--output` with a fresh directory to retain a named result.

## Verification and boundaries

`go test -race` in the test image exercises real PostgreSQL, HTTP extraction/search, concurrent claims, stale lease rejection, two worker loops sharing a rate gate, retries, a killed worker subprocess followed by recovery, and SIGTERM during an active fetch. [Verification](docs/verification.md) records actual local results. [Architecture](docs/architecture.md) records decisions and limits.

This is a controlled-origin implementation with a [connection-time DNS/IP boundary](docs/egress.md), but no authentication, recrawl scheduling or production operations. The admission cap bounds retained URL count, rather than disk bytes or requests per user. Do not expose the API or point it at untrusted/public-web targets. The database and fixture are local demo services. No throughput/capacity measurement or cloud deployment is claimed.

[Kubernetes packaging and disposable verification](deploy/README.md) include a two-worker configuration and an isolated kind harness for real pod-loss recovery and scaling checks. Accepted execution results are recorded in [verification](docs/verification.md). [GCP deployment boundary](deploy/gcp/README.md) describes the later image/identity/database requirements; it provisions nothing. Current milestone and next action are in [STATE.md](STATE.md).

## Robots policy

Requests use `CrawlIndex/0.1 (controlled-origin crawler)` with product token `CrawlIndex`. Matching delegates to pinned `github.com/jimsmart/grobotstxt` v1.0.3, a native Go port of Google's matcher, with percent-encoded unreserved-octet normalization and tested rule vectors. [Policy decisions](docs/architecture.md#robots-decisions) document the RFC basis and conservative deviations; this is not a full protocol-conformance claim.

| robots.txt result | Page behavior | Shared cache |
|---|---|---|
| 200 valid plain UTF-8 text, or empty 204 | Apply matching rules; a denial is terminal at its current attempt | One hour |
| 4xx except 429 | Allow as unavailable policy | One hour |
| 429, 5xx, network failure, redirect, invalid/oversized response | Fetch no page; retry after the cached error expires, up to three attempts | Ten seconds |

Policy requests use the exact allowed origin, the same no-proxy/no-redirect HTTP client and five-second per-request timeout, a one MiB body cap, and a 16,000-byte line cap before and after encoding normalization. Policy metadata stays in already admitted origin rows, consuming no extra URL slots. Policy/denied jobs still count toward the retained-URL budget. Restarting processes does not clear the cache or queued retry delay. Already terminal jobs are not automatically reopened when rules change, and existing indexed documents are not retroactively removed.

```sh
python scripts/robots_smoke.py
```

This uses a disposable project and local fixture controls to verify denial without any page fetch/indexing, an allowed exception, shared cache reuse through restart, a 503 retry through another restart, and cached 404 handling. It removes its own stack/volume and retains a report/log under `.work`. The fixture-only `/robots-control` and `/robots-status` endpoints do not exist in the API/worker commands. Public-web crawling remains out of scope.

## Connection-time egress

Both page and robots requests resolve once per new connection, reject denied or mixed destination answers and dial a checked numeric address. Host/TLS identity, certificate verification and existing request/body bounds are preserved. Private, loopback, link-local/metadata, mapped-private, translation and special-purpose destinations are denied by default. [Exact behavior and limits](docs/egress.md) include static address ranges, pooled connections and the absence of a network-layer guarantee.

The local Compose/Kubernetes fixtures explicitly enable `CONTROLLED_FIXTURE_ORIGIN=http://fixture:8081`; this requires that exact origin as the sole allowed origin and permits only private fixture answers at that host/port. It is not an arbitrary local-network bypass. `python scripts/egress_smoke.py` verifies zero fixture traffic without the exception, then normal durable retry and indexing with it enabled, in an owned disposable stack.
