# OpenShift reference

Applies to OpenShift 4.x and OKD. The operator has been exercised on OKD 4.21 (three schedulable
control-plane nodes, `restricted-v2`, cert-manager, user-workload monitoring and a pull-through
image mirror) with clusters of up to 10 × 3 nodes using ephemeral storage. Durable clusters need a
StorageClass, which that test cluster did not have, so PVC-backed clusters on OpenShift are
documented from the platform's behaviour rather than from that run.

## Security Context Constraints

The Riak operand image runs under the default **`restricted-v2`** SCC with no extra grants.

`restricted-v2` ignores the image's `USER` and runs the container as an arbitrary UID from the
namespace's range (for example `1000700000`) whose only supplementary group is `0`. The image is
built for that: every directory Riak writes at runtime (`/etc/riak`, `/var/lib/riak`,
`/var/log/riak`, `/var/run/riak`) is owned by `riak:root` and group-writable (`chmod g=u`), and the
entrypoint never `mkdir`/`chown`s at runtime.

!!! warning "Use the published image, or keep the permissions"
    If you build your own operand image, preserve group-0 ownership and `g=u` on those paths, or
    pods crash-loop under `restricted-v2`. Verify locally with
    `docker run --user 1000700000:0 <image>` — it must reach "Riak is ready".
    The repository's `images/riak/Dockerfile.ocp` is a single-arch (amd64) variant for in-cluster
    `BuildConfig` builds, where buildah would otherwise fail on the arm64 stage of the multi-arch
    Dockerfile.

No `anyuid`, `privileged` or custom SCC is needed for the operator either.

## Installing the operator

```bash
helm install openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator \
  --namespace openriak-system --create-namespace
```

Or without Helm: `oc apply -k config/default --server-side`. See
[Install on OpenShift](../getting-started/openshift.md).

## cert-manager

Install *cert-manager Operator for Red Hat OpenShift* (OKD: community cert-manager). Create the CA
`Issuer` in the same project as your Riak resources, or use a `ClusterIssuer`.

## Storage

| Option | Use |
|--------|-----|
| LVM Storage (LVMS) | `storageClassName: lvms-vg1` (name depends on your `LVMCluster`) — node-local volumes; a pod is bound to the node holding its volume |
| Local Storage Operator | Pre-provisioned local PVs via a `LocalVolume` StorageClass |
| CSI / cloud classes | `gp3-csi`, `thin-csi`, … |
| None available | `ephemeralStorage: true` — **non-durable**, for tests only |

Node-local storage plus the default `Required` hostname anti-affinity means a 3-node cluster needs three
nodes that each have a volume available, and a cluster can never be larger than the number of schedulable
nodes. A node loss makes that Riak node unavailable until the node returns; Riak replicates across the
other nodes. On a test cluster you can set `spec.podAntiAffinity: Preferred` to run more Riak nodes than
Kubernetes nodes (see [Running more Riak nodes than Kubernetes nodes](../crds/riakcluster.md#running-more-riak-nodes-than-kubernetes-nodes));
with it, losing one Kubernetes node can take out several Riak nodes at once.

Before you pick a load for a test, check the **NIC speed** of the nodes (`/sys/class/net/<nic>/speed`): with
`n_val` 3 the network, not Riak, is usually the limit. See [Sizing a load for your network](../scaling.md#sizing-a-load-for-your-network).

## Image mirrors

Disconnected or mirrored clusters redirect pulls with `ImageDigestMirrorSet` /
`ImageTagMirrorSet`. A digest mirror only applies to pulls **by digest**; a tag pull bypasses it.
If `ghcr.io` is mirrored through an `ImageDigestMirrorSet`, pin the operand by digest:

```yaml
spec:
  image: ghcr.io/marthydavid/riak@sha256:<digest>
```

and the operator's default via `riak.image` in the chart values.

## Exposing Riak

Protobuf (8087) is raw TCP, so an OpenShift `Route` (HTTP/SNI) does not carry it. In-cluster clients
use the Service `<cluster>.<project>.svc:8087`. For external access use a `LoadBalancer` Service or
MetalLB, and keep mTLS terminating in Riak.

## Monitoring

- Operator: set `metrics.serviceMonitor.enabled=true` in the chart.
- Riak nodes: `spec.monitoring.enabled: true` creates a `ServiceMonitor`. For projects outside the
  platform stack, enable [user workload monitoring](https://docs.openshift.com/container-platform/latest/observability/monitoring/enabling-monitoring-for-user-defined-projects.html)
  so Prometheus scrapes it.

## Differences from vanilla Kubernetes at a glance

| Topic | OpenShift | Vanilla |
|-------|-----------|---------|
| Namespaces | Projects (`oc new-project`) | `kubectl create ns` |
| Pod UID | Arbitrary, group 0 | Image `USER riak` |
| SCC / PSA | `restricted-v2`, no grants needed | Pod Security Admission levels |
| cert-manager | Red Hat operator from OperatorHub | Manifest or Helm |
| Storage | LVMS / LSO / CSI | Cloud/CSI classes |
| Image mirroring | IDMS/ITMS (digest vs tag) | Registry mirrors in the runtime |
| External access | LoadBalancer/MetalLB (not Routes) | LoadBalancer/NodePort |

## Known limitation at high user counts

In July 2026 testing on OKD (10 clusters × 20 cert-auth users × 5 buckets), buckets converged
cleanly, but not all users did: nodes restarted repeatedly while `riak-admin security enable` was
being issued. Since then it is enabled once per cluster (`status.securityEnabled`), which fixed
single clusters but was not sufficient at that scale. Keep the number of users per cluster modest
and see [Scaling](../scaling.md) for current guidance.
