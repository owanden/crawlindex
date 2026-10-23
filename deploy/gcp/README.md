# Later GCP deployment target

No GCP resources have been provisioned. There is no deployed service URL or measured GCP behavior. Local manifests are not evidence of a cloud deployment.

The application boundary is a nonroot container with separate `api` and `worker` commands, PostgreSQL connectivity through `DATABASE_URL`, and an explicit `ALLOWED_ORIGINS` trust list. Before a GKE deployment, choose an authorized project/region, image repository and immutable image digest; configure workload identity and secret delivery; provide a private PostgreSQL connection with suitable TLS and credentials; and decide authentication, egress, robots policy and crawl admission limits. Do not carry local demo credentials into GCP.

Build/test commands are in the root README. A future GCP overlay should replace the local PostgreSQL StatefulSet with the selected database connection, replace image references with the published digest, remove the fixture unless testing, and add the approved network/identity policies. Validate SIGTERM, forced worker loss and two-replica coordination there before recording a deployment milestone. Resource creation, paid services and public ingress require separate authorization.
