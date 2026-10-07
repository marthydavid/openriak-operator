# Vanilla Kubernetes reference

Applies to upstream Kubernetes 1.24+ and distributions such as kind, minikube, k3s, EKS, GKE and AKS.

## Requirements

| Need | Details |
|------|---------|
| Kubernetes | 1.24+ |
| StorageClass | Dynamic provisioning for durable clusters. kind: `standard`; EKS: `gp3`; GKE: `standard-rwo`; AKS: `managed-csi`. Otherwise use `ephemeralStorage: true` |
| cert-manager | Required for the cluster TLS certificate; also issues `RiakUser` certificates unless they come from an [external CA](../mtls.md#client-certificates-from-an-external-ca) |
| Nodes | At least `spec.size` schedulable nodes (required hostname anti-affinity) |
| Registry access | `ghcr.io/marthydavid/*`; set `imagePullSecrets` on the operator chart or mirror the images and use `--riak-image` |

## Operator permissions

The operator's ClusterRole covers its three CRDs (+ status/finalizers), StatefulSets, Services,
ConfigMaps, Pods (read), PVCs (read), cert-manager `Certificates`, Prometheus `ServiceMonitors`,
and **`pods/exec` create** — Riak administration (`riak-admin`) is run inside the Riak pods through
the Kubernetes exec API. If you restrict exec in your cluster (for example with admission policies),
allow the operator's ServiceAccount.

## Networking

- Clients inside the cluster connect to `<cluster>.<namespace>.svc:8087` (protobuf) or `:8098`/`:8443` (HTTP/HTTPS).
- Protobuf is raw TCP, not HTTP, so an Ingress cannot route it. Expose it outside the cluster with a
  `LoadBalancer`/`NodePort` Service of your own selecting the cluster's pods (label `cluster: <name>`),
  or through a TCP-capable gateway. Terminate mTLS in Riak: do not offload TLS in front of it, or the
  client certificate never reaches Riak.
- NetworkPolicies must allow node-to-node traffic within the cluster (the headless Service) and the
  operator reaching pods through the API server only (exec uses the API server, not pod IPs).

## Storage

- Each pod gets a PVC `data-<pod>`; choose `storageClassName` and `storageSize` (default `10Gi`).
- Use a class with `volumeBindingMode: WaitForFirstConsumer` on multi-zone clusters so volumes land
  where pods are scheduled.
- Local-path provisioners (kind, k3s) are fine for development only.

## CPU architecture

| Image | amd64 | arm64 |
|-------|-------|-------|
| Riak 3.2.6 (default) | ✔ | ✔ (Graviton2-compatible build) |
| Riak 3.0.16 | ✔ | ✔ |
| Riak 3.4.0 | ✔ | ✘ |

Apple Silicon: use the 3.2/3.0 arm64 images natively; 3.4 needs an amd64 environment.

## Pod security

The operand image runs as a non-root `riak` user. The operator does not set a custom pod
`securityContext`. If a namespace enforces the `restricted` Pod Security Standard, verify that Riak
pods are admitted; relax to `baseline` for the Riak namespace if not.

## Monitoring

Set `spec.monitoring.enabled: true`. With the Prometheus Operator installed a `ServiceMonitor` is
created; without it, scrape the exporter pods directly at
`http://<pod>:7979/probe?module=riak&target=http://127.0.0.1:8098/stats`.

## Local development with kind

```bash
kind create cluster
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
helm install openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator -n openriak-system --create-namespace
kubectl apply -f https://raw.githubusercontent.com/marthydavid/openriak-operator/main/examples/0-local-dev-cluster.yaml
```
