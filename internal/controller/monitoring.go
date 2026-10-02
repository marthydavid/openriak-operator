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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const (
	// riakMetricsPortName / riakMetricsPort: json_exporter's listen port,
	// exposed on the pod and the client Service for scraping.
	riakMetricsPortName = "metrics"
	riakMetricsPort     = int32(7979)

	// exporterContainerName is the metrics sidecar's container name, also used to
	// read its readiness back out of the pod status.
	exporterContainerName = "metrics-exporter"

	// defaultExporterImage translates Riak's JSON /stats into Prometheus
	// metrics. Multi-arch (amd64+arm64) upstream image.
	defaultExporterImage = "quay.io/prometheuscommunity/json-exporter:v0.6.0"

	// riakStatsURL is scraped by the sidecar over the pod-local loopback.
	// Verified against the operand: /stats keeps answering plain HTTP 200 on
	// loopback even when Riak security is enabled.
	riakStatsURL = "http://127.0.0.1:8098/stats"
)

// monitoringEnabled reports whether the metrics sidecar should be injected.
func monitoringEnabled(cluster *riakv1.RiakCluster) bool {
	return cluster.Spec.Monitoring != nil && cluster.Spec.Monitoring.Enabled
}

// exporterConfigMapName returns the name of the json_exporter config ConfigMap.
func exporterConfigMapName(clusterName string) string {
	return clusterName + "-metrics-exporter"
}

// exporterConfig is the json_exporter module mapping the useful numeric fields
// of Riak's /stats to stable riak_* metric names. Riak's stats document is
// flat, so every entry is a simple JSONPath.
const exporterConfig = `modules:
  riak:
    metrics:
      - name: riak_node_gets
        path: "{.node_gets}"
        help: GETs coordinated by this node in the last minute
      - name: riak_node_gets_total
        path: "{.node_gets_total}"
        help: Total GETs coordinated by this node
      - name: riak_node_puts
        path: "{.node_puts}"
        help: PUTs coordinated by this node in the last minute
      - name: riak_node_puts_total
        path: "{.node_puts_total}"
        help: Total PUTs coordinated by this node
      - name: riak_node_get_fsm_time_mean
        path: "{.node_get_fsm_time_mean}"
        help: Mean GET latency (microseconds) over the last minute
      - name: riak_node_get_fsm_time_median
        path: "{.node_get_fsm_time_median}"
        help: Median GET latency (microseconds) over the last minute
      - name: riak_node_get_fsm_time_95
        path: "{.node_get_fsm_time_95}"
        help: 95th percentile GET latency (microseconds) over the last minute
      - name: riak_node_get_fsm_time_99
        path: "{.node_get_fsm_time_99}"
        help: 99th percentile GET latency (microseconds) over the last minute
      - name: riak_node_get_fsm_time_100
        path: "{.node_get_fsm_time_100}"
        help: Maximum GET latency (microseconds) over the last minute
      - name: riak_node_put_fsm_time_mean
        path: "{.node_put_fsm_time_mean}"
        help: Mean PUT latency (microseconds) over the last minute
      - name: riak_node_put_fsm_time_median
        path: "{.node_put_fsm_time_median}"
        help: Median PUT latency (microseconds) over the last minute
      - name: riak_node_put_fsm_time_95
        path: "{.node_put_fsm_time_95}"
        help: 95th percentile PUT latency (microseconds) over the last minute
      - name: riak_node_put_fsm_time_99
        path: "{.node_put_fsm_time_99}"
        help: 99th percentile PUT latency (microseconds) over the last minute
      - name: riak_node_put_fsm_time_100
        path: "{.node_put_fsm_time_100}"
        help: Maximum PUT latency (microseconds) over the last minute
      - name: riak_vnode_gets
        path: "{.vnode_gets}"
        help: vnode GET operations in the last minute
      - name: riak_vnode_gets_total
        path: "{.vnode_gets_total}"
        help: Total vnode GET operations
      - name: riak_vnode_puts
        path: "{.vnode_puts}"
        help: vnode PUT operations in the last minute
      - name: riak_vnode_puts_total
        path: "{.vnode_puts_total}"
        help: Total vnode PUT operations
      - name: riak_read_repairs
        path: "{.read_repairs}"
        help: Read repairs in the last minute
      - name: riak_read_repairs_total
        path: "{.read_repairs_total}"
        help: Total read repairs
      - name: riak_coord_redirs_total
        path: "{.coord_redirs_total}"
        help: Total coordinator redirects to other nodes
      - name: riak_pbc_active
        path: "{.pbc_active}"
        help: Active protocol buffers connections
      - name: riak_pbc_connects_total
        path: "{.pbc_connects_total}"
        help: Total protocol buffers connections
      - name: riak_node_get_fsm_objsize_99
        path: "{.node_get_fsm_objsize_99}"
        help: 99th percentile object size (bytes) fetched in the last minute
      - name: riak_memory_processes
        path: "{.memory_processes}"
        help: Memory (bytes) used by Erlang processes
      - name: riak_memory_system
        path: "{.memory_system}"
        help: Memory (bytes) allocated by the Erlang VM
      - name: riak_sys_process_count
        path: "{.sys_process_count}"
        help: Number of Erlang processes
      - name: riak_ring_num_partitions
        path: "{.ring_num_partitions}"
        help: Ring size
`

