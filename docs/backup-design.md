# Automated backups: design proposal

Status: **proposal, not implemented.** Seeks agreement on the approach before any code.

## Goals

- Scheduled, retained backups of a RiakCluster's data, declared as Kubernetes resources.
- Restore into a new cluster from a named backup.
- Backup state visible in status, conditions and `/metrics` like the other CRDs.

Non-goals: point-in-time recovery, cross-cluster replication (that is the multi-datacenter item).

## Constraints from Riak

- Riak has no built-in snapshot or dump command. A node's data lives under `/var/lib/riak` (the
  `data-<pod>` PVC), and the on-disk backend files are only safely copied when the node is
  stopped or the files are frozen at one instant.
- Each node holds replicas (`n_val`, default 3), so a **per-node** copy is enough to restore that
  node; a consistent **cluster** backup means copying all nodes close in time.
- The ring state (`/var/lib/riak/ring`) must be captured with the data, or restore must re-join.
- Users and security config are reconciled from the RiakUser/RiakBucket CRs, so restore only
  needs the data and ring, not Riak-side security state.

## Option A: CSI VolumeSnapshots (recommended)

New CRDs:

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakBackup            # one point-in-time backup
spec:
  clusterRef: {name: mycluster}
  volumeSnapshotClassName: lvms-vg1
status:
  phase: Pending|Running|Completed|Failed
  snapshots: [{pod: mycluster-0, name: ..., readyToUse: true}]
---
kind: RiakBackupSchedule    # creates RiakBackups on a cron
spec:
  clusterRef: {name: mycluster}
  schedule: "0 2 * * *"
  retention: {keepLast: 7}
```

Controller flow for a `RiakBackup`: for every ready pod create a `VolumeSnapshot` of
`data-<pod>` (all in one reconcile pass, so they land seconds apart), watch `readyToUse`, then
set `Completed`. The schedule controller creates RiakBackups and prunes beyond `keepLast`
(deleting a RiakBackup deletes its snapshots through a finalizer).

Consistency: snapshots are crash-consistent (like a power loss). Riak is expected to recover
from this, but that must be verified per backend (see open questions).

Restore: `RiakCluster.spec.restoreFrom: {backupRef: name}` makes the StatefulSet's
`volumeClaimTemplates` use `dataSource` = the backup's per-pod snapshot. PVC templates are
immutable, so this only applies at creation: restore means creating a new cluster, which is also
the safe default.

Pros: no data movement, no credentials, fast, reuses the storage layer. Cons: needs a CSI driver
with snapshot support; snapshots live in the same storage backend (not offsite unless the driver
replicates); crash-consistent only.

## Option B: archive to object storage

A Job per node tars `/var/lib/riak` and uploads to S3-compatible storage using a credentials
Secret referenced by `spec.destination`. Offsite and portable, but needs the node quiesced or
stopped for a consistent copy (a rolling stop of one node at a time is safe with n_val >= 2 but
disruptive), a tool in the image, credentials handling, and all data flows through the pod.
Better as a follow-up (`RiakBackup.spec.destination`) once A exists.

## Plan

1. `RiakBackup` + controller (snapshot creation, status, finalizer cleanup), envtest unit tests.
2. `RiakBackupSchedule` + retention.
3. Restore through `RiakCluster.spec.restoreFrom`.
4. Metrics (`riak_backup_*`), Helm chart CRDs/RBAC (`volumesnapshots`), docs and an example.
5. Verify on metal1: write data, back up, delete the cluster, restore, run `test/scale -verify`.

## Open questions

- Is crash-consistent acceptable, or is a quiesce step required? Test restoring snapshots taken
  under write load for each backend (bitcask, leveldb, leveled).
- Does metal1's LVMS expose a VolumeSnapshotClass? (not yet checked)
- Should ring state restore as-is, or should restored nodes rejoin a fresh ring?
- Do we want Option B in the first release for offsite copies?
