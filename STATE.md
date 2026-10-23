# Project state

Updated October 23, 2026.

Initial implementation complete: Go API and fixed-goroutine workers, PostgreSQL leases/deduplication/retries, bounded HTML extraction, stored documents, weighted full-text search, synthetic fixture and Docker Compose. Static checks and local integration race checks passed against PostgreSQL 17.6, including killed-process recovery, terminal lease expiry and SIGTERM drain. Packaged API, fixture and two worker containers ran successfully; stored documents/search survived simultaneous database/API/worker restart. Results and the corrected initial restart failure are in docs/verification.md.

Kubernetes manifests render locally; no cluster context exists on the implementation machine. GCP remains a later deployment target; nothing has been provisioned. No benchmark or production-use claim exists.

Next milestone: validate the Kubernetes package in an explicitly selected disposable local cluster, including image loading, readiness, two-worker operation, pod deletion/recovery and scaling. Before broadening beyond controlled fixtures, implement crawl admission budgets, robots policy and network trust boundaries. Keep this runnable slice coherent before adding distributed features.

Publication boundary: this standalone repository only. Local environment files and temporary outputs are excluded.
