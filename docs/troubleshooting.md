# Troubleshooting

On OpenShift replace `kubectl` with `oc`.

## First look

```bash
kubectl get riakclusters,riakusers,riakbuckets
kubectl describe riakcluster <name>
kubectl -n <operator-namespace> logs deploy/<operator-deployment>
```

The `PHASE` and `READY` columns come from `status`; the `Ready` condition's message says why not.

## Cluster

| Symptom | Cause / fix |
|---------|-------------|
| Pods `Pending`, "didn't match pod anti-affinity" | Fewer schedulable nodes than `spec.size`. Reduce size or add nodes |
| Your own pod in the same namespace stays `Pending` with "didn't satisfy existing pods anti-affinity rules" | The Riak pods' required anti-affinity selects pods by the label `cluster=<riak cluster name>`. Do not label other pods `cluster=<that name>` (use another key, e.g. `riak-target`), or they cannot run on any node that hosts a node of that cluster |
| Pods `Pending`, unbound PVC | StorageClass missing or cannot provision. `kubectl get pvc`; set `storageClassName` or use `ephemeralStorage: true` for tests |
| `CrashLoopBackOff` on OpenShift, permission denied on `/etc/riak` | Custom operand image without group-0 writable dirs — see [OpenShift](platforms/openshift.md#security-context-constraints) |
| `ImagePullBackOff` | Registry access; on mirrored clusters pin by digest ([Image mirrors](platforms/openshift.md#image-mirrors)) |
| Certificates created but no TLS volume | `spec.tls.enabled` must be `true` |
| Phase flaps between Ready and Creating | Check pod restarts and exec errors in the operator log; see [Scaling](scaling.md) |
| Changing `ephemeralStorage` is rejected | Storage mode is immutable; recreate the cluster |

## RiakUser

| Symptom | Cause / fix |
|---------|-------------|
| Waits, never starts | Cluster not `Ready` yet — the user retries automatically |
| `Ready` but `Cert` column is `false` | cert-manager has not issued: `kubectl describe certificate <riakuser>-client-tls` |
| `certificate verify failed` | User and cluster issuers must chain to the same CA |
| Auth fails with a valid cert | CN must equal `spec.username`: inspect the issued cert with `openssl x509 -noout -subject` |

More in [mTLS with cert-manager](mtls.md#troubleshooting).

## RiakBucket

| Symptom | Cause / fix |
|---------|-------------|
| `Failed` with an `error` | `kubectl get riakbucket <name> -o jsonpath='{.status.error}'` |
| Property ignored | Typed fields (`nVal`, `allowMulti`, …) override the same key in `properties`; `status.properties` shows what was sent |

## Debug commands

```bash
kubectl exec <cluster>-0 -c riak -- riak-admin member-status
kubectl exec <cluster>-0 -c riak -- riak-admin ring-status
kubectl exec <cluster>-0 -c riak -- riak-admin security print-users
kubectl exec <cluster>-0 -c riak -- riak-admin bucket-type list
```
