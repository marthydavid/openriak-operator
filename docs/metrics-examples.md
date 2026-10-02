# Metrics examples

The repository ships ready-to-use monitoring objects in
[`examples/metrics/`](https://github.com/marthydavid/openriak-operator/tree/main/examples/metrics):

- `podmonitor.yaml`: a PodMonitor for the metrics sidecar (selects pods by `app=riak` and
  `cluster=<name>`, scrapes the `metrics` port at `/probe`).
- `prometheusrule.yaml`: example alerts (exporter down, ring size mismatch, GET/PUT p99 latency,
  read repairs, Protocol Buffers connections).
- `grafana-dashboard.json`: a Grafana dashboard over the `riak_*` series, with namespace, cluster and pod selectors.

They assume `spec.monitoring.enabled: true` and the default exporter mapping described in
[Operator configuration](operator-configuration.md#prometheus-metrics-specmonitoring). See the
[README](https://github.com/marthydavid/openriak-operator/blob/main/examples/metrics/README.md)
for how to apply them and which operator fields they depend on.

```bash
kubectl apply -f examples/metrics/podmonitor.yaml -f examples/metrics/prometheusrule.yaml
```

## Installing the dashboard with the Helm chart

The operator chart can ship the dashboard as a ConfigMap for Grafana's dashboard sidecar
(kube-prometheus-stack, the Grafana chart, ...):

```bash
helm upgrade --install openriak-operator charts/openriak-operator \
  --set dashboard.enabled=true \
  --set dashboard.namespace=monitoring \
  --set dashboard.annotations.grafana_folder=OpenRiak
```

`dashboard.namespace` defaults to the release namespace; set it to Grafana's namespace if the
sidecar only watches that one. `dashboard.labels` defaults to `grafana_dashboard: "1"`, the label
the sidecar selects on. The dashboard picks its Prometheus data source with a `DS_PROMETHEUS`
variable and filters every panel by the `namespace`, `cluster` and `pod` selectors.
