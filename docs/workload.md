# Controlled crawl/index workload

Run `python scripts/workload.py` from the repository. Docker Compose and Python 3 are required. The harness creates a unique project, builds the runtime once, resolves runtime/PostgreSQL images to local content-addressed IDs, then performs three repetitions with fresh containers, database volume, queue, documents and robots cache. It removes only its owned stack/volume after each run. Images, host/VM filesystem caches and other host activity are not reset. No crawler behavior or rate is changed for this experiment.

## Fixed experiment

| Item | Setting |
|---|---|
| Corpus | 24 ASCII HTML pages, exactly 16,384 bytes each; 393,216 bytes total |
| Discovery | One submitted root links to 23 leaf pages; root depth 0, leaves depth 1 |
| Content | Deterministic title/unique search token per page; repeated payload text; excluded script marker |
| Origins | One: `http://fixture:8081`, explicit private fixture exception |
| Workers | Two processes, four goroutines each |
| Rate | Existing 500 ms shared origin reservation interval; claims and page fetches both reserve slots |
| Request/retry bounds | Five-second request timeout, 20-second lease, three attempts per generation, one MiB body cap |
| Database quota | 1 CPU ceiling, 512 MiB memory, no additional container swap |
| API, fixture, each worker quota | 0.5 CPU ceiling, 128 MiB memory each, no additional container swap |
| Observation | SQL polling targets one-second periods; Docker CLI resource samples at least four seconds after the prior sample completes |
| Startup | Build/pull/startup excluded; API healthy, five containers running, empty admission ledger and two-second settling delay before submission |

CPU quotas are ceilings, not dedicated cores or reservations. The fixture, database, clients and workers share the same engine/host. Container readiness plus a settling delay is not a proof that all process initialization is finished. The accepted series has no warmup crawl or omitted repetition; the failed preliminary series is recorded separately below. Go 1.26.8, PostgreSQL 17.6 and the runtime base are declared by repository sources; the report records build-file hashes, actual image IDs/digests, engine/kernel/cgroup details, Python/OS and effective inspected resource limits. All repetitions must use the same image IDs, corpus manifest/digest and quotas. Host names and unrelated container names are omitted.

## Timing and correctness

The primary window starts immediately before the root URL HTTP POST and ends when a SQL snapshot is first observed to contain 24 jobs done and 24 indexed documents. The reported pages/second is 24 divided by this observed elapsed time; HTML bytes/second is 393,216 divided by the same time. Payload bytes exclude HTTP headers, robots, API/control traffic and database/index representation. Submission acknowledgment time is recorded separately inside the primary window. Build, service startup, manifest loading, post-completion verification and cleanup are outside it.

Each raw SQL observation includes its monotonic start/end and counts. The final completion bracket is from the preceding not-complete poll's start to the all-complete poll's end. Actual commit completion falls inside that interval; the measured endpoint overestimates it by polling/command delay. Resource commands also block polling and consume host/engine work. Actual maximum polling gaps and every sample duration are retained. Polling target values are not guarantees of one-second timing precision.

Outside the primary window the harness verifies every expected URL, depth, state, single attempt, generation and zero admission rejection. It retrieves all 24 documents, checks exact titles and independently generated expected extracted-text SHA256 values, and runs 24 unique-token searches expecting one correct document each. A common-token query must return the API's 20-result cap, and the hidden script token must return none. Fixture counters must show exactly one fetch per page, 393,216 emitted HTML bytes, correct user agents, one robots request and 24 retained admission slots. No failed/retried page, unexpected URL, OOM kill or container restart is accepted. Verification time is reported separately.

The fixture manifest identifies each raw HTML/text digest and a corpus SHA256 over the 24 raw pages concatenated in numeric path order. Corpus tests also compare actual extraction with independently constructed visible-text expectations and check the 23-link root. The dataset is synthetic, shallow and small enough to fit in cache; no external web latency, JavaScript, large index, crawl frontier diversity or failure load is represented.

## Resource observations and interpretation

