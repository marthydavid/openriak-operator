# Basho Bench on OpenShift, against an mTLS cluster

See [docs/basho-bench.md](../../docs/basho-bench.md) for the full write-up.

| File | Purpose |
|---|---|
| `build.yaml` | Namespace and `BuildConfig` that builds `OpenRiak/basho_bench` (branch `openriak-3.2`) with the TLS fix for the protobuf driver and pushes it to the internal registry |
| `soak-like.py` | Prints the cluster, buckets, users, configs and client Jobs of a run shaped like `test/scale -soak` |
| `collect.py` | Records memory, disk, `riak-admin status` and node usage during the run |
| `to-artifacts.py` | Converts the clients' CSVs and the collected series to the soak artifact format for `hack/soak-report.py` |
