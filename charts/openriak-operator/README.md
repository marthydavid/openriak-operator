# openriak-operator Helm chart

Installs the [OpenRiak operator](https://github.com/marthydavid/openriak-operator), which manages
`RiakCluster`, `RiakBucket` and `RiakUser` resources: Riak KV clusters on Kubernetes with
TLS (cert-manager) and mTLS client-certificate users (cert-manager or external CA).

| | |
|---|---|
| Chart version | see `Chart.yaml` `version` |
| Operator image | `ghcr.io/marthydavid/openriak-operator`, tag defaults to `appVersion` |
| Default Riak image | `ghcr.io/marthydavid/riak:3.2.6` (3.0, 3.2 and 3.4 are published) |

## Prerequisites

- Kubernetes 1.34 or later. Built and verified against the 1.35 client libraries; runs on OpenShift/OKD
  4.21 and 4.22 (default `restricted-v2` SCC, no custom SCC needed) and on AKS.
- [cert-manager](https://cert-manager.io/docs/installation/) for the node certificate of TLS-enabled
  clusters, and for `RiakUser` client certificates unless you supply them from an external CA.
- A `StorageClass` for durable clusters (or use ephemeral storage for tests).
- Optional: Prometheus Operator CRDs for the `ServiceMonitor` / `PodMonitor` resources, and Grafana with its
  dashboard sidecar for the bundled dashboard.

## Install

From the OCI registry (published by the chart release workflow):

```bash
helm install openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator \
  --namespace openriak-system --create-namespace
```

From a checkout:

```bash
helm install openriak-operator charts/openriak-operator \
  --namespace openriak-system --create-namespace
```

Verify:

```bash
kubectl -n openriak-system get pods -l control-plane=controller-manager
kubectl get crd | grep riak.openriak.io
```

Then create a cluster, for example
`kubectl apply -f https://raw.githubusercontent.com/marthydavid/openriak-operator/main/examples/0-local-dev-cluster.yaml`.

## Upgrade

```bash
helm upgrade openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator \
  --namespace openriak-system
```

The chart renders the CRDs from its templates, so `helm upgrade` updates them together with the
operator (`crds.install`, default `true`). If you manage the CRDs yourself (GitOps, a cluster admin), set
`crds.install=false` and apply `config/crd/bases` from the matching operator tag before each upgrade, with
`kubectl apply --server-side` because the schemas are large.

### Upgrading from an earlier chart

Earlier charts shipped the CRDs in a `crds/` directory, which Helm applies once and then does not own.
The first upgrade to a chart that renders them has to adopt the existing CRDs, otherwise Helm refuses
with `exists and cannot be imported into the current release`. Pick one:

```bash
# Helm 3.17 or later
helm upgrade openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator \
  --namespace openriak-system --take-ownership
```

or label and annotate the CRDs for the release first (any Helm 3), then run the normal upgrade:

```bash
for crd in riakclusters riakusers riakbuckets; do
  kubectl label crd $crd.riak.openriak.io app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate crd $crd.riak.openriak.io \
    meta.helm.sh/release-name=openriak-operator \
    meta.helm.sh/release-namespace=openriak-system --overwrite
done
```

Adoption does not touch the stored RiakClusters, RiakUsers or RiakBuckets. Use your own release name and
namespace if they differ.

Check the [release notes](https://github.com/marthydavid/openriak-operator/blob/main/docs/release-notes.md)
before upgrading: some releases restart existing RiakClusters once (one pod at a time).

## Uninstall

```bash
helm uninstall openriak-operator --namespace openriak-system
```

This leaves the CRDs and every RiakCluster, RiakBucket and RiakUser (and their data volumes) in place,
because the CRDs carry `helm.sh/resource-policy: keep` (`crds.keep`, default `true`). With `crds.keep=false`,
uninstalling **deletes the CRDs and with them every Riak resource**.
Delete the Riak resources first if you want them gone, then remove the CRDs by hand
(`kubectl delete crd riakclusters.riak.openriak.io riakusers.riak.openriak.io riakbuckets.riak.openriak.io`).

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `ghcr.io/marthydavid/openriak-operator` | Operator image |
| `image.tag` | `""` (chart `appVersion`) | Operator image tag; pin a digest-resolved tag if you need to |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Pull secrets for the operator image |
| `nameOverride`, `fullnameOverride` | `""` | Override resource names |
| `riak.image` | `ghcr.io/marthydavid/riak:3.2.6` | Default operand image (`--riak-image`), used when a RiakCluster omits `spec.image` |
| `maxConcurrentReconciles` | `1` | Parallel reconciles per controller (`--max-concurrent-reconciles`). Raise it to provision many clusters, users or buckets faster, at the cost of more parallel `kubectl exec` load on Riak nodes; measure first ([scaling guide](https://github.com/marthydavid/openriak-operator/blob/main/docs/scaling.md)) |
| `replicaCount` | `1` | Manager replicas; with more than one, leader election must stay enabled |
| `leaderElection.enabled` | `true` | Enable leader election |
| `metrics.enabled` | `true` | Serve authenticated metrics on `:8443`, with a Service and token-review RBAC |
| `metrics.serviceMonitor.enabled` | `false` | Create a `ServiceMonitor` for the operator (needs Prometheus Operator CRDs) |
| `metrics.serviceMonitor.tlsConfig` | `insecureSkipVerify: true` | Scrape TLS settings. The manager serves a self-signed certificate, so verification is skipped by default; set `insecureSkipVerify: false` explicitly and add `caFile`/`serverName` if you issue a real certificate |
| `crds.install` | `true` | Render the three CRDs from the chart so `helm upgrade` updates them; `false` when you manage them yourself |
| `crds.keep` | `true` | Add `helm.sh/resource-policy: keep` to the CRDs so `helm uninstall` does not delete them (and every Riak resource) |
| `dashboard.enabled` | `false` | Ship the Riak KV Grafana dashboard as a ConfigMap for Grafana's dashboard sidecar |
| `dashboard.namespace` | release namespace | Namespace of the dashboard ConfigMap; set to Grafana's namespace if the sidecar watches only one |
| `dashboard.labels` | `grafana_dashboard: "1"` | Labels the sidecar selects on |
| `dashboard.annotations` | `{}` | Extra annotations, e.g. `grafana_folder: OpenRiak` |
| `serviceAccount.create` | `true` | Create the ServiceAccount |
| `serviceAccount.name` | release fullname | ServiceAccount name |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations |
| `rbac.create` | `true` | Create the ClusterRole/Role and bindings |
| `podAnnotations` | `{}` | Extra pod annotations |
| `resources` | limits `500m` / `512Mi`, requests `10m` / `128Mi` | Manager resources. The operator caches what it watches and peaks after a restart; keep the memory limit comfortably above steady state |
| `nodeSelector`, `tolerations`, `affinity` | empty | Standard scheduling passthroughs |

The pod runs as non-root with a read-only root filesystem, all capabilities dropped and the
`RuntimeDefault` seccomp profile.

## Examples

Use a different default Riak image and scrape the operator with Prometheus:

```bash
helm install openriak-operator charts/openriak-operator \
  --namespace openriak-system --create-namespace \
  --set riak.image=ghcr.io/marthydavid/riak:3.4.0 \
  --set metrics.serviceMonitor.enabled=true
```

Ship the Grafana dashboard into Grafana's namespace:

```bash
helm upgrade --install openriak-operator charts/openriak-operator \
  --namespace openriak-system \
  --set dashboard.enabled=true \
  --set dashboard.namespace=monitoring \
  --set-string dashboard.annotations.grafana_folder=OpenRiak
```

Provision many resources faster:

```bash
helm upgrade --install openriak-operator charts/openriak-operator \
  --namespace openriak-system --set maxConcurrentReconciles=4
```

## More

- [Getting started on Kubernetes](https://github.com/marthydavid/openriak-operator/blob/main/docs/getting-started/kubernetes.md)
  and [on OpenShift](https://github.com/marthydavid/openriak-operator/blob/main/docs/getting-started/openshift.md)
- [Documentation](https://github.com/marthydavid/openriak-operator/tree/main/docs) and
  [release notes](https://github.com/marthydavid/openriak-operator/blob/main/docs/release-notes.md)
