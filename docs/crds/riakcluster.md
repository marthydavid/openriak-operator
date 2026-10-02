# RiakCluster

A `RiakCluster` is a set of Riak nodes. The operator reconciles it into:

| Object | Name | Notes |
|--------|------|-------|
| StatefulSet | `<cluster>` | One pod per node (`<cluster>-0`, `-1`, …), container `riak` |
| Service | `<cluster>` | Client-facing: `protobuf` (8087, or `spec.servicePort`), `http` (8098), `https` (8443 with TLS), `metrics` (7979 with monitoring) |
| Headless Service | `<cluster>-headless` | Node-to-node traffic and stable pod DNS |
| ConfigMap | — | Exporter mapping, when monitoring is enabled |
| `Certificate` | `<cluster>-tls` | When TLS is enabled |
| `ServiceMonitor` | — | When monitoring is enabled and the Prometheus Operator CRDs exist |

Pods of one cluster have a **required** anti-affinity on `kubernetes.io/hostname`: each node of a
cluster runs on a different Kubernetes node, so `spec.size` cannot exceed the number of schedulable
nodes (further pods stay `Pending`).

## Minimal example

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: demo
spec:
  size: 3
```

## Spec reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `size` | int, 1–999 | **required** | Number of Riak nodes |
| `image` | string | operator `--riak-image` (`ghcr.io/marthydavid/riak:3.2.6`) | Riak image. Changing it rolls the StatefulSet |
| `imagePullPolicy` | string | Kubernetes default | `Always`, `IfNotPresent`, `Never` |
| `resources` | ResourceRequirements | operator default (1 CPU / 2Gi requests) | Per-node CPU/memory |
| `storageClassName` | string | cluster default class | StorageClass of the data PVCs |
| `storageSize` | quantity | `10Gi` | Size of each node's data volume |
| `ephemeralStorage` | bool | `false` | Use an `emptyDir` instead of a PVC. **Data is lost on pod restart** |
| `riakConfig` | map[string]string | — | Any `riak.conf` key; see [below](#riak-configuration) |
| `tls.enabled` | bool | `false` | Enable TLS (HTTPS + protobuf TLS) using cert-manager |
| `tls.certManager.issuerName` | string | — | cert-manager Issuer/ClusterIssuer name |
| `tls.certManager.issuerKind` | string | `Issuer` | `Issuer` or `ClusterIssuer` |
| `monitoring.enabled` | bool | `false` | Add a Prometheus exporter sidecar (and a scrape object, see `scrapeKind`) |
| `monitoring.exporterImage` | string | operator default | Override the json_exporter image |
| `monitoring.scrapeKind` | enum | `PodMonitor` | Prometheus Operator object the operator creates: `PodMonitor`, `ServiceMonitor` or `None` (bring your own) |
| `monitoring.metricsConfig.configMapKeyRef` | `{name, key}` | built-in mapping | Your own json_exporter rules, from a ConfigMap in the cluster's namespace |
| `servicePort` | int, 1024–65535 | `8087` | Port of the protobuf service |
| `nodeSelector` | map | — | Restrict nodes the pods can schedule on |

## Storage

- **Durable (default):** each pod gets a PVC `data-<pod>` from `storageClassName`/`storageSize`,
  mounted at `/var/lib/riak`.
- **Ephemeral:** `ephemeralStorage: true` uses an `emptyDir`; `storageClassName` and `storageSize`
  are ignored. Meant for tests, CI and clusters without a provisioner.
- **The mode is immutable.** Flipping an existing cluster between durable and ephemeral is rejected
  to protect your data — recreate the cluster instead.

## Riak configuration

`riakConfig` entries become `riak.conf` settings on every node. Changing them rolls the StatefulSet.

```yaml
spec:
  riakConfig:
    ring_size: "128"
    transfer_limit: "2"
    anti_entropy: "on"
