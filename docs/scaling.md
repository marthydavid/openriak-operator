# Scaling the OpenRiak operator

Guidance for running the operator at fleet scale — dozens of RiakClusters with
hundreds of RiakUsers and RiakBuckets — plus a load-test harness to measure it.

## How the operator behaves at scale

Traced from the reconcilers (`internal/controller`):

### Steady state is cheap

- **RiakUser and RiakBucket** reconcilers reach `Ready` and return with **no
  requeue**, and use `predicate.GenerationChangedPredicate`. Once provisioned,
  hundreds of users/buckets do **zero ongoing work** unless their spec changes.
- **RiakCluster** self-requeues every 10s to refresh `status` from a **cached**
  Pod `List` — light, but it does not back off once Ready, so N clusters is a
  constant N/10 reconciles per second of low-cost work.

### The two things that bite

1. **Serial reconciles.** `MaxConcurrentReconciles` is the controller-runtime
   default (**1 worker per controller**), so RiakUsers/Buckets/Clusters are each
   reconciled one at a time. Hundreds of users apply serially, and on an
   operator restart the whole fleet re-syncs at once.

2. **Per-op `kubectl exec` cost.** All Riak-side work goes through `kubectl exec`
   (`internal/riak/executor.go`). Each RiakUser create runs
   `security enable` + `add-user` + `add-source` + one `security grant` per
   **distinct grant target** (grants on the same target are batched into one
   call — `Manager.GrantUserPermissions`). Each call is an apiserver `exec`
   round-trip plus a `riak-admin` invocation.

> **Measured, single node (local):** a `riak-admin security grant`/`add-user`
> costs **~0.15s** — it uses `nodetool` to attach to the *running* node, it does
> **not** cold-start a BEAM VM. (That cold-start cost applies to `riak ping` /
> `riak-admin status`, which is why the health *probes* are TCP, not those
> commands — a different code path from provisioning.) So grant batching cuts
> the *number* of serialized ops, not a per-op OOM risk: ~10 grants in 1.7s
> collapse to one ~0.17s call. The `kubectl exec` apiserver round-trip in a
> real cluster is likely the larger share and is **not** captured by a local
> measurement.

**Not yet measured:** end-to-end convergence at fleet scale (dozens of clusters,
hundreds of users). The dominant costs there are the two above plus **Riak
StatefulSet boot and ring-join time per cluster** (minutes each), which the
provisioning path does not affect. Run the harness below on a representative
multi-node cluster to get real numbers before sizing anything.

### Recommendations

- **Give Riak nodes headroom** (`spec.resources`) — a single node idles around
  ~120 MB; size for your data and connection load, not for the provisioning
  path (which is light per op).
- **Provision gradually** where possible so serial reconciles keep up.
- **Enable monitoring** (`spec.monitoring.enabled`) and watch the operator's
  `controller_runtime_reconcile_time_seconds` and `workqueue_depth` during
  rollouts — that is the real convergence signal.
- Consider a **namespace-per-tenant** layout so blast radius and RBAC stay
  bounded.

**Concurrency knob.** `--max-concurrent-reconciles` (Helm:
`maxConcurrentReconciles`, default **1**) sets `MaxConcurrentReconciles` for
every controller. Raising it lets the operator provision many users/buckets in
parallel instead of one at a time — the most direct lever on serial-reconcile
convergence. Each extra worker adds parallel `kubectl exec` load on the Riak
nodes, so raise it gradually and watch node CPU/memory and
`controller_runtime_reconcile_time_seconds` with the harness before settling on
a value.

## Load-test harness

`test/scale` creates a configurable fleet and measures how long the operator
takes to drive it all to `Ready`. It runs against **whatever your kubeconfig
points at** — the operator, CRDs, cert-manager and a usable operand image must
already be installed. It does not stand up a cluster; point it at a realistic
environment (a real multi-node cluster for a true 50-node test).

```bash
# Defaults: 3 clusters × (5 users + 5 buckets)
make scale-test

# Your target scale
go run ./test/scale -clusters 50 -users 4 -buckets 4 -timeout 45m

# Keep resources for inspection instead of tearing down
go run ./test/scale -clusters 10 -users 10 -buckets 10 -keep
```

Flags: `-clusters`, `-users` (per cluster), `-buckets` (per cluster),
`-namespace`, `-image`, `-storage-class`, `-timeout`, `-poll`, `-keep`.

It prints a live phase count and a summary: per-kind convergence time and
throughput (resources/second), total wall clock, and any resources stuck in
`Failed`. For reconcile-latency and queue-depth detail, scrape the operator's
Prometheus `/metrics` during the run
(`controller_runtime_reconcile_time_seconds`, `workqueue_depth`,
`controller_runtime_reconcile_total`).

> The harness is a dev/ops tool, not part of CI: a real fleet needs a real
> cluster with enough nodes to host the operand. Its client wiring, manifest
> generation and reporting are validated against a live cluster; the full-scale
> numbers are yours to gather in a representative environment.
