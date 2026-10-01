# Install on Kubernetes

## Prerequisites

- Kubernetes 1.24+ and `kubectl`
- Helm 3.8+ (OCI registry support)
- A default (or named) `StorageClass` for durable clusters — see [ephemeral storage](../crds/riakcluster.md#storage) for test clusters without one
- [cert-manager](https://cert-manager.io) for TLS and for **every** `RiakUser`

## 1. Install cert-manager

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook
```

## 2. Install the operator

```bash
helm install openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator \
  --namespace openriak-system --create-namespace
```

Useful values (full list in the chart README):

| Value | Default | Purpose |
|-------|---------|---------|
| `riak.image` | `ghcr.io/marthydavid/riak:3.2.6` | Default operand image when `spec.image` is omitted |
| `maxConcurrentReconciles` | `1` | Parallelism per controller — see [Scaling](../scaling.md) |
| `metrics.serviceMonitor.enabled` | `false` | Create a ServiceMonitor for the operator (needs Prometheus Operator CRDs) |
| `replicaCount` | `1` | Manager replicas (leader election handles >1) |

Verify:

```bash
kubectl -n openriak-system get pods
kubectl get crd | grep riak.openriak.io
```

!!! warning "CRDs are not upgraded by Helm"
    Helm installs `crds/` on first install only. When upgrading across a CRD change, apply them
    yourself and use server-side apply, because the schemas are large:

    ```bash
    kubectl apply --server-side -f https://raw.githubusercontent.com/marthydavid/openriak-operator/main/config/crd/bases/riak.openriak.io_riakclusters.yaml
    ```
    Repeat for `riakusers` and `riakbuckets`. `helm uninstall` also leaves the CRDs and every Riak
    resource in place.

## Alternative: kustomize from a checkout

```bash
make install                                   # CRDs
make deploy IMG=ghcr.io/marthydavid/openriak-operator:0.0.8
```

## Next

[Quick start](quickstart.md) · [Vanilla Kubernetes reference](../platforms/kubernetes.md)
