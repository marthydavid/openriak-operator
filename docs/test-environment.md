# Test environment

The cluster the [scale tests](scale-test-results.md) run on. It is a small, ordinary,
**shared** bare-metal OpenShift (OKD) cluster, not a purpose-built benchmark rig, so the
timings in the results are indicative rather than a benchmark. Every figure below was read from
the cluster itself.

## Cluster

| | |
|---|---|
| Distribution | OKD `4.22.0-okd-scos.10` (OpenShift on CentOS Stream CoreOS 10) |
| Kubernetes | `v1.35.5` |
| Container runtime | CRI-O `1.35.5` |
| Operating system / kernel | CentOS Stream CoreOS 10, Linux `6.12.0-254.el10.x86_64` |
| Platform | bare metal, highly available |
| Topology | **3 nodes, each control-plane + worker** (all schedulable, so the Riak pods share them with the control plane) |
| Pod network | OVN-Kubernetes |
| Architecture | `amd64` |

## Nodes

The three nodes are identical Supermicro bare-metal servers (not virtual machines):

| | |
|---|---|
| CPU | Intel Xeon E-2146G @ 3.50 GHz, 1 socket, 6 cores / 12 threads (12 logical CPUs) |
| Memory | about 125 GiB per node (about 377 GiB in the cluster) |
| Ephemeral storage | about 465 GiB per node |
| OS disk | Samsung 970 EVO / EVO Plus 500 GB NVMe |
| Data disks | two 1 TB SATA SSDs (Crucial MX500) and one 118 GB SSD; the Riak volumes use `/dev/sda` |

## Storage

Riak data volumes come from **LVM Storage** (LVMS operator `4.21.1`, TopoLVM CSI driver). All three
nodes are `Ready` and identical.

| | |
|---|---|
| StorageClass | `lvms-vg1`, provisioner `topolvm.io`, `WaitForFirstConsumer`, reclaim policy `Delete`, volume expansion allowed |
| Device class | `vg1` on `/dev/sda` only, XFS, thin pool using 90 % of the volume group, overprovision ratio 10 |
| Capacity | about 838 GiB thin pool per node (about 8.2 TiB schedulable per node at overprovision 10) |
| Volumes in the tests | one 10 GiB PVC per Riak node (the operator default), so 9 PVCs for 3 clusters × 3 nodes |

## Cluster services the tests use

| | |
|---|---|
| cert-manager | cert-manager Operator for Red Hat OpenShift `1.20.1`; the harness creates a self-signed `Issuer`, and every RiakUser gets a client certificate from it |
| Monitoring | OpenShift platform monitoring and user-workload monitoring are enabled. The Prometheus Operator CRDs are present, so the operator creates a scrape object per cluster when `spec.monitoring` is on (a `ServiceMonitor` in v0.0.9, which the recorded runs used; a `PodMonitor` by default since `spec.monitoring.scrapeKind` was added) |
| Other operators installed | Local Storage `4.21`, KubeVirt HyperConverged Cluster Operator `1.18.1` (no virtual machines were running), Custom Metrics Autoscaler `2.19` |
| Image pulls | Operator and Riak images are pulled **by digest** through the cluster's registry mirror; some test builds use in-cluster `BuildConfig`s |

## It is shared, not dedicated

About 390 pods run on the cluster (platform components, monitoring and other add-ons). While the
tests ran, other workloads kept the nodes at roughly **20–50 % CPU**.

What the scale test itself adds (3 clusters × 3 nodes):

| | |
|---|---|
| Riak pods | 9, each requesting the operator default of 1 CPU and 2 GiB (9 CPU / 18 GiB requested, of 36 CPUs / about 377 GiB in the cluster) |
| Actual Riak memory | about 77–80 MiB per node at idle (`riak_memory_system`) |
| Exporter sidecars | 9 (`json_exporter`), when `-monitoring` is on |
| Operator | one pod, 512 MiB memory limit and 128 MiB request, one reconcile worker per controller |
| Custom resources | 3 RiakClusters, 60 RiakUsers (60 client certificates), 60 RiakBuckets |

## Running it on your own cluster

You need at least:

- 3 schedulable nodes with room for the Riak requests above (each node hosts three Riak pods in this
  scenario, so about 3 CPUs and 6 GiB of requests per node);
- a StorageClass that can provision `ReadWriteOnce` volumes (or use the harness's `-ephemeral`);
- cert-manager;
- optionally the Prometheus Operator, for `-monitoring` and the `PodMonitor`/`ServiceMonitor` the operator creates.

See [Scaling](scaling.md) for the harness flags and [Scale test results](scale-test-results.md) for the
scenario and what is verified.
