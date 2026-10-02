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
| Your own pod in the same namespace stays `Pending` with "didn't satisfy existing pods anti-affinity rules" | Clusters created by operator versions before 0.0.11 select pods by `cluster=<riak cluster name>` alone, so any pod with that label is repelled from nodes hosting a Riak node. Upgrade the operator (the cluster rolls once and gets the narrower `app=riak,cluster=<name>` selector), or use another label key, e.g. `riak-target` |
| Pods `Pending`, unbound PVC | StorageClass missing or cannot provision. `kubectl get pvc`; set `storageClassName` or use `ephemeralStorage: true` for tests |
| `CrashLoopBackOff` on OpenShift, permission denied on `/etc/riak` | Custom operand image without group-0 writable dirs — see [OpenShift](platforms/openshift.md#security-context-constraints) |
| `ImagePullBackOff` | Registry access; on mirrored clusters pin by digest ([Image mirrors](platforms/openshift.md#image-mirrors)) |
| Certificates created but no TLS volume | `spec.tls.enabled` must be `true` |
| Phase flaps between Ready and Creating | Check pod restarts and exec errors in the operator log; see [Scaling](scaling.md) |
| Changing `ephemeralStorage` is rejected | Storage mode is immutable; recreate the cluster |
| A node crash-loops with `{function_clause,[{orddict,fetch,['riak@<pod>...',[]]}` from `riak_core_capability`, often next to `Hostname <pod> is illegal` | The ring on the node's volume belongs to another node name: the node once started under a short name (`riak@<pod>`) and later under its FQDN ([#59](https://github.com/marthydavid/openriak-operator/issues/59)). Older operand images could pick the short name on a pod's first start when they read `/etc/hosts` while the kubelet was rewriting it for the metrics sidecar. Fixed operand images retry that read and never start under a short name, and on start move a standalone ring of another name aside (keeping the data). A ring that has other members is refused with instructions instead; replace the node (`riak-admin cluster force-replace`) or delete its PVC and pod |
| Pod stays in `CrashLoopBackOff` with `cannot determine this pod's FQDN` | Neither `/etc/hosts` nor `/etc/resolv.conf` gave the pod's DNS name; the node refuses to start under a short name. The log prints both files |
| Operator logs `member-status on seed ...: Node riak@<name> is not responding to pings` | riak-admin cannot reach the node under that name: the node is down, still starting, or runs under another name (check the first line of the pod log, `Starting Riak node: ...`) |
| The previous failure's log was overwritten by later restarts | The operand keeps the logs of its last failed runs on the data volume: `kubectl exec <pod> -c riak -- ls /var/lib/riak/crash-logs` (each run has a `reason` file, the Riak logs and a gzipped crash dump) |

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

Riak nodes are named after the pod FQDN, which a bare `riak-admin` does not know: on its own it
addresses `riak@<pod>` and answers `Node riak@<pod> is not responding to pings` even for a healthy
node. Point it at the node's generated `vm.args`, as the operator does:

```bash
ra() { kubectl exec "$1" -c riak -- sh -c \
  'VMARGS_PATH=$(ls -1 /var/lib/riak/generated.conf/vm.*.args | tail -1) exec riak-admin "$@"' riak-admin "${@:2}"; }

ra <cluster>-0 member-status
ra <cluster>-0 ring-status
ra <cluster>-0 security print-users
ra <cluster>-0 bucket-type list
```
