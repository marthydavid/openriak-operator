# Install on Kubernetes

## Prerequisites

- Kubernetes 1.24+ and `kubectl`
- Helm 3.8+ (OCI registry support)
- A default (or named) `StorageClass` for durable clusters — see [ephemeral storage](../crds/riakcluster.md#storage) for test clusters without one
- [cert-manager](https://cert-manager.io) for the cluster's TLS certificate and for `RiakUser` certificates unless you bring them from an [external CA](../mtls.md#client-certificates-from-an-external-ca)

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

!!! note "CRDs are part of the release"
    The chart renders the CRDs from its templates, so `helm upgrade` updates them with the operator.
    `helm uninstall` leaves them, and every Riak resource, in place (`crds.keep`). To manage the CRDs
    yourself set `crds.install=false`. Upgrading from a chart that shipped them in `crds/` needs a
    one-time adoption: see [the chart README](https://github.com/marthydavid/openriak-operator/blob/main/charts/openriak-operator/README.md#upgrading-from-an-earlier-chart).

## Alternative: kustomize from a checkout

```bash
make install                                   # CRDs
make deploy IMG=ghcr.io/marthydavid/openriak-operator:1.0.0
```

## Next

[Quick start](quickstart.md) · [Vanilla Kubernetes reference](../platforms/kubernetes.md)
