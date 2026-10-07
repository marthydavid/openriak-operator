# DR game day 1

The first run of the disaster-recovery plan in [issue #98](https://github.com/marthydavid/openriak-operator/issues/98):
a constant load on one TLS cluster while failures are injected one after another, on the test OKD cluster
(see [Test environment](test-environment.md)). The script is
[`hack/dr-gameday.sh`](https://github.com/marthydavid/openriak-operator/blob/test/dr-gameday/hack/dr-gameday.sh),
the load and the read-back are the [soak harness](scaling.md#soak-test-a-constant-load-for-hours).

**Status: in progress.** This page is updated as each fault finishes.

## Setup

| | |
|---|---|
| Date | 2026-10-07, started 22:44 (CEST) |
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
| D7 | delete two pods at once | pending | pending | pending |
| D5 | delete one node's volume and pod | pending | pending | pending |
| D3 | isolate one pod with a NetworkPolicy for 3 minutes | pending | pending | pending |
| Read-back and `-verify-only` | every key read back after the load; Riak compared with the CRs | pending | pending | pending |

## Notes

- **D1**: the replacement pod was `Running` 2/2 and the ring was whole when the cluster reported `Ready`; no
  restarts or OOM kills on the other nodes.
