# Metrics examples

Ready-to-use monitoring objects for a `RiakCluster` with `spec.monitoring.enabled: true`.
They are examples you own and edit, not something the operator installs. Everything assumes a
cluster named `my-cluster` in namespace `default`; change those names to match yours.

| File | What it is |
|------|-----------|
| `podmonitor.yaml` | Prometheus Operator `PodMonitor` scraping the json_exporter sidecar |
| `prometheusrule.yaml` | `PrometheusRule` with example alerts |
| `grafana-dashboard.json` | Grafana dashboard over the `riak_*` series |

## podmonitor.yaml

Selects pods with `app=riak, cluster=my-cluster` (labels the operator sets) and scrapes the
sidecar's named container port `metrics` (7979) at `/probe` with `module=riak` and
`target=http://127.0.0.1:8098/stats` every 30s, the same endpoint the operator-generated
`ServiceMonitor` uses. `podTargetLabels: [cluster]` copies the `cluster` label onto every series,
which the alerts and dashboard rely on. Add your Prometheus `podMonitorSelector` label (for
example `release: kube-prometheus-stack`) to `metadata.labels`.

## prometheusrule.yaml

Alerts use only series in the operator's default exporter mapping: `RiakExporterDown`
(`up == 0`), `RiakRingSizeInconsistent`, `RiakGetLatencyHigh` / `RiakPutLatencyHigh` (p99 above
500 ms; the series are in microseconds), `RiakReadRepairRateHigh` and `RiakPBConnectionsHigh`.
Thresholds are starting points; each alert carries a `runbook` annotation with first commands.

## grafana-dashboard.json

Panels cover throughput, GET/PUT latency percentiles, vnode operations, read repairs,
Protocol Buffers connections, object size, ring size and Erlang memory/process count.
Import via Grafana > Dashboards > Import and pick your Prometheus data source (`DS_PROMETHEUS`).
The `cluster` and `pod` variables come from `label_values(riak_ring_num_partitions, ...)`.

## Apply

```bash
kubectl apply -f podmonitor.yaml -f prometheusrule.yaml
# dashboard: import grafana-dashboard.json in the Grafana UI, or load it into a
# ConfigMap labelled grafana_dashboard=1 if you run the Grafana sidecar provisioner
```

## Assumptions

- Prometheus Operator CRDs (`monitoring.coreos.com/v1`) are installed.
- `spec.monitoring.enabled: true`, which injects the `metrics-exporter` sidecar with container
  port `metrics` (7979). The metric names are the operator's default mapping. If you replace it
  with `monitoring.metricsConfig.configMapKeyRef` (operator versions that support it), keep the
  `riak_*` names used here or adjust the queries.
- Current operators create a `ServiceMonitor` themselves. Operator versions with
  `monitoring.scrapeKind` (`PodMonitor`, `ServiceMonitor`, `None`) can create the PodMonitor for
  you; set `None` when you apply `podmonitor.yaml` yourself, otherwise the pods are scraped twice.
- The `cluster` label comes from the PodMonitor's `podTargetLabels`. A `ServiceMonitor` does not
  add it unless you set `targetLabels`, in which case the `cluster` selectors need adjusting.
