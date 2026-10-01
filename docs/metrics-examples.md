# Metrics examples

The repository ships ready-to-use monitoring objects in
[`examples/metrics/`](https://github.com/marthydavid/openriak-operator/tree/main/examples/metrics):

- `podmonitor.yaml`: a PodMonitor for the metrics sidecar (selects pods by `app=riak` and
  `cluster=<name>`, scrapes the `metrics` port at `/probe`).
- `prometheusrule.yaml`: example alerts (exporter down, ring size mismatch, GET/PUT p99 latency,
  read repairs, Protocol Buffers connections).
- `grafana-dashboard.json`: a Grafana dashboard over the `riak_*` series.

They assume `spec.monitoring.enabled: true` and the default exporter mapping described in
[Operator configuration](operator-configuration.md#prometheus-metrics-specmonitoring). See the
[README](https://github.com/marthydavid/openriak-operator/blob/main/examples/metrics/README.md)
for how to apply them and which operator fields they depend on.

```bash
kubectl apply -f examples/metrics/podmonitor.yaml -f examples/metrics/prometheusrule.yaml
```
