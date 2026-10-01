/*
Copyright 2026 OpenRiak Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const (
	metricsNamespace = "openriak"
	// collectTimeout bounds the cache reads done on one scrape.
	collectTimeout = 10 * time.Second
	// unsetPhase labels resources whose controller has not written a phase yet.
	unsetPhase = "Pending"
)

var (
	clusterPhases = []riakv1.ClusterPhase{
		riakv1.PhaseCreating, riakv1.PhaseReady, riakv1.PhaseUpdating, riakv1.PhaseFailed,
	}

	descClusterPhase = prometheus.NewDesc(metricsNamespace+"_riakcluster_phase",
		"1 for the current phase of a RiakCluster, 0 for the other phases.",
		[]string{"namespace", "name", "phase"}, nil)
	descClusterNodes = prometheus.NewDesc(metricsNamespace+"_riakcluster_nodes",
		"Number of RiakCluster nodes by state (ready, total).",
		[]string{"namespace", "name", "state"}, nil)
	descClusterMonitoringReady = prometheus.NewDesc(metricsNamespace+"_riakcluster_monitoring_ready",
		"1 when spec.monitoring is enabled and every node's exporter sidecar is ready.",
		[]string{"namespace", "name"}, nil)
	descClusterTLSReady = prometheus.NewDesc(metricsNamespace+"_riakcluster_tls_ready",
		"1 when TLS is enabled and client TLS is ready.",
		[]string{"namespace", "name"}, nil)
	descBuckets = prometheus.NewDesc(metricsNamespace+"_riakbuckets",
		"Number of RiakBuckets by namespace, target cluster and phase.",
		[]string{"namespace", "cluster", "phase"}, nil)
	descUsers = prometheus.NewDesc(metricsNamespace+"_riakusers",
		"Number of RiakUsers by namespace, target cluster and phase.",
		[]string{"namespace", "cluster", "phase"}, nil)
	descUserCerts = prometheus.NewDesc(metricsNamespace+"_riakuser_certificates",
		"Number of RiakUsers by namespace, target cluster and whether the client certificate is issued.",
		[]string{"namespace", "cluster", "ready"}, nil)
	descResources = prometheus.NewDesc(metricsNamespace+"_resources",
		"Number of custom resources by kind and phase, across all namespaces.",
		[]string{"kind", "phase"}, nil)
)

// CRStateCollector exports the state of the RiakCluster/RiakBucket/RiakUser objects as
// Prometheus gauges. It reads from the (cached) client on every scrape instead of keeping
// per-reconcile bookkeeping, so deleted objects never leave stale series behind.
//
// Only RiakClusters get per-object series. Buckets and users are aggregated by
// (namespace, cluster, phase) so cardinality stays flat at fleet scale.
type CRStateCollector struct {
	client client.Reader
	log    logr.Logger
}

// NewCRStateCollector returns a collector reading from c.
func NewCRStateCollector(c client.Reader, log logr.Logger) *CRStateCollector {
	return &CRStateCollector{client: c, log: log}
}

// RegisterCRStateMetrics registers the collector with controller-runtime's metrics
// registry, which the manager serves on --metrics-bind-address.
func RegisterCRStateMetrics(c client.Reader, log logr.Logger) error {
	return ctrlmetrics.Registry.Register(NewCRStateCollector(c, log))
}

// Describe implements prometheus.Collector.
func (c *CRStateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descClusterPhase, descClusterNodes, descClusterMonitoringReady, descClusterTLSReady,
		descBuckets, descUsers, descUserCerts, descResources,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *CRStateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	// A failed list skips only that kind; the others are still reported.
	var clusters riakv1.RiakClusterList
	if err := c.client.List(ctx, &clusters); err != nil {
		c.log.Error(err, "listing RiakClusters for metrics")
	} else {
		collectClusters(ch, clusters.Items)
	}
	var buckets riakv1.RiakBucketList
	if err := c.client.List(ctx, &buckets); err != nil {
		c.log.Error(err, "listing RiakBuckets for metrics")
	} else {
		collectBuckets(ch, buckets.Items)
	}
	var users riakv1.RiakUserList
	if err := c.client.List(ctx, &users); err != nil {
		c.log.Error(err, "listing RiakUsers for metrics")
	} else {
		collectUsers(ch, users.Items)
	}
}

func gauge(d *prometheus.Desc, v float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func phaseOrPending(p string) string {
	if p == "" {
		return unsetPhase
	}
	return p
}

func collectClusters(ch chan<- prometheus.Metric, items []riakv1.RiakCluster) {
	byPhase := map[string]float64{}
	for i := range items {
		cl := &items[i]
		ns, name := cl.Namespace, cl.Name
		for _, p := range clusterPhases {
			ch <- gauge(descClusterPhase, boolValue(cl.Status.Phase == p), ns, name, string(p))
		}
		ch <- gauge(descClusterNodes, float64(cl.Status.ReadyNodes), ns, name, "ready")
		ch <- gauge(descClusterNodes, float64(cl.Status.TotalNodes), ns, name, "total")
		ch <- gauge(descClusterMonitoringReady,
			boolValue(cl.Status.MonitoringStatus.Enabled && cl.Status.MonitoringStatus.ExporterReady), ns, name)
		ch <- gauge(descClusterTLSReady,
			boolValue(cl.Status.TLSStatus.Enabled && cl.Status.TLSStatus.ClientReady), ns, name)
		byPhase[phaseOrPending(string(cl.Status.Phase))]++
	}
	known := make([]string, 0, len(clusterPhases))
	for _, p := range clusterPhases {
		known = append(known, string(p))
	}
	emitKindTotals(ch, "RiakCluster", byPhase, known...)
}

// groupKey identifies one aggregated bucket/user series.
type groupKey struct{ namespace, cluster, value string }

func emitGroups(ch chan<- prometheus.Metric, d *prometheus.Desc, groups map[groupKey]float64) {
	for k, n := range groups {
		ch <- gauge(d, n, k.namespace, k.cluster, k.value)
	}
}

func collectBuckets(ch chan<- prometheus.Metric, items []riakv1.RiakBucket) {
	groups := map[groupKey]float64{}
	byPhase := map[string]float64{}
	for i := range items {
		b := &items[i]
		phase := phaseOrPending(string(b.Status.Phase))
		groups[groupKey{b.Namespace, b.Spec.ClusterName, phase}]++
		byPhase[phase]++
	}
	emitGroups(ch, descBuckets, groups)
	emitKindTotals(ch, "RiakBucket", byPhase,
		string(riakv1.BucketPhaseCreating), string(riakv1.BucketPhaseReady), string(riakv1.BucketPhaseFailed))
}

func collectUsers(ch chan<- prometheus.Metric, items []riakv1.RiakUser) {
	groups := map[groupKey]float64{}
	certs := map[groupKey]float64{}
	byPhase := map[string]float64{}
	for i := range items {
		u := &items[i]
		phase := phaseOrPending(string(u.Status.Phase))
		groups[groupKey{u.Namespace, u.Spec.ClusterName, phase}]++
		ready := "false"
		if u.Status.CertificateReady {
			ready = "true"
		}
		certs[groupKey{u.Namespace, u.Spec.ClusterName, ready}]++
		byPhase[phase]++
	}
	emitGroups(ch, descUsers, groups)
	emitGroups(ch, descUserCerts, certs)
	emitKindTotals(ch, "RiakUser", byPhase,
		string(riakv1.UserPhaseCreating), string(riakv1.UserPhaseReady), string(riakv1.UserPhaseFailed))
}

// emitKindTotals emits openriak_resources for every known phase (so zero is visible) plus
// the unset phase when some object has no status yet.
func emitKindTotals(ch chan<- prometheus.Metric, kind string, byPhase map[string]float64, known ...string) {
	for _, p := range known {
		ch <- gauge(descResources, byPhase[p], kind, p)
	}
	if n := byPhase[unsetPhase]; n > 0 {
		ch <- gauge(descResources, n, kind, unsetPhase)
	}
}
