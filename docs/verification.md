# Initial verification

October 23, 2026, Docker Desktop Linux engine 28.3.0 on Windows. Host Go is absent; builds and tests use `golang:1.26.8-bookworm`. PostgreSQL image: `postgres:17.6-bookworm`. No performance measurements were taken.

First PostgreSQL integration run: `docker compose --profile test run --build --rm test` succeeded, crawler package 7.660 seconds, using the Go race detector. It exercised extraction/policy, HTTP response bounds, API submission/status/retrieval/search, normalized-URL deduplication, concurrent exclusive claims, expired/stale completion and failure fencing, two-worker origin coordination, retry success/exhaustion, actual child-process kill/recovery and SIGTERM during a gated active fetch. It uses temporary schemas, not a mocked database. Tests without `TEST_DATABASE_URL` explicitly skip database cases.

`kubectl kustomize deploy/local` succeeded, producing a 4,697-byte rendered package on the first render. No Kubernetes context is configured; no cluster deployment was attempted. GCP is undeployed.

Final runtime checks are recorded below.

Follow-up review caught an empty-queue transaction issue: marking an expired final lease failed was rolled back when no new job could be claimed. Added a regression requiring terminal state to persist with no other work and committed the cleanup transaction on that path. A simultaneous database/API/worker restart also reproduced immediate startup connection failures. Startup now retries within a 15-second deadline. The initial failed restart is retained here rather than represented as successful.

Final `go vet ./...` and `go test -race -v -count=1 ./...` in the Compose test image succeeded; crawler package 7.666 seconds, including `TestFinalExpiredLeaseBecomesTerminalWithoutOtherJobs`. Tests repeat HTTP fetches after a real killed subprocess; they do not simulate a process failure solely by editing database timestamps. The fencing/terminal-expiry checks separately edit lease times to test precise state boundaries.

Built the final distroless runtime and ran the API, fixture, database and two worker containers. Runtime UID was 65532. `python scripts/demo.py` succeeded before and after `docker compose restart db api worker`: root job remained ID 1, done, attempt 1; both PostgreSQL search results and retrieved text were unchanged. [Saved synthetic output](demo-output.json) contains the actual first successful runtime receipt. Search omitted `invisibleword` from the fixture script. The first host verification attempt required local socket access outside the execution sandbox; the authorized rerun succeeded. No application network workaround was introduced.

This verifies a small workload with two running replicas and shared durable state, not scaling efficiency, capacity, worker fairness or Kubernetes operation. Process recovery tests use a shortened 2.5-second lease/one-second fetch timeout; the normal runtime uses 20/five seconds. No benchmark or GCP deployment is implied.