// customMetricsConfig returns the user-supplied exporter config reference, or
// nil when the built-in mapping is used.
func customMetricsConfig(cluster *riakv1.RiakCluster) *corev1.ConfigMapKeySelector {
	if cluster.Spec.Monitoring == nil || cluster.Spec.Monitoring.MetricsConfig == nil {
		return nil
	}
	return &cluster.Spec.Monitoring.MetricsConfig.ConfigMapKeyRef
}

// reconcileMonitoringConfigMap prepares the exporter's config. With the built-in
// mapping it creates or updates the operator's ConfigMap. With
// spec.monitoring.metricsConfig it only checks that the referenced ConfigMap key
// exists: a missing one would leave the Riak pods stuck on a volume mount, so it
// fails the reconcile (before the StatefulSet changes) with a clear message.
func (r *RiakClusterReconciler) reconcileMonitoringConfigMap(ctx context.Context, cluster *riakv1.RiakCluster) error {
	if ref := customMetricsConfig(cluster); ref != nil {
		cm := &corev1.ConfigMap{}
		key := client.ObjectKey{Name: ref.Name, Namespace: cluster.Namespace}
		if err := r.Get(ctx, key, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("spec.monitoring.metricsConfig: ConfigMap %q not found", ref.Name)
			}
			return err
		}
		_, inData := cm.Data[ref.Key]
		_, inBinary := cm.BinaryData[ref.Key]
		if !inData && !inBinary {
			return fmt.Errorf("spec.monitoring.metricsConfig: ConfigMap %q has no key %q", ref.Name, ref.Key)
		}
		// Drop the generated mapping left behind by an earlier built-in config.
		stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: exporterConfigMapName(cluster.Name), Namespace: cluster.Namespace}}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      exporterConfigMapName(cluster.Name),
			Namespace: cluster.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"config.yml": exporterConfig}
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	})
	return err
}

// exporterContainer builds the metrics sidecar for the Riak pod.
func exporterContainer(cluster *riakv1.RiakCluster) corev1.Container {
	image := defaultExporterImage
	if cluster.Spec.Monitoring.ExporterImage != "" {
		image = cluster.Spec.Monitoring.ExporterImage
	}
	return corev1.Container{
		Name:  exporterContainerName,
		Image: image,
		Args:  []string{"--config.file=/config/config.yml"},
		Ports: []corev1.ContainerPort{
			{Name: riakMetricsPortName, ContainerPort: riakMetricsPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "metrics-exporter-config", MountPath: "/config", ReadOnly: true},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(riakMetricsPort)},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		// json_exporter is lightweight; give it modest requests so it schedules
		// predictably and caps so a wedged exporter can't starve the Riak node.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
}

// exporterConfigVolume returns the ConfigMap volume for the sidecar: the
// operator's generated mapping, or the user's key projected as config.yml.
func exporterConfigVolume(cluster *riakv1.RiakCluster) corev1.Volume {
	src := &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: exporterConfigMapName(cluster.Name)},
	}
	if ref := customMetricsConfig(cluster); ref != nil {
		src.LocalObjectReference = corev1.LocalObjectReference{Name: ref.Name}
		src.Items = []corev1.KeyToPath{{Key: ref.Key, Path: "config.yml"}}
	}
	return corev1.Volume{
		Name:         "metrics-exporter-config",
		VolumeSource: corev1.VolumeSource{ConfigMap: src},
	}
}

// Prometheus Operator groups and kinds the operator may manage.
var (
	serviceMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	podMonitorGVK     = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PodMonitor"}
)

// scrapeKind returns the effective spec.monitoring.scrapeKind (default PodMonitor).
func scrapeKind(cluster *riakv1.RiakCluster) string {
	if cluster.Spec.Monitoring == nil || cluster.Spec.Monitoring.ScrapeKind == "" {
		return riakv1.ScrapeKindPodMonitor
	}
	return cluster.Spec.Monitoring.ScrapeKind
}

// scrapeObjectName is the name shared by the PodMonitor and ServiceMonitor.
func scrapeObjectName(cluster *riakv1.RiakCluster) string { return cluster.Name + "-metrics" }

