# Local Kubernetes package

This directory packages the same controlled-fixture application. It is not a production database configuration. `local` contains known demo credentials, a single PostgreSQL StatefulSet/PVC, one API, one fixture and two workers. Services are cluster-local. Resource limits and 30-second termination grace are included. No ingress or cloud load balancer is created.

## Disposable cluster verification

Requires an existing working Docker Engine 28+ (API 1.48+), Python 3, and Git. The installer downloads checksum-verified kind v0.30.0 and kubectl v1.34.0 into the given directory (AMD64 Linux or Windows). It does not install system-wide tools. The smoke harness pins the Kubernetes v1.34.0 node image by digest from the [kind release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.30.0). It exports an explicit Linux/AMD64 archive before loading, avoiding incomplete multi-platform indexes with Docker's containerd image store; see [kind's known issue](https://kind.sigs.k8s.io/docs/user/known-issues/).

```sh
python scripts/install_kubernetes_tools.py .work/tools
python scripts/kubernetes_smoke.py --kind "$PWD/.work/tools/kind" --kubectl "$PWD/.work/tools/kubectl" --output .work/kubernetes
```

On Windows, pass absolute paths to `.work/tools/kind.exe` and `.work/tools/kubectl.exe`. The output directory must not exist; select a fresh path for each run. Reports and diagnostics are retained on failure.

The harness generates a unique `crawlindex-smoke-*` cluster name, writes a separate kubeconfig, loads uniquely tagged application and PostgreSQL images, applies the actual local package and waits for rollouts. Every Kubernetes operation specifies that kubeconfig and context explicitly. It verifies:

- Two ready worker pods and the real submission, deduplication, extraction, retrieval and full-text search path.
- Scale-down to one ready worker; `/gate` holds a fixture fetch while the harness stops its actual container through the owned node's CRI, verifies exited state/code 137, then force-deletes the pod. The job must remain leased at attempt one immediately afterward. The replacement must complete it at attempt two after lease expiry, with exactly two fixture requests and the expected indexed document. The observed wait must span the normal lease rather than a fast HTTP retry.
- Scale-up to three ready worker pods, followed by six new completed jobs, each with one attempt. This tests replica operation, not capacity, fairness or a speedup.

The fixture command alone exposes `/gate`, `/gate-status` and `/gate-release`. The API/worker commands expose no gate control. This is a synthetic failure boundary; it is not a normal crawl target. Normal runtime lease/fetch settings remain 20/five seconds. The explicit runtime stop matters: [force deletion does not confirm process termination](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_delete/).

The owned cluster is deleted in cleanup, including failed creation attempts. The default kubeconfig hash is checked before and after. Reports, bounded logs, pods, events and rendered demo manifests are uploaded by CI; kubeconfig credentials are excluded. A hard interruption can prevent cleanup; the report/log records the unique owned name for explicit recovery. The harness never selects an existing cluster or changes a current context.

October 26 initially encountered a Docker Desktop inference-service socket startup failure; the engine later became available without resetting host settings. The dedicated Kubernetes CI job executes this same harness on a Linux runner. Actual accepted results and preserved failures belong in [verification](../docs/verification.md).

## Existing disposable local cluster

Render without contacting a cluster:

```sh
kubectl kustomize deploy/local
```

For an explicitly selected disposable local cluster, build and load `crawlindex:local` using that cluster's image-loading command, then:

```sh
docker build -t crawlindex:local .
kubectl --context YOUR_LOCAL_CONTEXT apply -k deploy/local
kubectl --context YOUR_LOCAL_CONTEXT -n crawlindex rollout status deployment/api
kubectl --context YOUR_LOCAL_CONTEXT -n crawlindex rollout status deployment/worker
kubectl --context YOUR_LOCAL_CONTEXT -n crawlindex port-forward service/api 8080:8080
```

In another terminal run `python scripts/demo.py`. Then verify pod deletion/recovery and worker scaling in that cluster before claiming Kubernetes validation. PVC deletion and cluster lifecycle are deliberate operator actions, outside the simple demo script.
