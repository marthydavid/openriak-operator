#!/usr/bin/env python3
"""Print the manifests for a Basho Bench run shaped like `test/scale -soak`.

Mirrors the soak test of docs/soak-test-results.md: one TLS RiakCluster, 10 buckets (one bucket
type each), 10 certificate users and one load client per user, 600 ops/s in total, 50 % reads,
128 KiB objects, 16 workers per client, with the soak test's resource limits.

    python3 soak-like.py [--namespace bench] [--duration 28] > soak-like.yaml
    kubectl apply -f soak-like.yaml

The Issuer riak-ca-issuer must exist in the namespace (see docs/mtls.md). The load image defaults to
ghcr.io/marthydavid/basho-bench:openriak-3.2 (images/basho-bench); pass --bench-image to use one built
with examples/basho-bench/build.yaml.
"""
import argparse

p = argparse.ArgumentParser()
p.add_argument("--namespace", default="bench")
p.add_argument("--name", default="bsoak", help="cluster name")
p.add_argument("--image", default="ghcr.io/marthydavid/riak:3.2.6")
p.add_argument("--bench-image", default="ghcr.io/marthydavid/basho-bench:openriak-3.2")
p.add_argument("--storage-class", default="lvms-vg1")
p.add_argument("--storage", default="300Gi")
p.add_argument("--memory", default="16Gi")
p.add_argument("--cpu", default="4")
p.add_argument("--replicas", type=int, default=3)
p.add_argument("--clients", type=int, default=10, help="users, each with one client and one bucket")
p.add_argument("--rate", type=float, default=600, help="operations per second, all clients together")
p.add_argument("--workers", type=int, default=16, help="workers (connections) per client")
p.add_argument("--value-size", type=int, default=131072)
p.add_argument("--keyspace", type=int, default=3000, help="keys per client (the soak test uses 300 per bucket x 10 buckets)")
p.add_argument("--duration", type=int, default=28, help="minutes of load")
p.add_argument("--client-cpu", default="250m")
a = p.parse_args()

ns, name = a.namespace, a.name
per_client = a.rate / a.clients
per_worker = per_client / a.workers  # basho_bench's {rate, N} is per worker


def btype(i): return f"{name}-t{i:02d}"
def bname(i): return f"{name}-b{i:02d}"
def user(i): return f"{name}_u{i:02d}"
def ucr(i): return f"{name}-u{i:02d}"


out = []
out.append(f"""apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: {{name: {name}, namespace: {ns}}}
spec:
  size: {a.replicas}
  image: {a.image}
  storageClassName: {a.storage_class}
  storageSize: {a.storage}
  riakConfig: {{ring_size: "128"}}
  resources:
    requests: {{cpu: "{a.cpu}", memory: {a.memory}}}
    limits: {{memory: {a.memory}}}
  monitoring: {{enabled: true}}
  tls:
    enabled: true
    certManager: {{issuerName: riak-ca-issuer, issuerKind: Issuer}}""")

for i in range(a.clients):
    out.append(f"""apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata: {{name: {bname(i)}, namespace: {ns}}}
spec:
  clusterName: {name}
  bucketName: {bname(i)}
  bucketType: {btype(i)}
  nVal: 3
  properties: {{pr: "2", pw: "2"}}""")
    out.append(f"""apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata: {{name: {ucr(i)}, namespace: {ns}}}
spec:
  clusterName: {name}
  username: {user(i)}
  certificateRef:
    issuerRef: {{name: riak-ca-issuer, kind: Issuer}}
  grants:
  - {{resource: bucket, bucketName: {btype(i)}, permission: admin}}""")

# One ConfigMap holds every client's config; each Job mounts it and picks its own file.
cfg = []
for i in range(a.clients):
    cfg.append(f"""  client-{i:02d}.config: |
    {{mode, {{rate, {per_worker:.6g}}}}}.
    {{duration, {a.duration}}}.
    {{concurrent, {a.workers}}}.
    {{driver, basho_bench_driver_riakc_pb}}.
    {{key_generator, {{int_to_bin_bigendian, {{uniform_int, {a.keyspace}}}}}}}.
    {{value_generator, {{fixed_bin, {a.value_size}}}}}.
    {{riakc_pb_ips, [{{"{name}.{ns}.svc.cluster.local", 8087}}]}}.
    {{riakc_pb_bucket, {{<<"{btype(i)}">>, <<"{bname(i)}">>}}}}.
    {{riakc_pb_replies, quorum}}.
    {{pb_connect_options, [
        {{credentials, "{user(i)}", ""}},
        {{cacertfile, "/certs/ca.crt"}},
        {{certfile, "/certs/tls.crt"}},
        {{keyfile, "/certs/tls.key"}}]}}.
    {{operations, [{{get, 1}}, {{put, 1}}]}}.""")
out.append(f"""apiVersion: v1
kind: ConfigMap
metadata: {{name: {name}-bench-config, namespace: {ns}}}
data:
""" + "\n".join(cfg))

REPORT = r"""cd /tmp
START=$(date -u +%s)
/usr/local/bin/basho_bench /config/client-@I@.config > run.log 2>&1
T=tests/current
echo "@@START $START"
tail -3 run.log
for f in summary get_latencies put_latencies errors; do echo "@@CSV $f"; cat $T/$f.csv; done
echo "@@END"
"""
for i in range(a.clients):
    script = REPORT.replace("@I@", f"{i:02d}")
    script = "\n".join("          " + l for l in script.splitlines())
    out.append(f"""apiVersion: batch/v1
kind: Job
metadata: {{name: {name}-client-{i:02d}, namespace: {ns}}}
spec:
  backoffLimit: 0
  template:
    metadata: {{labels: {{app: {name}-client}}}}
    spec:
      restartPolicy: Never
      containers:
      - name: bench
        image: {a.bench_image}
        command: [/bin/bash, -c]
        args:
        - |
{script}
        resources:
          requests: {{cpu: {a.client_cpu}, memory: 256Mi}}
          limits: {{memory: 1Gi}}
        volumeMounts:
        - {{name: certs, mountPath: /certs, readOnly: true}}
        - {{name: config, mountPath: /config}}
      volumes:
      - name: certs
        secret: {{secretName: {ucr(i)}-client-tls}}
      - name: config
        configMap: {{name: {name}-bench-config}}""")

print("\n---\n".join(out))