// exporterEndpoint is the scrape endpoint, identical for both monitor kinds:
// the exporter's /probe path, pointed at the pod-local Riak /stats.
func exporterEndpoint() map[string]interface{} {
	return map[string]interface{}{
		"port": riakMetricsPortName,
		"path": "/probe",
		"params": map[string]interface{}{
			"module": []interface{}{"riak"},
			"target": []interface{}{riakStatsURL},
		},
		"interval": "30s",
	}
}

// reconcileScrapeObject makes the cluster's Prometheus Operator scrape object
// match spec.monitoring.scrapeKind: it creates the selected kind and removes the
// other one (so switching kinds, or None, leaves nothing behind that would
// double-scrape).
func (r *RiakClusterReconciler) reconcileScrapeObject(ctx context.Context, cluster *riakv1.RiakCluster) error {
	var err error
	switch scrapeKind(cluster) {
	case riakv1.ScrapeKindServiceMonitor:
		if err = r.reconcileServiceMonitor(ctx, cluster); err == nil {
			err = r.deleteMonitor(ctx, cluster, podMonitorGVK)
		}
	case riakv1.ScrapeKindNone:
		if err = r.deleteMonitor(ctx, cluster, serviceMonitorGVK); err == nil {
			err = r.deleteMonitor(ctx, cluster, podMonitorGVK)
		}
	default:
		if err = r.reconcilePodMonitor(ctx, cluster); err == nil {
			err = r.deleteMonitor(ctx, cluster, serviceMonitorGVK)
		}
	}
	return err
}

// reconcileServiceMonitor creates or updates the Prometheus Operator
// ServiceMonitor scraping the exporter through the client Service (which carries
// the app/cluster labels the selector matches).
func (r *RiakClusterReconciler) reconcileServiceMonitor(ctx context.Context, cluster *riakv1.RiakCluster) error {
	return r.reconcileMonitor(ctx, cluster, serviceMonitorGVK, "endpoints")
}

// reconcilePodMonitor creates or updates the Prometheus Operator PodMonitor
// selecting the Riak pods directly by their app/cluster labels. Unlike a
// ServiceMonitor it does not depend on Service labels or endpoints.
func (r *RiakClusterReconciler) reconcilePodMonitor(ctx context.Context, cluster *riakv1.RiakCluster) error {
	return r.reconcileMonitor(ctx, cluster, podMonitorGVK, "podMetricsEndpoints")
}

// reconcileMonitor creates or updates a monitor of the given kind. Clusters
// without the Prometheus Operator CRDs are supported: a missing kind is logged
// and skipped, not treated as an error. CreateOrUpdate so spec changes propagate
// to existing objects; the mutate sets the full desired spec and owner reference.
func (r *RiakClusterReconciler) reconcileMonitor(
	ctx context.Context, cluster *riakv1.RiakCluster, gvk schema.GroupVersionKind, endpointsField string,
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(scrapeObjectName(cluster))
	obj.SetNamespace(cluster.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		spec := map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app":     "riak",
					"cluster": cluster.Name,
				},
			},
			endpointsField: []interface{}{exporterEndpoint()},
		}
		// Copy the cluster label onto the series so dashboards and alerts can
		// group by cluster, whichever monitor kind is used.
		if gvk == podMonitorGVK {
			spec["podTargetLabels"] = []interface{}{"cluster"}
		} else {
			spec["targetLabels"] = []interface{}{"cluster"}
		}
		obj.Object["spec"] = spec
		return controllerutil.SetControllerReference(cluster, obj, r.Scheme)
	})

	// A missing Prometheus Operator CRD surfaces as a NoMatchError from the
	// CreateOrUpdate Get; treat it as "no scraping configured", not a failure.
	if meta.IsNoMatchError(err) {
		log.FromContext(ctx).Info("Prometheus Operator CRDs not installed; skipping "+gvk.Kind,
			"cluster", cluster.Name)
		return nil
	}
	return err
}

// deleteMonitor removes the cluster's monitor of the given kind if it exists.
// Absent objects and absent CRDs are fine.
func (r *RiakClusterReconciler) deleteMonitor(
	ctx context.Context, cluster *riakv1.RiakCluster, gvk schema.GroupVersionKind,
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(scrapeObjectName(cluster))
	obj.SetNamespace(cluster.Namespace)
	err := r.Delete(ctx, obj)
	if err == nil || apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

// monitoringSidecars returns the metrics exporter sidecar(s) for the pod, or an
// empty slice when monitoring is disabled.
func monitoringSidecars(cluster *riakv1.RiakCluster) []corev1.Container {
	if !monitoringEnabled(cluster) {
		return nil
	}
	return []corev1.Container{exporterContainer(cluster)}
}
