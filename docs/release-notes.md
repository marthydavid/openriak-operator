# Release notes

## Operator 0.0.9 / chart 0.1.7

Fixes Prometheus scraping: with `spec.monitoring.enabled`, Prometheus showed **0 targets** even
though the exporter sidecar was running.

### Fixes

- **The ServiceMonitor now selects a Service.** It matches `app=riak,cluster=<name>`, but the
  operator created its Services without labels, so nothing matched. The client Service now carries
  those labels. The headless Service stays unlabelled so each pod is scraped only once
  ([#42](https://github.com/marthydavid/openriak-operator/issues/42)). Existing clusters pick the
  labels up on the next reconcile after the operator is upgraded; no CRD change is needed.

### Changes you may notice

- The [scale test](scaling.md#verifying-riak-metrics) gained `-monitoring`
  (`make scale-test MONITORING=true`): it enables monitoring on every cluster and verifies the
  `riak_*` metrics on every node, and that the ServiceMonitor selects a Service.
- Releases are now described in a `release` skill (`.claude/skills/release`).

## Operator 0.0.8 / chart 0.1.6

Multi-node clusters now actually form, and what Riak holds is kept equal to what the custom
resources declare. This release comes out of the [scale test](scale-test-results.md), which now
verifies Riak itself on every node instead of trusting `status`.

### Fixes

- **Multi-node clusters form a ring.** Previously `size: 3` produced three pods that each formed a
  one-member ring while the cluster reported `Ready 3/3`. The operator now joins the nodes and
  only reports `Ready` once every node is a valid ring member (condition reason `FormingCluster`
  until then). See [Multi-node clusters](crds/riakcluster.md#multi-node-clusters).
- **Riak pods no longer crash-loop.** The Riak entrypoint waited for `riak ping`, which never
  succeeds in the image, and exited with code 1 after about 2m17s. The probes only checked TCP,
  so pods looked healthy until then.
- **Failed `riak-admin` commands are no longer treated as success.** `riak-admin` exits 0 even
  when a command fails and reports the error in its reply. The operator now reads the reply.
  Editing a RiakBucket (for example `n_val` or `allow_mult`) now reaches Riak: existing bucket
  types are changed with `bucket-type update`.
- **Grants follow the spec.** Grants removed from a RiakUser are revoked in Riak (an empty list
  revokes everything).
- **Deleting a RiakUser deletes the Riak user** (best effort, and abandoned after two minutes
  if the cluster cannot be reached, so it never blocks deletion).

### Changes you may notice

- The Riak image's default `ring_size` is now **128** (was 64). It only applies to clusters you
  create from now on; `ring_size` cannot be changed on an existing cluster.
- Riak nodes are named by their pod FQDN. If you run `riak-admin` by hand, set `VMARGS_PATH` as
  shown under [Multi-node clusters](crds/riakcluster.md#multi-node-clusters).
- Pods of a multi-node cluster need the rebuilt `ghcr.io/marthydavid/riak:3.2.6` image; roll your
  clusters after pulling it.

### Images

| Image | Tag |
|---|---|
| Operator | `ghcr.io/marthydavid/openriak-operator:0.0.8` |
| Riak KV 3.2 | `ghcr.io/marthydavid/riak:3.2.6` (rebuilt) |
| Helm chart | `oci://ghcr.io/marthydavid/charts/openriak-operator` `0.1.6` |

### Test tooling

- `test/scale` verifies Riak against the CRs on every node: ring membership and balance, bucket
  types and properties, users and certificate sources, and exact grants. It also supports
  `-verify-only`, `-mutate` and `-delete-users-every`, and requires `-ring-size` of at least 128.