```

!!! warning "Backends do not migrate data"
    Changing `storage_backend` (or a bucket's `backend`) on a cluster that holds data does not
    move that data. Plan a backup before switching. Multi-backend and memory/TTL recipes are in
    [Operator configuration](../operator-configuration.md#riak-configuration-specriakconfig).

`ring_size` must be a power of two and is fixed when the cluster is first formed.

!!! tip "Use a ring size of at least 128"
    A ring is divided into whole partitions, so a small ring cannot be spread evenly over a few
    nodes: 8 partitions over 3 nodes is 4/2/2 (50/25/25 %). With 128 partitions, 3 nodes own
    43/42/43. The Riak image defaults to 128 when `ring_size` is not set.

## Multi-node clusters

For `size` greater than 1 the operator forms the ring itself:

1. Nodes are named by their pod FQDN (`riak@<pod>.<cluster>-headless.<ns>.svc.<domain>`), so
   they can reach each other.
2. Once every pod is Ready, the operator joins each standalone node to `<cluster>-0`, then plans
   and commits once.
3. The cluster only reports `Ready` when **every** node is a valid ring member. Until then
   `status.phase` stays `Creating` with the condition reason `FormingCluster`.

!!! note "Operand image"
    Multi-node formation needs the Riak image from operator `0.0.8` or later
    (`ghcr.io/marthydavid/riak:3.2.6` was rebuilt for it). Older images name nodes by the bare pod
    name, which Erlang refuses for remote nodes.

To inspect the ring, run `riak-admin` against the generated `vm.args`:

```bash
kubectl exec my-cluster-0 -c riak -- sh -c \
  'VMARGS_PATH=$(ls -1 /var/lib/riak/generated.conf/vm.*.args | tail -1) riak-admin member-status'
```

## TLS

```yaml
spec:
  tls:
    enabled: true
    certManager:
      issuerName: riak-ca-issuer
      issuerKind: Issuer
```

`enabled: true` is required; setting only `certManager` has no effect. The operator requests the
certificate `<cluster>-tls`, mounts the Secret at `/etc/riak/certs` and configures Riak's HTTPS
listener. Details and rotation: [mTLS with cert-manager](../mtls.md).

## Monitoring

```yaml
spec:
  monitoring:
    enabled: true
```

Adds a `json_exporter` sidecar translating Riak's `/stats` into Prometheus metrics on port 7979.
See [Operator configuration](../operator-configuration.md#prometheus-metrics-specmonitoring).

## Status

```bash
kubectl get riakcluster
# NAME   PHASE   READY   NODES   TOTAL   AGE
```

| Field | Meaning |
|-------|---------|
| `phase` | `Creating`, `Ready`, `Updating`, `Failed` |
| `readyNodes` / `totalNodes` | Ready pods out of `spec.size` |
| `conditions` | `Ready` condition with reason and message |
| `members[]`, `nodeConditions[]` | Per node: `ready`, `health` (`Healthy`/`Unhealthy`/`Unknown`), `phase`, `storageReady`, storage class/size |
| `securityEnabled` | Riak security has been enabled (done once, when the first user is created) |
| `storageClassName`, `storageSize`, `ephemeralStorage` | The storage actually in use |
| `tlsStatus` | `enabled`, `certManagerReady`, `certManagerError`, `interNodeReady`, `clientReady` |
| `monitoringStatus` | `enabled`, `exporterReady`, `scrapeKind`, `scrapeObjectReady`, `exporterError` (`serviceMonitorReady` is deprecated) |
| `buckets[]`, `users[]` | The RiakBuckets / RiakUsers targeting this cluster and whether they are ready |

Status is recomputed from live pods, PVCs and certificates about every 10 seconds.

## Scaling and updates

- Change `spec.size` to add or remove nodes.
- Changing `image`, `riakConfig`, `resources` or `tls` triggers a rolling update of the StatefulSet.
- At fleet scale see [Scaling](../scaling.md).

## Deleting

Delete the `RiakUser` and `RiakBucket` objects of a cluster first, then the cluster. PVCs created
from the StatefulSet's volume claim template follow your StorageClass reclaim policy and Kubernetes'
StatefulSet PVC retention rules.
