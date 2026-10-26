# Project state

Updated October 26, 2026.

Initial implementation complete: Go API and fixed-goroutine workers, PostgreSQL leases/deduplication/retries, bounded HTML extraction, stored documents, weighted full-text search, synthetic fixture and Docker Compose. Static checks and local integration race checks passed against PostgreSQL 17.6, including killed-process recovery, terminal lease expiry and SIGTERM drain. Packaged API, fixture and two worker containers ran successfully; stored documents/search survived simultaneous database/API/worker restart. Results and the corrected initial restart failure are in docs/verification.md.

Kubernetes milestone passed locally and in Linux CI on kind v0.30.0/server v1.34.0: actual image loading and package rollouts, two ready workers and real search, hard container exit 137 followed by pod deletion, normal-lease recovery to a new pod at attempt two, and scaling one to three workers with six completed single-attempt jobs. Owned clusters deleted; default kubeconfig unchanged. Implementation commit `041056f3c814ff57a786da26942ef9eb3f366953` passed [exact CI](https://github.com/owanden/crawlindex/actions/runs/40387657075), including the existing container/race suite. The actual 30,981-byte cluster artifact was checksum-verified and inspected, including pod readiness and credential exclusion. [Local report](docs/kubernetes-local-report.json) preserves the precommit source boundary; [hosted report](docs/kubernetes-ci-report.json) records the clean committed source. Setup/image-load/pod-deletion failures remain documented in [verification](docs/verification.md). GCP remains undeployed; no performance or production-use claim exists.

Next implementation milestone: a durable admission budget covering explicit seeds and discovered URLs under concurrent workers, with deterministic duplicate behavior and a reproducible cap-exhaustion scenario. Robots policy and network trust boundaries still precede public-web crawling. Keep this runnable slice coherent before adding distributed features.

Publication boundary: this standalone repository only. Local environment files and temporary outputs are excluded.