Raw `docker stats --no-stream` JSON is retained for all five containers, including CPU percentage, memory usage/limit, PIDs and cumulative network/block-I/O strings. Summary maxima are only the largest collected Docker CLI CPU/memory readings for each container. They are sparse samples, not continuous peaks, process RSS, total CPU time, system-wide resource use or memory sizing guidance. On Linux the [CLI subtracts inactive file cache](https://docs.docker.com/reference/cli/docker/container/stats/) from memory usage; human-readable units are rounded before conversion to bytes. [Resource constraints](https://docs.docker.com/engine/containers/resource_constraints/) limit container resources but do not remove shared-host contention. CPU percentages retain the CLI's units; they are not normalized to the configured quota.

This origin is intentionally rate-constrained. The current implementation spends one origin reservation on each claim and another on each page request, even with a warm robots cache. At 500 ms per reservation the steady-state page ceiling is approximately one page/second, with at least 47 reservation intervals before the 24th page can begin from the first claim. The single cold robots request uses the first claim reservation. This policy, the extra claim reservations, 100 ms worker idle polling and observation/generator overhead dominate the interpretation; these measurements do not estimate parser, PostgreSQL or crawler saturation capacity.

Three repetitions support a min/median/max description of this particular fixed setup, not a statistical tail-latency or confidence-interval claim. There is no worker-count comparison and no scaling-speedup claim. A meaningful later scaling experiment would require enough explicitly permitted independent origins/work, fixed other factors and matched repeated configurations; adding replicas under this one-origin gate would not establish capacity scaling.

CI runs the same three repetitions and retains `report.json`, `observations.json` and `commands.log` as the `workload` artifact. Failed reports/raw observations remain separate; no successful result replaces a failed one. Local and hosted measurements describe different environments and must not be pooled into a speedup claim. Public-web crawling and GCP remain out of scope.

## Local observations — October 27, 2026

The [report](measurements/workload-local/report.json) and [raw observations](measurements/workload-local/observations.json) describe a dirty precommit tree based on `78d1ed4`, not a clean final revision. The client was Windows 11/Python 3.12.8; Docker Desktop 28.3.0 used Linux AMD64/WSL2, cgroup v1, with 16 exposed CPUs and 16,733,515,776 bytes of engine memory. There were zero other running containers at the initial environment snapshot; other host activity was not measured. Actual content-addressed runtime/database images and effective per-container quotas are in the report.

| Repetition | Observed elapsed (s) | Pages/s | HTML bytes/s | Submission acknowledgment (ms) | Completion bracket (s) |
|---|---:|---:|---:|---:|---|
| 1 | 24.531 | 0.978 | 16,029 | 15 | 21.015–24.531 |
| 2 | 24.407 | 0.983 | 16,111 | 16 | 20.907–24.407 |
| 3 | 24.484 | 0.980 | 16,060 | 15 | 20.984–24.484 |

Observed median: **24.484 seconds, 0.980 pages/s**. Every repetition verified 24 documents and 24 unique searches, one attempt per page, zero failures/restarts/OOM kills, one robots request and unchanged admission accounting. All three owned stacks/volumes were removed. Each repetition collected four resource samples. The maximum actual poll-start gap was 3.094 seconds; completion brackets span approximately 3.50–3.52 seconds. The 0.124-second spread between observed run endpoints is smaller than this observation uncertainty and is not evidence that one run processed faster.

Largest sampled readings across the three repetitions (each worker remains separate):

| Container role | Docker CLI CPU (%) | Docker CLI memory (MiB) |
|---|---:|---:|
| api-1 | 0.00 | 4.977 |
| db-1 | 9.08 | 57.680 |
| fixture-1 | 0.17 | 9.453 |
| worker-1 | 4.26 | 9.988 |
| worker-2 | 5.51 | 8.008 |

API CPU samples rounded to zero during this mostly worker-driven window; this does not mean API work costs no CPU. These sparse values are not peaks or proof of headroom. The small corpus, shared host, quota configuration, policy ceiling and synchronous observation overhead prevent a saturation/capacity conclusion.

Corpus version 1 SHA256: `c3b6acb5f485320c5eb6ca23d031c13b6c09287610072f4ba80bde43a5b9e9b2`. The committed LF-normalized raw-observation file SHA256 is `54c9b88f4b7e898d0785e442880ee62f7cdd5aaf8423c86c0639f2a0b7077271`. The original Windows CRLF file retained locally has SHA256 `e34868ec19a98c2cf1ca715e152d728a49a17735e8787999e355119885944e37`; only line endings differ, not parsed values. The harness now writes UTF-8/LF bytes explicitly and embeds the exact observation-file checksum in each new report, so later artifact verification is independent of host newline translation. Original local measurements and source boundaries are preserved.

A preliminary run indexed all 24 pages but failed its post-window unique-search verification. Its original alphabetic tokens included two that PostgreSQL English stemming maps to the same lexeme. That failed report/log/raw data remain separately under `.work/workload-local-20261027` and are excluded from the accepted series. The revised numeric-suffix tokens are checked for 24 distinct PostgreSQL vectors; all three fresh repetitions then passed. Docker was initially stopped and was started without changing settings before checks and measurement. No failed measurement was relabeled or discarded from the record.
