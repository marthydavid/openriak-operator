# OpenRiak Operator

The OpenRiak operator runs [Riak KV](https://riak.com) on Kubernetes and OpenShift. You describe
clusters, users and buckets as custom resources; the operator creates the StatefulSets, services,
certificates and Riak-side configuration and keeps them in sync.

| Resource | What it manages | Guide |
|----------|-----------------|-------|
| `RiakCluster` | A StatefulSet of Riak nodes, its Services, storage, TLS and metrics | [RiakCluster](crds/riakcluster.md) |
| `RiakUser` | A Riak security user that authenticates with an mTLS client certificate, plus its grants | [RiakUser](crds/riakuser.md) |
| `RiakBucket` | A Riak bucket type with its properties | [RiakBucket](crds/riakbucket.md) |

All three live in API group `riak.openriak.io/v1` and are namespaced. Users and buckets point at a
cluster in the same namespace via `spec.clusterName`.

## Where to start

- **Install:** [Kubernetes](getting-started/kubernetes.md) or [OpenShift](getting-started/openshift.md)
- **First cluster in five minutes:** [Quick start](getting-started/quickstart.md)
- **Copy-paste manifests:** [Examples](examples.md)
- **Platform differences:** [Vanilla Kubernetes](platforms/kubernetes.md) · [OpenShift](platforms/openshift.md)

## Container images

| Image | Reference |
|-------|-----------|
| Operator | `ghcr.io/marthydavid/openriak-operator:<version>` |
| Riak KV 3.2 (default) | `ghcr.io/marthydavid/riak:3.2.6` — amd64 + arm64 |
| Riak KV 3.0 | `ghcr.io/marthydavid/riak:3.0.16` — amd64 + arm64 |
| Riak KV 3.4 | `ghcr.io/marthydavid/riak:3.4.0` — amd64 only |
| Helm chart | `oci://ghcr.io/marthydavid/charts/openriak-operator` |

!!! note "Authentication model"
    Users authenticate with **client certificates only**. There are no passwords: `RiakUser`
    requires `spec.certificateRef`, and the certificate comes either from
    [cert-manager](https://cert-manager.io) or from an external CA you bring. See
    [mTLS authentication](mtls.md).
