# DR game day 1

The first run of the disaster-recovery plan in [issue #98](https://github.com/marthydavid/openriak-operator/issues/98):
a constant load on one TLS cluster while failures are injected one after another, on the test OKD cluster
(see [Test environment](test-environment.md)). The script is
[`hack/dr-gameday.sh`](https://github.com/marthydavid/openriak-operator/blob/test/dr-gameday/hack/dr-gameday.sh),
the load and the read-back are the [soak harness](scaling.md#soak-test-a-constant-load-for-hours).

**Status: in progress.** This page is updated as each fault finishes. Findings so far: D5 (finding 1) and D3 (finding 2), below.

## Setup

| | |
|---|---|
| Date | 2026-10-07, started 22:25 (CEST); baseline from 22:28, first fault 22:38 |
| Operator | `ghcr.io/marthydavid/openriak-operator:1.0.0` (`sha256:7b6fe7e4…`), swapped in for this run |
| Riak | `ghcr.io/marthydavid/riak:3.2.6`, TLS, `ring_size` 128, 3 nodes |
| Node resources | 8 GiB memory (request = limit), 2 CPU request, 50 GiB volume on `lvms-vg1` |
| Pod anti-affinity | `Preferred` (so a fourth node could be scheduled; not used in this run) |
| Load | 200 ops/s, 50 % reads, 16 KiB objects, 10 mTLS users, 10 buckets (one bucket type each), `n_val` 3, `pr`/`pw` 2, 4 connections per client |
| Harness | `test/scale -soak -soak-no-scale`: observes, never changes the cluster; reads every key back at the end |
| Run from | the OKD host `okd-pve` (close to the cluster), detached |
| Cluster | 3 nodes that are control plane and workers at once, so no node was powered off |

Sequence: 10 minutes of baseline, then each fault, waiting for the cluster to be `Ready` again and then
3 more minutes of load before the next. D9 (killing the operator during a scale-up) was left out on request.
Recovery times are measured by polling `kubectl` after a 25 s pause (the operator refreshes the status every
10 s), so they have a resolution of a few seconds.

An earlier start of the same run was stopped after D1 to remove D9 from the sequence; its D1 recovered in the
same 15 s.

## Results

| Fault | What was done | Ready again | Effect on the load | Verdict |
|---|---|---|---|---|
| D1 | delete one Riak pod (`soak-1`) | 16 s after the 25 s pause | one 60 s window at 192 of 200 ops/s with 4.06 % errors, back to 200 ops/s and 0 % in the next; p99 stayed at 46 ms | recovered |
| D7 | delete two pods at once (`soak-1`, `soak-2`) | 31 s after the 25 s pause | the cluster showed 1 of 3 nodes for a sample; two windows at 172 and 170 of 200 ops/s with 13.6 % and 14.0 % errors; p99 of the operations that succeeded stayed at 45 ms | recovered, no restart or OOM elsewhere |
| D5 | delete one node's volume (`data-soak-2`) and pod | `Ready` 15 s after the 25 s pause, **but the node did not rejoin the ring** (see below) | one window at 190 of 200 ops/s with 4.22 % errors, then back to 198 | **operator reports Ready while Riak is split: finding 1** |
| D3 | isolate `soak-1` with a NetworkPolicy (deny all ingress and egress) for 3 minutes, then lift it | `Ready` again on the first check after the policy was removed | no effect: 198 to 199 ops/s, 0 % errors, p99 44 ms throughout | **inconclusive: the isolation did not isolate** (finding 2) |
| Read-back and `-verify-only` | every key read back after the load; Riak compared with the CRs | pending | pending | pending |

## Notes

- **D1**: the replacement pod was `Running` 2/2 and the ring was whole when the cluster reported `Ready`; no
  restarts or OOM kills on the other nodes.
- **D7**: with two of three nodes gone only one primary was left, so reads and writes that need `pr`/`pw` 2
  fail until a second node is back; about 14 % of the operations in the affected minute errored, the rest
  succeeded, and the cluster was `Ready` with three running pods 56 s after the pods were deleted. The client
  windows are 60 s long, so one outage of under a minute shows in two consecutive windows. Whether any
  acknowledged write was lost is only known after the read-back at the end of the run.
- **D5**: the pod and a fresh, empty volume came back within a minute and the operator reported the cluster
  `Ready`. Checked directly in Riak about 80 s after the new volume was created:

  | | What Riak says |
  |---|---|
  | `riak-admin member-status` on `soak-0` | 3 valid members, `soak-2` owns 33.6 % |
  | `riak-admin member-status` on **`soak-2`** | **1 member, itself, owning 100 %** |
  | `riak-admin transfers` on `soak-0`, `soak-1` | waiting to hand off 43 and 23 partitions, no active transfer |
  | `/var/lib/riak/bitcask` | `soak-0` 2.8 GB, `soak-1` 2.0 GB, **`soak-2` 0.4 GB** |

## Findings

1. **A node that comes back with an empty volume is never rejoined, and the cluster still reports `Ready`.**
   The operator joins a node only when it is a standalone ring whose name is *not* in the seed's ring
   (`internal/riak/manager.go`, `ReconcileMembership`), and decides `Ready` from the seed's view alone. After
   a data loss the node has the same name (it is a StatefulSet), so the seed lists it as a valid member and the
   operator skips it, while the node itself runs a one-member ring that owns every partition. The other nodes
   keep hinted data for it that cannot be handed off. The read-back at the end of this run shows whether any
   acknowledged write was lost because of it. Needs an issue: either detect the divergence (compare each node's
   own `member-status` with the seed's, report not `Ready`) and repair it (for example `riak-admin cluster
   replace`/force-replace), or document the manual procedure.
2. **A NetworkPolicy is not a network partition on OVN-Kubernetes (hypothesis, not verified).** With `soak-1`
   isolated in both directions for 3 minutes the clients saw no errors and no latency change, the kubelet's
   readiness probes of `soak-1` never failed (probes are exempt from NetworkPolicy), and the cluster status
   flipped to not `Ready` for two samples only, for a reason that was not captured. The likeliest explanation
   is that the policy stops *new* connections but leaves *established* ones running, and the Riak peer
   (Erlang distribution) and client protobuf connections are long-lived. This run therefore says nothing
   about how Riak behaves when one node is partitioned. A real partition needs a fault that also drops
   established flows (for example Chaos Mesh `NetworkChaos`, which is what [#98](https://github.com/marthydavid/openriak-operator/issues/98)
   proposes for this). A short follow-up test can confirm the explanation: apply the same policy and
   compare an existing connection with a new one from `soak-0` to `soak-1:8087`.

## Still to come

The load runs until about 23:38; then the harness reads every key back, and `-verify-only` compares Riak with
the CRs. Those two results say whether D5 lost acknowledged writes, and are added below.

