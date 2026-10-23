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
- HTML title/body extraction omits script, style, template and noscript subtrees. PostgreSQL stores weighted title/body `tsvector` values and a GIN index.
- SIGTERM stops new claims and lets active work finish within its deadline. Hard failure leaves the lease for recovery; fetches can repeat, while fenced transactions prevent stale document commits.

`DATABASE_URL` and comma-separated `ALLOWED_ORIGINS` are required for API/workers. Compose permits only its synthetic fixture. Origins must use canonical spelling, with no trailing slash. Changing allowed origins does not remove existing indexed documents.

## Verification and boundaries

`go test -race` in the test image exercises real PostgreSQL, HTTP extraction/search, concurrent claims, stale lease rejection, two worker loops sharing a rate gate, retries, a killed worker subprocess followed by recovery, and SIGTERM during an active fetch. [Verification](docs/verification.md) records actual local results. [Architecture](docs/architecture.md) records decisions and limits.

This is a controlled-origin implementation, with no authentication, robots policy, DNS rebinding defense, global crawl budget, recrawl scheduling or production operations. Do not expose the API or point it at untrusted/public-web targets. The database and fixture are local demo services. No throughput/capacity measurement or cloud deployment is claimed.

[Kubernetes packaging](deploy/README.md) includes a local two-worker configuration. [GCP deployment boundary](deploy/gcp/README.md) describes the later image/identity/database requirements; it provisions nothing. Current milestone and next action are in [STATE.md](STATE.md).
