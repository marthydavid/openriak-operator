# Example application: Riak stress client

`riak_stress.py` is a small, self-contained **example client application** for a Riak cluster run by the
operator. It hammers the cluster with a mix of writes and reads over the Protocol Buffers interface,
authenticating with the **client certificate of a `RiakUser`** (mTLS, certificate auth), and reports
throughput, latency percentiles and data integrity. It is what `test/scale -stress` runs against every
cluster, but it works standalone against any TLS-enabled `RiakCluster`.

It uses **only the Python standard library**, so it runs in any stock python image with the user's client
certificate Secret mounted. There is no image to build and nothing to `pip install`.

## What it does

- Opens `--threads` connections (StartTLS, client certificate, certificate auth) to the cluster's client
  Service, so the connections spread over the Riak nodes.
- Each thread owns its own keys (`<prefix>t<thread>-<n>`): one writer per key, so there are no sibling
  conflicts, and every value is derived deterministically from *(thread, key, version)*.
- Runs a timed mix of writes (new keys, and overwrites of its own keys with the previous vclock) and
  reads (of keys it has written). A read that finds no value counts as **lost**, a wrong value as
  **corrupt**.
- After the timed phase it reads back **every key it wrote** (the final verification).
- Prints progress to stderr and one final line to stdout:

  ```
  RESULT {"corrupt": 0, "duration_s": 60.03, "errors": 0, "final_corrupt": 0, "final_lost": 0, "gets": 301229,
          "latency_ms": {"get": {"max": ..., "p50": 1.9, "p95": 12.5, "p99": 41.9},
                         "put": {"max": ..., "p50": 2.8, "p95": 17.0, "p99": 48.0}},
          "lost": 0, "ops": 430000, "ops_per_s": 7051.0, "puts": 128766, "siblings": 0, "verified": 86269}
  ```

  Exit status is `0` when there were no errors, lost or corrupt values, otherwise `2`.

## Run it against a cluster

You need a TLS-enabled RiakCluster, a RiakBucket (for the bucket type) and a RiakUser with grants on that
type. Example resources (names are placeholders):

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: {name: demo}
spec:
  size: 3
  tls:
    enabled: true
    certManager: {issuerName: my-ca-issuer}      # a CA-backed Issuer, see below
  riakConfig: {ring_size: "128"}
---
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata: {name: demo-stress}
spec: {clusterName: demo, bucketName: stress, bucketType: demo-tstress}
---
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata: {name: demo-stress}
spec:
  clusterName: demo
  username: demo_stress
  certificateRef: {issuerRef: {name: my-ca-issuer, kind: Issuer}}
  grants:
    - {resource: bucket, bucketName: demo-tstress, permission: admin}
```

The client verifies the server certificate with the `ca.crt` in its own client-certificate Secret, so the
cluster's TLS certificate and the user's certificate must come from the **same CA** (a CA-backed `Issuer`,
not a self-signed one per certificate).

Then create the script ConfigMap and the Job from [`job.yaml`](job.yaml):

```bash
kubectl create configmap riak-stress-script --from-file=examples/stressapp/riak_stress.py
kubectl apply -f examples/stressapp/job.yaml
kubectl logs -f job/demo-stress
```

## Options

| Option | Default | |
|---|---|---|
| `--host` | required | the cluster Service, e.g. `demo.default.svc.cluster.local` (must be in the server certificate) |
| `--user` | required | Riak username, equal to the client certificate's CN |
| `--cert --key --cacert` | required | the mounted client certificate files |
| `--bucket-type --bucket` | required | where to read and write |
| `--threads` | 16 | connections |
| `--duration` | 60 | seconds of timed load |
| `--value-size` | 1024 | bytes per object |
| `--read-ratio` | 0.7 | fraction of operations that are reads |
| `--key-prefix` | none | distinguishes parallel clients, e.g. `c0-` |
| `--port` | 8087 | protocol buffers port |

## As part of the scale test

`test/scale -stress` creates the TLS-enabled clusters, a CA-backed issuer and one stress user and bucket
per cluster, runs this application as Jobs, and then checks the clients' results **and** Riak's own metrics
against them. See [Scaling](https://marthydavid.github.io/openriak-operator/scaling/#stress-testing-riak).

## Notes

- The Python client is not fast: it is a load generator for checking a cluster's correctness and behavior
  under concurrent load, not a benchmark of Riak's maximum throughput. Add more clients (pods) to push harder.
- Avoid labelling the client pods `cluster=<riak cluster name>`: clusters created by operators older than
  0.0.11 select pods by that label alone in their required anti-affinity, so such a pod could not be scheduled
  on any node that runs a node of that cluster. Newer clusters select `app=riak,cluster=<name>` and are unaffected.
- Tests for the protocol helpers: `python3 -m unittest examples/stressapp/test_riak_stress.py`.

## Constant rate, quorums and long runs

For soak tests the client can hold a fixed load instead of going as fast as it can:

| Flag | Meaning |
|------|---------|
| `--rate OPS` | constant total operations per second (open loop: each thread follows a fixed schedule and never bursts to catch up; missed slots are counted as `late`) |
| `--pr N` / `--pw N` | primary read / write quorum sent with every request |
| `--bucket a,b,c` | rotate over several buckets; `--bucket-type` is one type for all, or one per bucket in the same order |
| `--keyspace N` | keys each thread keeps per bucket; once full every write overwrites, so client memory stays bounded on long runs |
| `--window S` | print a `WINDOW {json}` line to stderr every S seconds: that interval's ops/s, failed operations, late slots and p50/p95/p99 |

The offline tests (`make test-stressapp`) cover the pacing, the quorum fields, the bucket rotation and the
bounded keyspace against an in-memory fake Riak.
