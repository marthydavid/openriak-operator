# Scaling the OpenRiak operator

Guidance for running the operator at fleet scale — dozens of RiakClusters with
hundreds of RiakUsers and RiakBuckets — plus a load-test harness to measure it.
Measured results from a real cluster are in [Scale test results](scale-test-results.md).

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
- **Give the operator memory.** It caches the objects it watches, and a resync
  after a restart briefly peaks well above steady state. In a 3-cluster / 9-node /
  60-user / 60-bucket run the operator peaked around **180 MiB** just after a
  restart, which is more than the old 128 MiB limit: it was `OOMKilled` in a loop
  and the fleet stalled. The defaults are now a 512 MiB limit and a 128 MiB
  request (manifest and Helm chart); keep the limit comfortably above what
  `kubectl top pod` shows after a restart, and raise it for larger fleets.
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
a value. A value below `1` falls back to the controller-runtime default (1).

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
`-replicas` (Riak nodes per cluster, `spec.size`), `-namespace`, `-image`,
`-storage-class`, `-ephemeral`, `-timeout`, `-poll`, `-keep`.

### Verifying Riak against the CRs

`Ready` only says the operator *thinks* it is done. With `-verify` (on by
default) the harness then reads Riak itself, on **every node** of every
cluster, and diffs it against the CRs: ring membership (`Valid:<size>`, nothing
joining/leaving/down), every RiakBucket's type is active and carries the
spec's `n_val` / `allow_mult` / `properties`, every RiakUser exists with a
`certificate` source, no Riak user exists without a RiakUser, and each user's
grants are **exactly** the spec (missing and extra permissions both count). It
maps CRD permissions to Riak tokens with its own table rather than the
operator's, so an operator mapping bug cannot hide itself. Metadata gossips, so
it retries for `-verify-timeout` (default 10m) before printing `MISMATCH:` lines;
success prints `MATCH: Riak holds exactly what the CRs declare`.

