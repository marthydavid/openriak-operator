# RiakBucket

A `RiakBucket` creates and activates a Riak **bucket type** on a cluster and applies properties to
it. The cluster must be `Ready`; until then the bucket waits and retries.

## Example

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata:
  name: orders
spec:
  clusterName: demo
  bucketName: orders
  bucketType: orders
  nVal: 3
  allowMulti: true
  properties:
    last_write_wins: "false"
```

## Spec reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterName` | string | **required** | `RiakCluster` in the same namespace |
| `bucketName` | string | **required** | Bucket name |
| `bucketType` | string | `default` | Bucket type to create/use |
| `nVal` | int ≥ 1 | Riak default | Replicas (`n_val`) |
| `replicationFactor` | int, 1–999 | Riak default | Alias of `n_val`; `nVal` wins if both are set |
| `allowMulti` | bool | `false` | Allow siblings (`allow_mult`). Only sent when `true` |
| `properties` | map[string]string | — | Any other bucket properties |

Values in `properties` that parse as numbers or booleans are sent to Riak as native JSON types.
The typed fields (`nVal`, `replicationFactor`, `allowMulti`) take precedence over the same key in
`properties`.

## Binding a bucket to a backend

With a multi-backend [`riakConfig`](../operator-configuration.md#multi-backend-durable-default-ttld-cache-buckets):

```yaml
spec:
  clusterName: demo
  bucketName: cache
  bucketType: cache
  properties:
    backend: mem_ttl
```

!!! warning
    Re-binding a bucket type to a different backend does not move existing data.

## Status

```bash
kubectl get riakbucket
# NAME     PHASE   READY   TYPE     AGE
```

| Field | Meaning |
|-------|---------|
| `phase` | `Creating`, `Ready`, `Failed` |
| `bucketName`, `bucketType` | What was created (`default` filled in when the spec leaves it empty) |
| `nVal`, `replicationFactor` | Values applied |
| `properties` | The exact property set sent to `riak-admin` (spec properties + typed fields) |
| `nodes[]` | Cluster nodes serving the bucket at last reconcile |
| `error` | Failure detail when `phase: Failed` |
| `conditions` | `Ready` condition |

## Granting access

Buckets carry no permissions themselves. Grant access with
[`RiakUser.spec.grants`](riakuser.md) using `resource: bucket` and the `bucketName`.
