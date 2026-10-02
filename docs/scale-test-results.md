# Scale test results

Results of the `test/scale` harness against a real OpenShift (OKD) cluster. The test
does not stop at "every CR says `Ready`": after convergence it **reads the state back
from Riak on every node** and compares it with what the custom resources declare.

!!! success "Latest run (operator v0.0.9) — everything matches"
    3 clusters × 3 nodes, 60 users, 60 buckets, on persistent volumes, using the
    published images. Riak held exactly what the CRs declare at all three verification
    stages, the rings were evenly balanced, the Riak metrics were correct on every node
    (including after a real write), no Riak pod restarted and neither did the operator.

!!! warning "The default 128Mi operator memory limit was too small"
    The first v0.0.9 run had the operator **OOMKilled in a loop** at the old 128Mi default
    while it resynced this small fleet (peak about 180Mi). Convergence took 11m51s / 13m07s /
    15m49s instead of about 2 / 3 / 6 minutes. The default is now a 512Mi limit with a 128Mi
    request ([#48](https://github.com/marthydavid/openriak-operator/issues/48)), and the harness
    now fails a run when the operator restarts or is OOMKilled. The numbers below are the clean
    run with the fixed limit.

## Environment

Full details of the cluster (hardware, storage, services, what else runs on it) are on the
[Test environment](test-environment.md) page.

| | |
|---|---|
| Cluster | OKD, 3 schedulable control-plane nodes (12 CPU / ~125 GiB each), shared with other workloads |
| Storage | LVM Storage (TopoLVM) thin pool on a dedicated disk, StorageClass `lvms-vg1` |
| Operator | `ghcr.io/marthydavid/openriak-operator` `0.0.9`, `--max-concurrent-reconciles=1` (default), memory limit 512Mi / request 128Mi |
| Riak | `ghcr.io/marthydavid/riak` `3.2.6`, `ring_size: 128`, one PVC per node |
| Both images | pulled by digest |

## Scenario

```bash
scale-test -clusters 3 -users 20 -buckets 20 -replicas 3 -ring-size 128 -monitoring \
  -operator-namespace openriak-operator -storage-class lvms-vg1 -keep -mutate -delete-users-every 4
```

Each of the 3 clusters has 3 Riak nodes, 20 certificate-authenticated users and 20 buckets.
Every user gets 1–4 random grants: mostly on a whole bucket type, some on a bucket inside a
type, a few on `any`. Every bucket gets a random `n_val` and `allow_mult`. Every cluster runs
with `spec.monitoring.enabled: true`, so each Riak pod has the Prometheus exporter sidecar.

## Timings

| Resource | All Ready after |
|---|---|
| 3 RiakClusters (9 nodes, rings formed) | 1m50s |
| 60 RiakBuckets | 3m06s |
| 60 RiakUsers | 5m40s |
| Whole scenario | 5m40s |

Users are the slow part: the operator reconciles users one at a time, and each user is
several `riak-admin` calls over `kubectl exec`. See [Scaling](scaling.md) for the
`--max-concurrent-reconciles` lever.

## What is verified, on every node

| Check | Passes when |
|---|---|
| Ring membership | every node reports `Valid:N / Leaving:0 / Exiting:0 / Joining:0 / Down:0` |
| Ring balance | each node owns `ring_size / nodes` partitions, rounded down or up (43/42/43 of 128) |
| Bucket types | every RiakBucket's type is active |
| Bucket properties | `n_val` and `allow_mult` equal the spec |
| Users | each RiakUser exists with a `certificate` source, and Riak has no user that no RiakUser declares |
| Grants | each user's dedicated permissions equal the spec **exactly** (nothing missing, nothing extra) |

The harness maps CRD permissions to Riak permission tokens with its own table, so a mistake
in the operator's mapping cannot hide itself.

The checks run after three stages:

| Stage | Facts checked against Riak | Result |
|---|---|---|
| After convergence | 918 | match |
| After changing the grants of 20 users and the properties of 20 buckets | 918 | match |
| After deleting 15 RiakUsers | 828 | match |

After the run all 9 Riak pods had **0 restarts**, and every ring was `Valid:3`. The operator
pod also had **0 restarts** (`OPERATOR OK`).

## Riak metrics

With `-monitoring` the harness also checks Riak through the metrics the operator exposes
(the `json_exporter` sidecar, scraped through the apiserver pod proxy, so no Prometheus is
needed). Provisioning sends no data, so the harness then **writes to Riak** — one
`riak-admin test` write/read cycle through the first node of every cluster — and checks that
the counters move.

| Check | Passes when |
|---|---|
| Exporter ready | every cluster reports `monitoringStatus.enabled` and `exporterReady` |
| Series present | every node serves `riak_node_gets_total`, `riak_node_puts_total`, `riak_vnode_gets_total`, `riak_memory_system`, `riak_memory_processes`, `riak_sys_process_count`, `riak_node_get_fsm_time_95`, `riak_ring_num_partitions` |
| Ring size | `riak_ring_num_partitions` equals the cluster's configured `ring_size` (128) |
| Plausible values | memory and process count above zero, no negative series |
| Write seen by the node | on the node that took the write, `riak_node_puts_total` rose by exactly 1 and `riak_node_gets_total` by at least 1 |
| Write seen across replicas | `riak_vnode_puts_total`, summed over the cluster, rose by at least a write quorum (2) |

What the run reported after the write (one cluster, change from the write in brackets):

| Node | partitions | vnode puts | vnode gets | node puts | node gets | memory | Erlang processes |
|---|---|---|---|---|---|---|---|
| `scale-c000-0` (took the write) | 128 | 1 (+1) | 2 (+2) | 1 (+1) | 2 (+2) | 79 MiB | 1607 |
| `scale-c000-1` | 128 | 1 (+1) | 2 (+2) | 0 | 0 | 78 MiB | 1583 |
| `scale-c000-2` | 128 | 1 (+1) | 2 (+2) | 0 | 0 | 78 MiB | 1605 |

The write was replicated to all three nodes (every node's `vnode_puts` rose by one, `n_val` 3)
while only the node that took the request counted a client-level put, which is what a correct
exporter should show. The other two clusters behaved the same. Memory per node was 77–80 MiB
and there were no active protocol-buffer connections (`pbc_active` 0).

## What the test found

Running it against a real cluster (and verifying Riak rather than trusting `status`) turned
up these problems, all fixed:

| Problem | Effect | Fix |
|---|---|---|
| The entrypoint waited for `riak ping`, which never succeeds in the image | every Riak pod exited with code 1 after ~2m17s, while the TCP-only probes kept it looking healthy | [#35](https://github.com/marthydavid/openriak-operator/pull/35), then [#36](https://github.com/marthydavid/openriak-operator/pull/36) |
| Nodes were never joined into a ring | multi-node clusters reported `Ready 3/3` but every node was its own one-member ring | operator joins nodes to `<cluster>-0`; a multi-node cluster is Ready only when all nodes are ring members ([#36](https://github.com/marthydavid/openriak-operator/pull/36)) |
| Node names were not usable between pods | Erlang rejects a dotless remote host (`Hostname … is illegal`), so `riak@<pod>` only works single-node | nodes are named by their pod FQDN; `riak-admin` is run with `VMARGS_PATH` set ([#36](https://github.com/marthydavid/openriak-operator/pull/36)) |
| Grants removed from a RiakUser stayed in Riak | stale access | grants are reconciled to the spec; stale ones are revoked ([#36](https://github.com/marthydavid/openriak-operator/pull/36)) |
| Deleting a RiakUser left the Riak user behind | working identity after deletion | the Riak user is deleted (best effort, bounded) ([#36](https://github.com/marthydavid/openriak-operator/pull/36)) |
| `riak-admin` exits 0 even when a command fails | failed commands counted as success; edits to a RiakBucket never reached Riak | the operator reads Riak's reply and fails on errors; existing bucket types are changed with `bucket-type update` ([#36](https://github.com/marthydavid/openriak-operator/pull/36)) |
| Operator memory limit of 128Mi (v0.0.9) | the operator was OOMKilled in a loop while resyncing a small fleet and the fleet stalled; it only showed as slow convergence | default raised to 512Mi limit / 128Mi request; manifest and chart defaults are guarded by a test, and the harness fails on operator restarts ([#48](https://github.com/marthydavid/openriak-operator/issues/48)) |
| `-monitoring` check expected `ring_size` 8 | the metrics check failed on every node once the harness used a 128-partition ring | compares with each cluster's configured ring size |
| Test used `ring_size: 8` | 8 partitions over 3 nodes is 4/2/2 (50/25/25 %) | harness requires `ring_size` ≥ 128 and verifies balance; the Riak entrypoint now defaults to 128 ([#37](https://github.com/marthydavid/openriak-operator/pull/37)) |

!!! tip "Use a ring size of at least 128"
    A ring is divided into whole partitions, so a small ring cannot be spread evenly over a
    few nodes. `ring_size` cannot be changed on an existing cluster — recreate it.

## Limits of this test

- It is a **control-plane** test: it proves the operator provisions Riak correctly and
  quickly. Data traffic is a single write/read cycle per cluster, enough to check that the
  metrics move, not a load test.
- One clean run on one three-node cluster shared with other workloads; timings are indicative,
  not a benchmark.
- The operator ran with a single reconcile worker. Raising `--max-concurrent-reconciles`
  was not tested here.
- Verification reads Riak's state through `riak-admin`; it does not authenticate with the
  issued client certificates.

## Run it yourself

The harness lives in `test/scale`. Build it and run it next to a cluster with the operator,
cert-manager and a StorageClass installed:

```bash
go build -o scale-test ./test/scale
./scale-test -clusters 3 -users 20 -buckets 20 -replicas 3 -ring-size 128 \
  -storage-class <your-class> -mutate -delete-users-every 4
```

`-verify-only` checks an existing namespace without creating anything. See
[Scaling](scaling.md) for the other flags.
