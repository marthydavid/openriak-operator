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

1. **Provisioning storms / operator restart.** `MaxConcurrentReconciles` is the
   controller-runtime default (**1 worker per controller**), so resources are
   reconciled serially. Each RiakUser create runs
   `security enable` + `add-user` + `add-source` + one `security grant` per
   distinct grant target (grants on the same target are batched into a single
   call) — every call is a `kubectl exec` that **spawns a temporary Erlang VM
   (BEAM) on the Riak node**. Hundreds of users = hundreds of serialized
   exec→BEAM spawns. Frequent BEAM spawns are known to OOM small nodes (it is
   why the health probes are TCP, not `riak ping`). On an operator restart the
   whole fleet re-syncs at once.

2. **Per-node exec cost.** All Riak-side work goes through `kubectl exec`
   (`internal/riak/executor.go`). This is correct and injection-safe, but it is
   the dominant cost per operation. A user's grants are **batched by target** —
   one `security grant` call per distinct resource/bucket instead of one per
   grant (`Manager.GrantUserPermissions`) — so a user with several grants on the
   same target costs a single exec. Holding a longer-lived admin connection
   would cut the remaining per-user calls (enable/add-user/add-source) further,
   but that is a larger change, not yet done.

### Recommendations

- **Give Riak nodes headroom.** Size CPU/memory so a burst of `riak-admin` BEAM
  spawns during provisioning does not OOM them (`spec.resources`).
- **Provision gradually** where possible, rather than applying hundreds of
  RiakUsers at once, to smooth the exec storm.
- **Enable monitoring** (`spec.monitoring.enabled`) and watch node CPU/memory
  and the operator's `controller_runtime_reconcile_time_seconds` /
  `workqueue_depth` during rollouts.
- Consider a **namespace-per-tenant** layout so blast radius and RBAC stay
  bounded.

Tuning `MaxConcurrentReconciles` up would speed convergence but multiplies the
concurrent exec/BEAM load per node — measure before raising it. It is not yet a
CRD/flag knob.

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
