# Operator Configuration

The operator binary accepts the following flags. In a standard deployment they are
set in the manager container args (`config/manager/manager.yaml`).

| Flag | Default | Description |
|------|---------|-------------|
| `--riak-image` | `ghcr.io/marthydavid/riak:3.2.6` | Default Riak operand image used when `spec.image` is not set on a RiakCluster |
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

Published operand variants (all multi-arch, amd64 + arm64): `riak:3.0.16`, `riak:3.2.6`
(the default) and `riak:3.4.0`, with minor aliases `3.0`/`3.2`/`3.4`. amd64 images are
RHEL-UBI-based; arm64 images use Amazon Linux with upstream's Graviton builds. Any of them
works with `--riak-image` or `spec.image`.

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
