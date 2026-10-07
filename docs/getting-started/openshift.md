# Install on OpenShift / OKD

The operator and the Riak operand image both run under the default `restricted-v2` SCC — no custom
SCC, `anyuid` grant or privileged pod is needed. See the [OpenShift reference](../platforms/openshift.md)
for how and why.

## Prerequisites

- OpenShift 4.x / OKD with `oc` (or `kubectl`) and Helm 3.8+
- **cert-manager**: install *cert-manager Operator for Red Hat OpenShift* from OperatorHub (OKD: the community cert-manager)
- A `StorageClass` for durable clusters — for example from the **LVM Storage** operator (`lvms-vg1`) or your CSI driver. Without one, use [ephemeral storage](../crds/riakcluster.md#storage) for test clusters
- Cluster nodes must be able to pull `ghcr.io/marthydavid/*`, directly or through a mirror ([details](../platforms/openshift.md#image-mirrors))

## 1. Install the operator

```bash
helm install openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator \
  --namespace openriak-system --create-namespace
oc -n openriak-system get pods
```

No `helm`? Use the kustomize manifests from a checkout instead:

```bash
oc apply -k config/default --server-side
```

## 2. Create a project and an issuer

```bash
oc new-project riak
```

Create a CA-backed `Issuer` in that project (see [mTLS](../mtls.md#example-a-namespace-local-ca-for-cert-manager)).

## 3. Create a cluster

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: riak
  namespace: riak
spec:
  size: 3
  storageClassName: lvms-vg1      # your StorageClass
  storageSize: 20Gi
  tls:
    enabled: true
    certManager:
      issuerName: riak-ca-issuer
```

```bash
oc -n riak get riakcluster -w
```

!!! note "Pod anti-affinity"
    Riak pods of one cluster are required to land on different nodes. A 3-node cluster needs at
    least 3 schedulable nodes (compact 3-master clusters qualify); otherwise pods stay `Pending`.

## Next

[Quick start](quickstart.md) · [OpenShift reference](../platforms/openshift.md)
