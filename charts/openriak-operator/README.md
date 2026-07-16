# openriak-operator Helm chart

Installs the [OpenRiak operator](https://github.com/marthydavid/openriak-operator), which
manages `RiakCluster`, `RiakBucket` and `RiakUser` resources. RiakUsers authenticate with
mTLS client certificates issued by [cert-manager](https://cert-manager.io), which must be
installed for TLS-enabled clusters and users.

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

CRDs ship in the chart's `crds/` directory: Helm installs them on first install but never
upgrades or deletes them. Apply `config/crd/bases/` manually when upgrading across CRD
changes, and note `helm uninstall` leaves the CRDs (and all Riak custom resources) in place.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `ghcr.io/marthydavid/openriak-operator` | Operator image |
| `image.tag` | chart `appVersion` | Operator image tag |
| `image.pullPolicy` | `IfNotPresent` | Pull policy |
| `riak.image` | `ghcr.io/marthydavid/riak:3.2.6` | Default operand image (`--riak-image`) used when a RiakCluster omits `spec.image` |
| `replicaCount` | `1` | Manager replicas (leader election handles >1) |
| `leaderElection.enabled` | `true` | Enable leader election |
| `metrics.enabled` | `true` | Serve authenticated metrics on `:8443` with Service + token-review RBAC |
| `metrics.serviceMonitor.enabled` | `false` | Create a ServiceMonitor (requires Prometheus Operator CRDs) |
| `serviceAccount.create` | `true` | Create the ServiceAccount |
| `serviceAccount.name` | release fullname | ServiceAccount name |
| `rbac.create` | `true` | Create ClusterRole/Role and bindings |
| `resources` | see `values.yaml` | Manager resources |
| `imagePullSecrets`, `podAnnotations`, `nodeSelector`, `tolerations`, `affinity` | — | Standard passthroughs |

## Example

```bash
helm install openriak-operator charts/openriak-operator \
  --namespace openriak-system --create-namespace \
  --set riak.image=ghcr.io/marthydavid/riak:3.2.6 \
  --set metrics.serviceMonitor.enabled=true
```
