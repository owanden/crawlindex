# Initial verification

October 23, 2026, Docker Desktop Linux engine 28.3.0 on Windows. Host Go is absent; builds and tests use `golang:1.26.8-bookworm`. PostgreSQL image: `postgres:17.6-bookworm`. No performance measurements were taken.

First PostgreSQL integration run: `docker compose --profile test run --build --rm test` succeeded, crawler package 7.660 seconds, using the Go race detector. It exercised extraction/policy, HTTP response bounds, API submission/status/retrieval/search, normalized-URL deduplication, concurrent exclusive claims, expired/stale completion and failure fencing, two-worker origin coordination, retry success/exhaustion, actual child-process kill/recovery and SIGTERM during a gated active fetch. It uses temporary schemas, not a mocked database. Tests without `TEST_DATABASE_URL` explicitly skip database cases.

`kubectl kustomize deploy/local` succeeded, producing a 4,697-byte rendered package on the first render. No Kubernetes context is configured; no cluster deployment was attempted. GCP is undeployed.

Final runtime checks are recorded below.

Follow-up review caught an empty-queue transaction issue: marking an expired final lease failed was rolled back when no new job could be claimed. Added a regression requiring terminal state to persist with no other work and committed the cleanup transaction on that path. A simultaneous database/API/worker restart also reproduced immediate startup connection failures. Startup now retries within a 15-second deadline. The initial failed restart is retained here rather than represented as successful.

Final `go vet ./...` and `go test -race -v -count=1 ./...` in the Compose test image succeeded; crawler package 7.666 seconds, including `TestFinalExpiredLeaseBecomesTerminalWithoutOtherJobs`. Tests repeat HTTP fetches after a real killed subprocess; they do not simulate a process failure solely by editing database timestamps. The fencing/terminal-expiry checks separately edit lease times to test precise state boundaries.

Built the final distroless runtime and ran the API, fixture, database and two worker containers. Runtime UID was 65532. `python scripts/demo.py` succeeded before and after `docker compose restart db api worker`: root job remained ID 1, done, attempt 1; both PostgreSQL search results and retrieved text were unchanged. [Saved synthetic output](demo-output.json) contains the actual first successful runtime receipt. Search omitted `invisibleword` from the fixture script. The first host verification attempt required local socket access outside the execution sandbox; the authorized rerun succeeded. No application network workaround was introduced.

This verifies a small workload with two running replicas and shared durable state, not scaling efficiency, capacity, worker fairness or Kubernetes operation. Process recovery tests use a shortened 2.5-second lease/one-second fetch timeout; the normal runtime uses 20/five seconds. No benchmark or GCP deployment is implied.

## October 26 Kubernetes verification work

Added a disposable kind v0.30.0/Kubernetes v1.34.0 harness and a fixture-only fetch gate. Tool downloads are checksum-verified; application images are built for Linux/AMD64 and loaded through a single-platform archive. Each run uses a new cluster name, explicit context and separate kubeconfig. The CI artifact selects reports/diagnostics explicitly and excludes kubeconfig credentials and image archives.

Preserved failed local runs before accepting a result:

- Docker Desktop initially could not start its inference-service socket. It later became available without a reset or host-settings change. No cluster result was claimed from the startup failure.
- The first command-capture implementation hit a Windows default-codepage decoding failure on kind's Unicode output. File-based UTF-8 capture now retains output and bounds command waits. Its partially created cluster was deleted and the default kubeconfig stayed unchanged.
- Direct kind image loading failed on an incomplete image-index digest in Docker's containerd store. Explicit Linux/AMD64 build/export plus `kind load image-archive` passed that boundary on the next run.
- The first pod-deletion experiment correctly failed its recovery assertion: the API pod record disappeared before the process stopped, and the old worker completed attempt one when the gate opened. The harness now stops the actual owned container via CRI, requires exited state/code 137, and then deletes its pod. It checks that the job is still leased at attempt one before allowing the replacement's fetch.

Local Go static checks and tests passed with the new fixture gate; database cases were explicitly skipped in that initial native run. Once Docker became available, the complete PostgreSQL/static/race suite passed in containers: command package 1.025s, crawler package 7.756s.

Accepted local run: `crawlindex-smoke-7b63522b`, October 26, 19:29:28..19:31:22 UTC, Docker Desktop Linux 28.3.0 hosted on Windows/AMD64, kind v0.30.0, matching kubectl/server v1.34.0. [Actual report](kubernetes-local-report.json) records the precommit dirty source tree based on a3ede43; it does not represent these new checks as having run against clean a3ede43.

The actual package rolled out with two ready worker pods; submission, fragment deduplication, document retrieval and PostgreSQL full-text search passed. After scaling down to one worker, the runtime stop reported `CONTAINER_EXITED`/137. Immediately after deleting that pod, job 7 was still leased at attempt one with no error. A different pod UID completed it at attempt two; fixture request count was exactly two and the indexed title was `Recovered lease`. The observed wait from runtime stop to recovery was 20.093s with the normal 20-second lease/five-second fetch timeout. This is a lease-boundary observation, not a recovery-latency benchmark.

Scale-up reached three ready worker pods. Six new seed URLs all completed at attempt one. Worker fairness, maximum capacity, rate-policy expansion across registrable domains and multi-node/node-loss behavior were not measured. The owned cluster was deleted, and the default kubeconfig hash was unchanged. All failed runs above were retained separately under local ignored `.work` output directories. No GCP resource or public service was deployed.

Hosted repetition: clean implementation `041056f3c814ff57a786da26942ef9eb3f366953` passed [CI 40387657075](https://github.com/owanden/crawlindex/actions/runs/40387657075). Both jobs succeeded: existing PostgreSQL/static/race/Compose restart checks and the actual Kubernetes harness. The Linux/AMD64 runner used Docker 28.0.4, kind v0.30.0 and Kubernetes/kubectl v1.34.0. [Actual hosted report](kubernetes-ci-report.json) records cluster `crawlindex-smoke-7916fe75`, verified container exit 137, leased attempt one immediately after pod deletion, attempt-two completion under a different pod UID, exactly two fixture fetches, and an observed 20.134s lease wait. Three workers were ready and all six scaled-workload jobs completed at attempt one. Cleanup and unchanged default kubeconfig were verified again.

Downloaded and independently inspected the actual `kubernetes-smoke` artifact, ID 12682146728: 30,981 bytes, SHA256 `494e6496616c29355e2be5367367f440ab6163cbfd880c998ed6be121999332b`. The archive contained exactly `report.json`, `commands.log`, `pods.json`, `events.json`, `worker.log`, and `rendered.yaml`; no kubeconfig or image archive. Report revision/clean source, runtime exit state, replacement UID, lease states/attempts, six completed jobs, three actual ready pod records and cleanup matched. These results show repeatable behavior in disposable single-node clusters on two hosts, not a multi-node availability or cloud deployment claim.
