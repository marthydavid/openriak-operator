# Release notes

## Operator 0.0.12 / chart 0.1.10

Moves the operator to **Go 1.25, controller-runtime v0.23 and Kubernetes client libraries 0.35**, so it matches
OpenShift/OKD 4.22 (Kubernetes 1.35) exactly and stays within one minor version of OpenShift 4.21 (1.34) and
AKS (1.34 and later). It also fixes Riak 3.0 clusters never becoming Ready. No behaviour change for existing
clusters; a plain `helm upgrade` is enough.

### Fixes

- **Riak 3.0 clusters never left `Creating`.** The `3.0.16` operand image has no `riak-admin` script (in Riak
  3.0 the admin CLI is `riak admin ...`), so the entrypoint readiness check never succeeded and pods restarted
  every 120 seconds, and the operator's `riak-admin` calls could not run either. The image now ships a
  `riak-admin` wrapper where the RPM lacks one ([#64](https://github.com/marthydavid/openriak-operator/pull/64)).
  The existing `3.0.16` tag was rebuilt: pin it by digest or set `spec.imagePullPolicy: Always` to pick it up.

### Changes you may notice

- **Platform support is verified on the new stack**: the scale test (3-node cluster, users and buckets, verified
  against Riak on every node) passes on OKD 4.22 for the `3.0`, `3.2` and `3.4` operand images with this release.
- The CRDs were regenerated with controller-gen v0.20.1; the schema is unchanged apart from a reworded
  field description.
- Build and development: the module requires Go 1.25, golangci-lint moved to v2, envtest uses Kubernetes 1.35.
  New lifecycle tests cover scaling a cluster up and adding and deleting buckets and users
  ([#68](https://github.com/marthydavid/openriak-operator/pull/68)).
- The README roadmap no longer lists Riak search integration.

## Operator 0.0.11 / chart 0.1.9

Fixes an intermittent **Riak crash-loop on first start**, and stops the Riak pod anti-affinity from keeping
unrelated pods off your nodes. **Upgrading restarts every existing RiakCluster once** (see below).

!!! warning "Upgrading rolls every RiakCluster once"
    The Riak pod anti-affinity is part of the StatefulSet pod template, so changing it makes every
    existing cluster restart its pods **one at a time** (StatefulSet rolling update, highest ordinal first,
    so the seed node `<cluster>-0` goes last) when the operator is upgraded. Restarted pods keep their data
    volumes and rejoin the ring. On OpenShift (3-node clusters under constant client load) this took about
    80 seconds per cluster, the rings stayed complete, no container crashed and a final read-back of every
    written key found nothing lost or corrupt. Reads that arrive while a node is restarting can return a
    value that is briefly missing or stale: that is Riak's normal behavior under node loss with the default
    quorum settings. Plan the upgrade for a quiet period if a cluster is sensitive to a node restart
    ([#58](https://github.com/marthydavid/openriak-operator/issues/58)). No CRD changes: a plain
    `helm upgrade` is enough.

!!! note "Pick up the rebuilt Riak image"
    The entrypoint fix below ships in the Riak image, and the existing tags (`3.0.16`, `3.2.6`, `3.4.0`) were
    rebuilt with it. A pod only gets it when it pulls the image: with the default `imagePullPolicy:
    IfNotPresent` a node that already has `3.2.6` cached keeps using the old one. Pin the image by digest, or
    set `spec.imagePullPolicy: Always`, and let the pods restart.

### Fixes

- **A Riak node could name itself after its short host name and then crash-loop forever**
  ([#59](https://github.com/marthydavid/openriak-operator/issues/59)). The kubelet rewrites a pod's
  `/etc/hosts` each time it creates another container of the pod, and with `spec.monitoring` the
  `metrics-exporter` sidecar is created right after the `riak` container starts. The entrypoint read the
  file once, could see it empty, fell back to `riak@<pod>`, and the ring that node then persisted on its
  volume under that name made every later start (under the real name) crash with
  `riak_core_capability ... orddict:fetch`. The entrypoint now retries the lookup, falls back to the pod's name
  in the headless Service and the DNS search domain, and **refuses to start** rather than use a short name.
  Before starting it checks that the persisted ring lists this node: a one-member ring of another name is moved
  aside to `ring.stale-<time>` (the vnode data is kept and Riak starts a fresh ring under the right name), and a
  ring with several members is refused with recovery instructions. The logs of the last five failed starts are
  kept under `/var/lib/riak/crash-logs/` and printed on the next start, so the first failure is no longer
  overwritten. See [Troubleshooting](troubleshooting.md).
- **`riak-admin` answers "Node ... is not responding to pings" with exit status 0**, and the operator took
  that for success (for example an empty member list). It is now treated as a failure, so these problems
  surface instead of passing silently.
- **Riak pods no longer keep unrelated pods off their nodes**
  ([#58](https://github.com/marthydavid/openriak-operator/issues/58)). The required anti-affinity selected
  every pod labelled `cluster=<name>`; it now selects only Riak pods (`app=riak,cluster=<name>`). Clusters
  created by older operators keep the old selector until they roll, which this upgrade does.

### Changes you may notice

- When you run `riak-admin` by hand in a pod, set `VMARGS_PATH` to the newest generated `vm.args`; a bare
  `riak-admin` reports "not responding to pings" even on a healthy node. The README and the troubleshooting page
  show the command.

### Test tooling and docs

- `test/scale` has a `-stress` option and ships an example application (`examples/stressapp`, a stdlib-only
  Python client speaking the Riak protocol over mTLS). It checks the clients' results and Riak's own metrics
  against each other, and that nothing restarted. See [Stress-testing Riak](scaling.md#stress-testing-riak)
  and [Scale test results](scale-test-results.md) ([#60](https://github.com/marthydavid/openriak-operator/pull/60)).
- New [Test environment](test-environment.md) page describing the cluster the scale tests run on.

### Images

| Image | Tag |
|---|---|
| Operator | `ghcr.io/marthydavid/openriak-operator:0.0.11` |
| Helm chart | `oci://ghcr.io/marthydavid/charts/openriak-operator` `0.1.9` |
| Riak KV | `ghcr.io/marthydavid/riak:3.0.16`, `3.2.6`, `3.4.0` (rebuilt with the new entrypoint) |

## Operator 0.0.10 / chart 0.1.8

Fixes the operator being **OOMKilled** at its old default memory limit, and adds a choice of Prometheus
scrape object, metrics about the operator's own view of your fleet, and ready-made alerts and a Grafana
dashboard.

!!! warning "Upgrade the CRDs first"
    This release adds fields to the RiakCluster CRD (`spec.monitoring.scrapeKind`,
    `spec.monitoring.metricsConfig`, and new `status.monitoringStatus` fields). Helm does not upgrade
    CRDs shipped in a chart's `crds/` directory, so apply them before upgrading the operator:

    ```bash
    kubectl apply --server-side -f charts/openriak-operator/crds/
    ```

### Fixes

- **The operator is no longer OOMKilled at its default memory limit.** With the old 128Mi limit the
  operator crash-looped (`OOMKilled`) while resyncing a small fleet (3 clusters, 9 nodes, 60 users, 60
  buckets; it peaks around 180Mi right after a start), which stalled every cluster. The default is now a
  **512Mi limit and a 128Mi request**, in both the kustomize manifest and the Helm chart. If you set
  `resources` in your Helm values, check that the limit is not still 128Mi
  ([#48](https://github.com/marthydavid/openriak-operator/issues/48)).

### New

- **Choose what Prometheus scrapes: `spec.monitoring.scrapeKind`** (`PodMonitor` is the new default,
  `ServiceMonitor`, or `None` to bring your own). The operator creates the selected kind and removes the
  other, so a cluster is never scraped twice. `status.monitoringStatus` reports `scrapeKind` and
  `scrapeObjectReady`. See [Choosing the scrape object](operator-configuration.md#choosing-the-scrape-object)
  ([#52](https://github.com/marthydavid/openriak-operator/pull/52)).
- **Custom exporter rules: `spec.monitoring.metricsConfig.configMapKeyRef`** replaces the built-in
  metric mapping with your own `json_exporter` rules. A missing ConfigMap or key fails the reconcile with
  a clear error instead of leaving pods stuck on a volume mount.
- **Operator metrics on `/metrics`:** `openriak_riakcluster_phase`, `openriak_riakcluster_nodes{state}`,
  `openriak_riakcluster_monitoring_ready`, `openriak_riakcluster_tls_ready`, plus buckets and users
  aggregated by namespace, cluster and phase (`openriak_riakbuckets`, `openriak_riakusers`,
  `openriak_riakuser_certificates`), and a fleet-wide `openriak_resources{kind,phase}`. They are computed from the operator's cache on each scrape, so there
  are no stale series ([#51](https://github.com/marthydavid/openriak-operator/pull/51)).
- **Metrics examples and a Grafana dashboard:** a `PodMonitor`, a `PrometheusRule` with alerts and a
  dashboard in `examples/metrics/`, described in [Metrics examples](metrics-examples.md). The Helm chart
  can ship the dashboard as a ConfigMap for Grafana's sidecar with `dashboard.enabled: true`
  ([#50](https://github.com/marthydavid/openriak-operator/pull/50)).

### Changes you may notice

- **Monitored clusters move from a `ServiceMonitor` to a `PodMonitor`** on the first reconcile after the
  operator upgrade, because `PodMonitor` is the new default. To keep the `ServiceMonitor`, set
  `spec.monitoring.scrapeKind: ServiceMonitor`. `status.monitoringStatus.serviceMonitorReady` is
  deprecated; it is still set for the `ServiceMonitor` kind.
- The operator needs permission on `podmonitors.monitoring.coreos.com`. The chart and the kustomize
  manifests include it; if you manage RBAC yourself, add it.

### Images

| Image | Tag |
|---|---|
| Operator | `ghcr.io/marthydavid/openriak-operator:0.0.10` |
| Helm chart | `oci://ghcr.io/marthydavid/charts/openriak-operator` `0.1.8` |

The Riak image is unchanged (`ghcr.io/marthydavid/riak:3.2.6`).

### Test tooling

- `test/scale` now checks the Riak metrics after a real write on every cluster (counters on the node
  that took it, and replication across the vnodes), and **fails the run if the operator restarts or is
  OOMKilled**. Results and the cluster they ran on: [Scale test results](scale-test-results.md),
  [Test environment](test-environment.md).

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