| Flag | Effect |
|------|--------|
| `-verify` | verify after convergence (default `true`) |
| `-verify-only` | verify an existing namespace; create nothing |
| `-mutate` | then re-randomise or drop the grants of every 3rd user and change `n_val`/`allow_mult` of every 3rd bucket, wait until the operator observed the new generations, verify again |
| `-delete-users-every N` | then delete every Nth RiakUser and verify the Riak users are gone too |
| `-verify-workers` | parallel `kubectl exec` calls while verifying (default 6) |
| `-verify-timeout` | how long verification retries before reporting mismatches |
| `-monitoring` | enable `spec.monitoring` on every cluster and verify the Riak metrics (see below) |
| `-external-users` | of `-users` per cluster, how many use a certificate from an external CA instead of cert-manager (see below) |
| `-scrape-kind` | with `-monitoring`: `PodMonitor`, `ServiceMonitor` or `None` (default: the operator's) |
| `-stress` | also stress-test Riak with the example application over mTLS (see [Stress-testing Riak](#stress-testing-riak)) |
| `-stress-duration` | timed load per client (default `1m`) |
| `-stress-threads` / `-stress-clients` | connections per client (16) / client pods per cluster (2) |
| `-stress-value-size` / `-stress-read-ratio` | bytes per object (1024) / fraction of reads (0.7) |
| `-stress-max-errors` | client errors tolerated per cluster (0) |
| `-stress-image` | image the clients run in; needs `python3` (default `registry.access.redhat.com/ubi9/python-311`) |
| `-operator-namespace` | where the operator runs, for the restart check (default: found by label) |

```bash
go run ./test/scale -clusters 3 -users 20 -buckets 20 -replicas 3 \
  -storage-class lvms-vg1 -mutate -delete-users-every 4 -timeout 30m
```

### Both certificate patterns

`-external-users N` makes the last N of each cluster's `-users` authenticate with a certificate
from an **external CA** (`certificateRef.externalSecretName`) while the others keep using
cert-manager (`issuerRef`), so both patterns share every cluster. The harness plays the external
PKI: it creates its own CA, publishes only the CA certificate in the Secret `scale-ext-ca`, which
every cluster trusts through `spec.tls.additionalClientCAs`, and issues each external user's
certificate itself. Cluster TLS is switched on (server certificates come from the scale CA Issuer).

After convergence it verifies, on every cluster and **every node**:

- every RiakUser reports `certificateReady`, no cluster reports a `trustBundleError`, and the
  cluster trusts the expected number of CAs (cluster CA, plus the external CA when used);
- a cert-manager user has a `Certificate`, an external-CA user has none;
- the node's real `ca.crt` (read from the pod, the file Riak uses as `ssl.cacertfile`) holds those
  CAs, and **each user's certificate, of both kinds, verifies against it** with CN equal to the
  username and client-auth usage.

It retries for `-verify-timeout` (cert-manager issues asynchronously and the kubelet refreshes the
mounted bundle on its own schedule) and prints `CERTS OK` or `CERTS:` lines. The same check runs
with `-stress` (cert-manager users only) and with `-verify-only`; pass the same `-external-users`
to `-verify-only` so it knows what to expect.

```bash
make scale-test CLUSTERS=3 USERS=6 EXTERNAL_USERS=3
go run ./test/scale -clusters 3 -users 10 -external-users 4 -replicas 3 -storage-class lvms-vg1
```

### Stress-testing Riak

The checks above prove the operator provisions Riak correctly. To test **Riak itself under load**, add
`-stress`. It runs an [example application](https://github.com/marthydavid/openriak-operator/tree/main/examples/stressapp)
(`riak_stress.py`, a stdlib-only Python client) against every cluster as Kubernetes Jobs and checks what
happened:

```bash
go run ./test/scale -clusters 3 -users 5 -buckets 5 -replicas 3 -monitoring \
    -stress -stress-duration 60s -stress-threads 16 -stress-clients 2
```

What `-stress` does:

1. Enables TLS on the clusters with a CA-backed cert-manager `Issuer`, and adds a stress bucket and a
   stress user (all KV permissions on the stress bucket type) per cluster. The mutate and delete stages
   leave them alone.
2. After the normal verification, ships the script in a ConfigMap and runs `-stress-clients` Jobs per
   cluster. Each client opens `-stress-threads` mTLS connections with the stress user's client certificate and
   runs a timed mix of writes and reads (`-stress-read-ratio`, `-stress-value-size`) for
   `-stress-duration`, then reads back every key it wrote. No image is built: any image with `python3` works
   (`-stress-image`).
3. Checks the clients' results: **no errors** (`-stress-max-errors` to tolerate some), **no lost values, no
   corrupt values** (every value is derived from its key and version, so each read can be verified), and
   that writes happened.
4. With `-monitoring`, cross-checks Riak's own metrics against what the clients did: `riak_node_puts_total`
   must have risen by exactly the number of completed writes, `riak_node_gets_total` by the number of
   completed reads (including the final verification pass), and `riak_vnode_puts_total` by at least a write
   quorum (2x) of the writes: the clients and the cluster are independent observers of the same traffic.
5. Verifies the cluster against the CRs again, and that **no Riak pod and not the operator restarted**.

It prints a table per cluster (ops/s, put and get latency p50/p95/p99, errors, lost, corrupt) and ends with
`STRESS OK` or the problems. `-verify-only -stress` runs the stress phase against an existing namespace.

!!! note "Not a benchmark"
    The client is a Python script. It is a load generator for checking correctness and behavior under
    concurrent load, not a measurement of Riak's maximum throughput; add `-stress-clients` to push harder.

### Soak test: a constant load for hours

`-soak` is a different kind of run: instead of "how fast", it asks "does one cluster hold a
**constant** load for hours, and what happens to it". It builds one TLS cluster, `-soak-buckets`
buckets (each with its own bucket type, `n_val` 3, `pr`/`pw` 2) and `-soak-users` certificate users,
and starts one client per user. Every client rotates over all buckets at its share of `-soak-rate`,
sending `pr`/`pw` with every request. A real 4-hour run, with charts, is on the
[Soak test results](soak-test-results.md) page.

```bash
# the defaults: 3 nodes, 300Gi PVC each, 10 buckets, 10 users, 200 ops/s of 16 KiB objects, 4 hours
go run ./test/scale -soak -storage-class lvms-vg1 -operator-namespace openriak-operator -verify-timeout 15m

# a heavier, bigger-object run that keeps everything it measures
go run ./test/scale -soak -soak-rate 600 -soak-value-size 131072 -soak-keyspace 300 -soak-threads 16 \
  -soak-cpu 4 -soak-memory 16Gi -soak-max-memory 64Gi -soak-artifacts ./soak-run \
  -storage-class lvms-vg1 -operator-namespace openriak-operator -timeout 30m -verify-timeout 20m

make soak-test STORAGE_CLASS=lvms-vg1 DURATION=4h
```

A 4-hour run outlives most terminals and tool timeouts: start it detached (`nohup setsid ... > soak.log &`
on a host close to the cluster) and follow the log.

| Flag | Default | Meaning |
|------|---------|---------|
| `-soak-duration` | `4h` | how long the clients hold the load (then they read every key back) |
| `-soak-rate` | `200` | operations per second, all clients together |
| `-soak-users`, `-soak-buckets` | `10`, `10` | users (one client each) and buckets (one bucket type each) |
| `-soak-threads` | `4` | connections per client |
| `-soak-value-size` | `16384` | bytes per object |
| `-soak-read-ratio` | `0.5` | share of operations that are reads |
| `-soak-keyspace` | `2000` | keys each client thread keeps per bucket; once full, writes overwrite. Live data is users x threads x buckets x keyspace x value-size, before replication |
| `-soak-nval`, `-soak-pr`, `-soak-pw` | `3`, `2`, `2` | bucket `n_val`; quorums sent with every request and set as bucket defaults |
| `-soak-storage` | `300Gi` | data volume of each node |
| `-soak-memory`, `-soak-max-memory` | `4Gi`, `16Gi` | each node's initial memory request and limit, and the most the scaler may raise it to |
| `-soak-cpu` | `2` | CPU **request** of each Riak node; there is no CPU limit, so a node can use more |
| `-soak-client-cpu` | `250m` | CPU request of each load client (no limit) |
| `-soak-replicas`, `-soak-max-replicas` | `3`, `5` | initial and maximum number of nodes |
| `-soak-pod-anti-affinity` | operator default (`Required`) | `spec.podAntiAffinity` of the cluster; `Preferred`/`None` let it grow past the number of Kubernetes nodes |
| `-soak-check`, `-soak-window`, `-soak-cooldown` | `30s`, `1m`, `20m` | sample interval, the clients' own reporting interval, minimum time between two scaling actions |
| `-soak-mem-pressure`, `-soak-p99`, `-soak-min-rate` | `0.85`, `1000`, `0.9` | thresholds the scaler (and the verdict) use |
| `-soak-max-error-rate` | `0.005` | tolerated share of failed operations |
| `-soak-max-disk` | `90` | percent: stop the clients and fail when any data volume is fuller than this |
| `-soak-action-timeout` | `20m` | a scaling action that leaves the cluster not ready this long is **stalled** (a failure) |
| `-soak-no-scale` | off | only observe, never change the cluster |
| `-soak-artifacts` | none | directory to save the run's time series, logs and summary to (see below) |

Every `-soak-check` the harness samples the pods (restarts, `OOMKilled`), `kubectl top` (memory against
the limit), the data volumes (`df`, every minute), and the clients' own `WINDOW` lines (achieved ops/s,
failed share, worst p99). A policy acts on that, with a cooldown between actions:

| Observation | Action |
|-------------|--------|
| a Riak container was `OOMKilled`, or its working set is above `-soak-mem-pressure` of the limit for 3 samples | raise every node's memory request and limit by 50% (rounded to 256Mi), up to `-soak-max-memory` |
| memory is already at its cap, or p99 stays above `-soak-p99` / throughput below `-soak-min-rate` for 5 samples | add a node, up to `-soak-max-replicas` **and, with the default `Required` anti-affinity, the number of schedulable Kubernetes nodes** |

It never scales in, never acts while a node is down or restarting, and reports a stalled action as a
failure. With the default `Required` anti-affinity a Riak node needs a Kubernetes node of its own, so on
a 3-node Kubernetes cluster the scaler can raise memory but can never add a 4th Riak node; it knows that
and says so at start-up. With `-soak-pod-anti-affinity Preferred` it may add nodes, but **extra Riak nodes
on the same machines add no network or disk bandwidth**, and the rebalancing that follows costs
throughput: in a smoke run at the network's limit, adding a 4th node lowered throughput while it joined.

**Safety.** Big objects can fill a volume within minutes, so the harness reads `df` every minute and,
when any data volume passes `-soak-max-disk`, deletes the clients and fails the run with the reason
rather than let a volume fill.

**Integrity.** Each client remembers what it wrote, and every read is judged against it:

| Result | Meaning | Fails the run |
|--------|---------|---------------|
| ok | the expected version was returned | no |
| `landed` | a write that timed out or failed *did* reach Riak, and a later read returned exactly that version | no: the client did not know the outcome |
| `lost` | the key was missing | yes |
| `stale` | an older version than was acknowledged: a lost update | yes |
| `ahead` | a newer version nobody can account for | yes |
| `corrupt` | bytes that are not any version the client could have written | yes |

The report gives throughput against the target, latency percentiles, the clients' error breakdown,
integrity (including a final read of every key), OOM kills and restarts, peak memory and disk, warning
events and the timeline of actions. The run **fails** on lost, stale, unexplained or corrupt data, a
failed-operation share above `-soak-max-error-rate`, throughput below `-soak-min-rate` of the target, a
stalled action, errors while the cluster was healthy (restart windows excluded), a data volume past
`-soak-max-disk`, or a cluster that does not end Ready; it then verifies Riak against the CRs as the other
modes do.

**What it saves.** With `-soak-artifacts DIR` the run leaves, after the cluster is gone:

| File | Content |
|------|---------|
| `samples.jsonl` | one row per sample: cluster state, per-node memory and disk, and **each client's own load** (rate, errors, latency percentiles) |
| `metrics.jsonl` | every `riak_*` series of every node, once a minute |
| `nodes.jsonl` | `kubectl top nodes`, once a minute |
| `logs/` | Riak, exporter and operator logs, Riak's own `error.log`/`crash.log`, the previous container's log after a restart, every client's log, events, pod descriptions, the custom resources |
| `summary.json` | the verdict, settings, final numbers and the timeline |

`hack/soak-report.py DIR OUT` turns those into the charts on the results page (SVG, light and dark,
standard library only), and `make test-soak-report` checks the generator.

#### Sizing a load for your network

The limit that decides what a cluster can carry is often the network, not Riak. With `n_val` 3 and quorum
reads and writes every operation moves several copies of the object between nodes: a write is sent to two
more nodes, and a read pulls copies from the other replicas. Measured on the test cluster, about **1.04 x
the object size** crosses each node's NIC, in each direction, per operation:

> per-node network use = 1.04 x ops/s x object size, and it has to stay below what the link really carries

On the test cluster's **1 GbE** links that is about 90 MB/s per direction after overlay overhead, which
gives (per node, 50% reads):

| Object size | Most ops/s on 1 GbE | At 1,000 ops/s |
|---|---|---|
| 500 KiB | about 165 | needs about 530 MB/s per node (4.3 Gbit/s) |
| 128 KiB | about 650 | about 135 MB/s (over the limit) |
| 64 KiB | about 1,300 | about 68 MB/s (fits) |
| 16 KiB | about 5,000 | about 17 MB/s |

Two runs show the model's edges: 500 KiB objects topped out at about 150 ops/s no matter how many
connections or Riak nodes were used (Riak and the clients were mostly idle; the 1 GbE links were not), and
128 KiB at 600 ops/s was sustained for 4 hours. See [the capacity chart](soak-test-results.md#what-the-network-allows)
and check your own NICs before choosing a target: a node's physical NIC speed is in
`/sys/class/net/<nic>/speed`, and a 10 GbE port that is down does not help.

!!! warning "A 3-node cluster does not survive a node restart with `pr=2` / `pw=2`"
    With only 3 nodes Riak cannot place all three replicas of every partition on different nodes (its
    `target_n_val` is 4). When one node restarts, some partitions are left with a single live primary and
    requests asking for 2 (`pr_val_unsatisfied`, `pw_val_unsatisfied`) fail for those keys, for the minutes
    the node needs to come back. In the smoke runs a rolling restart under load meant 9–16% failed
    operations for about two minutes per restart (nothing lost or corrupt). Any change that rolls the
    nodes (a memory increase, an upgrade) causes this; with `pr=1`/`pw=1`, or five or more nodes, it does not.
    The report's "steady state" line leaves these windows out.

The client behind it, `examples/stressapp/riak_stress.py`, also takes `--rate` (constant open-loop rate),
`--pr`/`--pw`, several buckets (`--bucket a,b,c` with one `--bucket-type` or one per bucket), `--keyspace`
and `--window`.

### Verifying Riak metrics

With `-monitoring` (or `make scale-test MONITORING=true`) every RiakCluster is
created with `spec.monitoring.enabled: true`, so each Riak pod gets the
`json_exporter` sidecar (see
[Prometheus metrics](operator-configuration.md#prometheus-metrics-specmonitoring)).
After convergence the harness checks that:

- every cluster reports `status.monitoringStatus.enabled` and `exporterReady`;
- the cluster's PodMonitor selects every Riak pod (or its ServiceMonitor selects a Service) that exposes the `metrics` port, so Prometheus would find a target per node;
- **every node** serves the exporter's Riak probe — scraped through the
  apiserver pod proxy (`/api/v1/namespaces/<ns>/pods/<pod>:7979/proxy/probe`),
  so no Prometheus is needed — with `riak_node_gets_total`,
  `riak_node_puts_total`, `riak_vnode_gets_total`, `riak_memory_system` and
  `riak_ring_num_partitions` present;
- `riak_ring_num_partitions` equals the cluster's configured `ring_size`, which
  cross-checks the exporter against live Riak rather than just its shape;
- the value series are plausible: memory (`riak_memory_system`,
  `riak_memory_processes`) and `riak_sys_process_count` are above zero and no
  series is negative.

Then, because provisioning sends no data and the traffic counters would
otherwise only ever be checked at zero, it **exercises Riak**: it runs one
`riak-admin test` write/read cycle through the first node of each cluster, scrapes
every node again and checks that

- `riak_node_puts_total` rose by exactly 1 and `riak_node_gets_total` by at
  least 1 on the node that took the request;
- `riak_vnode_puts_total`, **summed over the cluster**, rose by at least a write
  quorum (2) — the object is replicated `n_val` times, so a node that only
  counted its own writes would fail this.

It prints the key series for every node (partitions, vnode and node puts/gets with
the change caused by the test write, memory, Erlang process count and active
protocol-buffer connections).

It retries for `-verify-timeout` and prints `METRICS OK` or `METRICS:` lines.
It also works with `-verify-only -monitoring` against a namespace kept with
`-keep`. The harness does not need the Prometheus Operator; if it is installed,
the operator also creates a `ServiceMonitor` per cluster, so Prometheus scrapes
the fleet during the run and you can chart Riak (`riak_node_*`) next to the
operator's own series.

```bash
go run ./test/scale -clusters 3 -users 10 -buckets 10 -replicas 3 -monitoring
```

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
