# Local Kubernetes package

This directory packages the same controlled-fixture application. It is not a production database configuration. `local` contains known demo credentials, a single PostgreSQL StatefulSet/PVC, one API, one fixture and two workers. Services are cluster-local. Resource limits and 30-second termination grace are included. No ingress or cloud load balancer is created.

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

In another terminal run `python scripts/demo.py`. Then verify pod deletion/recovery and worker scaling in that cluster before claiming Kubernetes validation. The local environment at initial implementation had no configured Kubernetes context; only rendering was verified. PVC deletion and cluster lifecycle are deliberate operator actions, outside the demo script.
