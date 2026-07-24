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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RiakClusterSpec defines the desired state of RiakCluster.
type RiakClusterSpec struct {
	// Size is the number of nodes in the cluster.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	Size int32 `json:"size"`

	// Image is the Riak Docker image to use (e.g., ghcr.io/marthydavid/riak:3.2.6).
	// When omitted, the operator's --riak-image flag value is used as the default.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image,omitempty"`

	// ImagePullPolicy defines how the image should be pulled.
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// Resources specifies compute resources for each Riak node.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// StorageClassName for persistent volumes.
	StorageClassName string `json:"storageClassName,omitempty"`

	// StorageSize is the size of storage for each Riak node (e.g., 10Gi).
	StorageSize *resource.Quantity `json:"storageSize,omitempty"`

	// EphemeralStorage, when true, backs each Riak node's data directory with an
	// emptyDir volume instead of a PersistentVolumeClaim. Data does NOT survive
	// pod restarts. Intended for testing/CI on clusters without a dynamic storage
	// provisioner; not for production. When set, StorageClassName and StorageSize
	// are ignored for data-volume provisioning.
	// +optional
	EphemeralStorage bool `json:"ephemeralStorage,omitempty"`

	// RiakConfig sets arbitrary riak.conf keys on all nodes. Any key from the
	// Riak configuration reference works, e.g.:
	//   storage_backend: memory
	//   memory_backend.ttl: 60s
	//   memory_backend.max_memory_per_vnode: 128MB
	//   multi_backend.mem_ttl.storage_backend: memory
	// Changing values rolls the StatefulSet so nodes restart with the new
	// configuration. WARNING: changing storage_backend (or a bucket's backend
	// binding) on a cluster with data does NOT migrate that data — objects
	// written to the previous backend become unreachable. Plan a backup or
	// migration before switching backends on a live cluster.
	// Bind buckets to a multi_backend entry via
	// RiakBucket.spec.properties ("backend": "<name>").
	RiakConfig map[string]string `json:"riakConfig,omitempty"`

	// TLS configuration for inter-node communication.
	TLS *TLSConfig `json:"tls,omitempty"`

	// Monitoring configures Prometheus metrics for the Riak nodes.
	Monitoring *MonitoringConfig `json:"monitoring,omitempty"`

	// ServicePort is the port for the Riak protocol service.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	ServicePort int32 `json:"servicePort,omitempty"`

	// NodeSelector for node affinity.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// TLSConfig defines TLS settings for the cluster.
// MonitoringConfig enables a Prometheus metrics sidecar on every Riak pod.
// The sidecar translates Riak's JSON /stats endpoint into Prometheus metrics
// (served on port 7979) and, when the Prometheus Operator CRDs are installed,
// a ServiceMonitor is created to scrape it.
type MonitoringConfig struct {
	// Enabled turns on the metrics exporter sidecar and ServiceMonitor.
	Enabled bool `json:"enabled"`

	// ExporterImage overrides the json_exporter sidecar image.
	ExporterImage string `json:"exporterImage,omitempty"`
}

type TLSConfig struct {
	// Enabled specifies if TLS should be used.
	Enabled bool `json:"enabled,omitempty"`

	// CertManager enables cert-manager integration.
	CertManager *CertManagerConfig `json:"certManager,omitempty"`
}

// CertManagerConfig specifies cert-manager configuration.
type CertManagerConfig struct {
	// IssuerName is the cert-manager issuer to use.
	IssuerName string `json:"issuerName,omitempty"`

	// IssuerKind is the kind of issuer (Issuer or ClusterIssuer).
	IssuerKind string `json:"issuerKind,omitempty"`
}

// RiakClusterStatus defines the observed state of RiakCluster.
type RiakClusterStatus struct {
	// Phase is the current phase of the cluster (Creating, Ready, Updating, Failed).
	Phase ClusterPhase `json:"phase,omitempty"`

	// ReadyNodes is the number of ready nodes.
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// TotalNodes is the total number of nodes in the cluster.
	TotalNodes int32 `json:"totalNodes,omitempty"`

	// Conditions represent the latest observed conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastUpdateTime is the last time the cluster was updated.
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// Members is a list of current cluster members.
	Members []RiakNodeMember `json:"members,omitempty"`

	// SecurityEnabled records that Riak security has been enabled on the cluster.
	// It is enabled once (when the first cert-auth user is created), not per user:
	// repeatedly running `riak-admin security enable` on a live node bounces its
	// client listeners and destabilises it under load.
	// +optional
	SecurityEnabled bool `json:"securityEnabled,omitempty"`

	// StorageClassName is the storage class name currently in use.
	StorageClassName string `json:"storageClassName,omitempty"`

	// StorageSize is the storage size currently configured.
	StorageSize string `json:"storageSize,omitempty"`

	// EphemeralStorage indicates if ephemeral storage is in use.
	EphemeralStorage bool `json:"ephemeralStorage,omitempty"`

	// TLSStatus indicates the TLS configuration status.
	TLSStatus TLSStatus `json:"tlsStatus,omitempty"`

	// MonitoringStatus indicates the monitoring configuration status.
	MonitoringStatus MonitoringStatus `json:"monitoringStatus,omitempty"`

	// Buckets is a list of buckets created in this cluster.
	Buckets []RiakBucketRef `json:"buckets,omitempty"`

	// Users is a list of users created in this cluster.
	Users []RiakUserRef `json:"users,omitempty"`

	// NodeConditions contains detailed conditions for each node.
	NodeConditions []NodeCondition `json:"nodeConditions,omitempty"`
}

// TLSStatus indicates the TLS configuration status.
type TLSStatus struct {
	// Enabled indicates if TLS is enabled.
	Enabled bool `json:"enabled,omitempty"`

	// CertManagerReady indicates if cert-manager certificates are ready.
	CertManagerReady bool `json:"certManagerReady,omitempty"`

	// CertManagerError contains error details if certificate provisioning failed.
	CertManagerError string `json:"certManagerError,omitempty"`

	// InterNodeReady indicates if inter-node TLS is ready.
	InterNodeReady bool `json:"interNodeReady,omitempty"`

	// ClientReady indicates if client TLS is ready.
	ClientReady bool `json:"clientReady,omitempty"`
}

// MonitoringStatus indicates the monitoring configuration status.
type MonitoringStatus struct {
	// Enabled indicates if monitoring is enabled.
	Enabled bool `json:"enabled,omitempty"`

	// ExporterReady indicates if the metrics exporter sidecar is ready.
	ExporterReady bool `json:"exporterReady,omitempty"`

	// ServiceMonitorReady indicates if the ServiceMonitor is ready (when using Prometheus Operator).
	ServiceMonitorReady bool `json:"serviceMonitorReady,omitempty"`

	// ExporterError contains error details if the exporter failed to start.
	ExporterError string `json:"exporterError,omitempty"`
}

// NodeCondition represents the condition of a single node.
type NodeCondition struct {
	// Name is the node name.
	Name string `json:"name,omitempty"`

	// Ready indicates if the node is ready.
	Ready bool `json:"ready,omitempty"`

	// Health is the health status of the node.
	Health string `json:"health,omitempty"`

	// Phase is the lifecycle phase of the node.
	Phase string `json:"phase,omitempty"`

	// PodName is the Kubernetes Pod name.
	PodName string `json:"podName,omitempty"`

	// StorageReady indicates if the node's storage is ready.
	StorageReady bool `json:"storageReady,omitempty"`

	// StorageClassName is the storage class name in use.
	StorageClassName string `json:"storageClassName,omitempty"`

	// StorageSize is the storage size configured.
	StorageSize string `json:"storageSize,omitempty"`

	// Conditions contains detailed conditions for this node.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RiakNodeMember represents a cluster member node.
type RiakNodeMember struct {
	// Name is the member name.
	Name string `json:"name,omitempty"`

	// Pod is the Kubernetes Pod name.
	Pod string `json:"pod,omitempty"`

	// Ready indicates if the node is ready.
	Ready bool `json:"ready,omitempty"`

	// Health is the health status of the node.
	Health string `json:"health,omitempty"`

	// Phase is the lifecycle phase of the node.
	Phase string `json:"phase,omitempty"`

	// StorageReady indicates if the node's storage is ready.
	StorageReady bool `json:"storageReady,omitempty"`

	// Conditions contains detailed conditions for this node.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterPhase is the phase of cluster lifecycle.
type ClusterPhase string

const (
	PhaseCreating ClusterPhase = "Creating"
	PhaseReady    ClusterPhase = "Ready"
	PhaseUpdating ClusterPhase = "Updating"
	PhaseFailed   ClusterPhase = "Failed"
)

// NodeCondition represents the condition of a single node.
type NodeCondition struct {
	// Name is the node name.
	Name string `json:"name,omitempty"`

	// Ready indicates if the node is ready.
	Ready bool `json:"ready,omitempty"`

	// Health is the health status of the node.
	Health string `json:"health,omitempty"`

	// Phase is the lifecycle phase of the node.
	Phase string `json:"phase,omitempty"`

	// PodName is the Kubernetes Pod name.
	PodName string `json:"podName,omitempty"`

	// StorageReady indicates if the node's storage is ready.
	StorageReady bool `json:"storageReady,omitempty"`

	// StorageClassName is the storage class name in use.
	StorageClassName string `json:"storageClassName,omitempty"`

	// StorageSize is the storage size configured.
	StorageSize string `json:"storageSize,omitempty"`

	// Conditions contains detailed conditions for this node.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RiakNodeMember represents a cluster member node.
type RiakNodeMember struct {
	// Name is the member name.
	Name string `json:"name,omitempty"`

	// Pod is the Kubernetes Pod name.
	Pod string `json:"pod,omitempty"`

	// Ready indicates if the node is ready.
	Ready bool `json:"ready,omitempty"`

	// Health is the health status of the node.
	Health string `json:"health,omitempty"`

	// Phase is the lifecycle phase of the node.
	Phase string `json:"phase,omitempty"`

	// StorageReady indicates if the node's storage is ready.
	StorageReady bool `json:"storageReady,omitempty"`

	// Conditions contains detailed conditions for this node.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RiakCluster is the Schema for the riakclusters API.
type RiakCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RiakClusterSpec   `json:"spec,omitempty"`
	Status RiakClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RiakClusterList contains a list of RiakCluster.
type RiakClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RiakCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RiakCluster{}, &RiakClusterList{})
}
