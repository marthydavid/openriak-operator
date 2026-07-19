# Operator Configuration

The operator binary accepts the following flags. In a standard deployment they are
set in the manager container args (`config/manager/manager.yaml`).

| Flag | Default | Description |
|------|---------|-------------|
| `--riak-image` | `ghcr.io/marthydavid/riak:3.2.6` | Default Riak operand image used when `spec.image` is not set on a RiakCluster |
| `--max-concurrent-reconciles` | `1` | Max concurrent reconciles per controller; raise to speed up provisioning at scale (see [scaling.md](scaling.md)) |
| `--leader-elect` | `false` | Enable leader election (required when running more than one replica) |
| `--health-probe-bind-address` | `:8081` | Address for the liveness/readiness probe endpoint |
| `--metrics-bind-address` | `0` (disabled) | Address for the metrics endpoint; `:8443` for HTTPS, `:8080` for HTTP |
| `--metrics-secure` | `true` | Serve metrics over HTTPS; set `--metrics-secure=false` for plain HTTP |
| `--enable-http2` | `false` | Enable HTTP/2 on the metrics and webhook servers (disabled by default to mitigate CVE-2023-44487 and CVE-2023-39325) |

Standard [controller-runtime zap flags](https://sdk.operatorframework.io/docs/building-operators/golang/references/logging/)
(`--zap-log-level`, `--zap-encoder`, …) are also available.

## Choosing the Riak operand image

The image for Riak pods is resolved in this order:

1. `spec.image` on the individual `RiakCluster` — always wins when set
2. The operator's `--riak-image` flag
3. The built-in default (`ghcr.io/marthydavid/riak:3.2.6`)

Published operand variants: `riak:3.0.16` and `riak:3.2.6` (the default) are multi-arch
(amd64 + arm64); `riak:3.4.0` is amd64-only — upstream's only aarch64 RPMs for 3.4 target
graviton3/SVE and crash on generic arm64 CPUs. Minor aliases `3.0`/`3.2`/`3.4` exist. amd64
images are RHEL-UBI-based; arm64 images use Amazon Linux with upstream's Graviton builds.
Any of them works with `--riak-image` or `spec.image`.

Use `--riak-image` to pin the fleet-wide default — for example an internal registry
mirror — without editing every `RiakCluster`:

```yaml
# config/manager/manager.yaml
args:
  - --leader-elect
  - --health-probe-bind-address=:8081
  - --riak-image=registry.example.com/mirrors/riak:3.2.6
```

Individual clusters can still override it:

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: canary
spec:
  size: 1
  image: registry.example.com/mirrors/riak:3.3.0-rc1
```

Changing `--riak-image` (or `spec.image`) triggers a rolling update of the
StatefulSet on the next reconcile of each affected cluster.

## Riak configuration (`spec.riakConfig`)

`RiakCluster.spec.riakConfig` passes **any riak.conf key** to every node — the operator maps
each key to the entrypoint's `RIAK_CONFIG_*` environment scheme and nodes render them into
`riak.conf` at startup. Changing values rolls the StatefulSet automatically.

> **Warning:** changing `storage_backend` (or a bucket type's `backend` binding) on a
> cluster that already holds data does **not** migrate the data — objects stored in the
> previous backend become unreachable. Take a backup or plan a migration before switching
> backends on a live cluster; pick the backend layout up front where possible.

### Memory backend with TTL

```yaml
spec:
  riakConfig:
    storage_backend: memory
    memory_backend.ttl: 60s
    memory_backend.max_memory_per_vnode: 128MB
```

### Multi-backend: durable default + TTL'd cache buckets

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: my-cluster
spec:
  riakConfig:
    storage_backend: multi
    multi_backend.default: bitcask_data
    multi_backend.bitcask_data.storage_backend: bitcask
    multi_backend.mem_ttl.storage_backend: memory
    multi_backend.mem_ttl.memory_backend.ttl: 60s
    multi_backend.mem_ttl.memory_backend.max_memory_per_vnode: 32MB
---
# Buckets of this type live on the memory backend and expire after 60s.
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata:
  name: cache-bucket
spec:
  clusterName: my-cluster
  bucketName: cache
  bucketType: cache
  properties:
    backend: mem_ttl
```

Any other riak.conf key works the same way (bitcask/leveldb tuning, AAE, limits, …) — see the
[Riak configuration reference](https://www.tiot.jp/riak-docs/riak/kv/latest/configuring/reference/).

## Prometheus metrics (`spec.monitoring`)

Riak has no native Prometheus endpoint — it exposes a JSON document at `GET /stats` on the HTTP
port (~470 numeric fields). Enabling monitoring adds a `json_exporter` **sidecar** to every Riak
pod that translates `/stats` into Prometheus metrics on port 7979, exposes that port on the
cluster Service, and (when the Prometheus Operator CRDs are present) creates a `ServiceMonitor`.
Clusters without the Prometheus Operator are supported: the ServiceMonitor is skipped, and
Prometheus can scrape the exporter directly at
`http://<pod>:7979/probe?module=riak&target=http://127.0.0.1:8098/stats` (the exporter's own
`/metrics` path serves only json_exporter's internal metrics, not Riak's).

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: my-cluster
spec:
  size: 3
  monitoring:
    enabled: true
    # exporterImage: quay.io/prometheuscommunity/json-exporter:v0.6.0  # override optional
```

Exported series are prefixed `riak_` and cover throughput (`riak_node_gets`, `riak_node_puts`,
`riak_vnode_*`), GET/PUT latency percentiles (`riak_node_get_fsm_time_95`/`_99`), read repairs,
coordinator redirects, protocol-buffer connections, object sizes, and Erlang VM internals
(`riak_memory_processes`, `riak_sys_process_count`, `riak_ring_num_partitions`). The mapping
lives in a ConfigMap the operator generates; scrape metrics from the exporter's `/probe` endpoint
(`/probe?module=riak&target=http://<pod>:8098/stats`), which is what the generated ServiceMonitor
does. These names map directly onto the community Riak Grafana dashboards.
