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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const (
	// riakMetricsPortName / riakMetricsPort: json_exporter's listen port,
	// exposed on the pod and the client Service for scraping.
	riakMetricsPortName = "metrics"
	riakMetricsPort     = int32(7979)

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

// reconcileMonitoringConfigMap creates or updates the json_exporter ConfigMap.
func (r *RiakClusterReconciler) reconcileMonitoringConfigMap(ctx context.Context, cluster *riakv1.RiakCluster) error {
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
		Name:  "metrics-exporter",
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
		Resources: corev1.ResourceRequirements{},
	}
}

// exporterConfigVolume returns the ConfigMap volume for the sidecar.
func exporterConfigVolume(cluster *riakv1.RiakCluster) corev1.Volume {
	return corev1.Volume{
		Name: "metrics-exporter-config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: exporterConfigMapName(cluster.Name),
				},
			},
		},
	}
}

// reconcileServiceMonitor creates the Prometheus Operator ServiceMonitor
// scraping the exporter through the client Service. Clusters without the
// Prometheus Operator CRDs are supported: a missing ServiceMonitor kind is
// logged and skipped, not treated as an error.
func (r *RiakClusterReconciler) reconcileServiceMonitor(ctx context.Context, cluster *riakv1.RiakCluster) error {
	sm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "monitoring.coreos.com/v1",
			"kind":       "ServiceMonitor",
			"metadata": map[string]interface{}{
				"name":      cluster.Name + "-metrics",
				"namespace": cluster.Namespace,
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app":     "riak",
						"cluster": cluster.Name,
					},
				},
				"endpoints": []interface{}{
					map[string]interface{}{
						"port": riakMetricsPortName,
						"path": "/probe",
						"params": map[string]interface{}{
							"module": []interface{}{"riak"},
							"target": []interface{}{riakStatsURL},
						},
						"interval": "30s",
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(cluster, sm, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference on ServiceMonitor: %w", err)
	}

	// Create-only, like the cluster/user Certificates: attempt the create and
	// tolerate the two benign outcomes. AlreadyExists keeps reconciles
	// idempotent; NoMatchError means the Prometheus Operator CRDs are not
	// installed, so scraping is simply skipped rather than failing the cluster.
	err := r.Create(ctx, sm)
	switch {
	case err == nil, apierrors.IsAlreadyExists(err):
		return nil
	case meta.IsNoMatchError(err):
		log.FromContext(ctx).Info("Prometheus Operator CRDs not installed; skipping ServiceMonitor",
			"cluster", cluster.Name)
		return nil
	default:
		return err
	}
}

// monitoringSidecars returns the metrics exporter sidecar(s) for the pod, or an
// empty slice when monitoring is disabled.
func monitoringSidecars(cluster *riakv1.RiakCluster) []corev1.Container {
	if !monitoringEnabled(cluster) {
		return nil
	}
	return []corev1.Container{exporterContainer(cluster)}
}
