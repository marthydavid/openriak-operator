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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RiakBucketSpec defines the desired state of RiakBucket.
type RiakBucketSpec struct {
	// ClusterName is the name of the RiakCluster this bucket belongs to.
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`

	// BucketName is the name of the bucket.
	// +kubebuilder:validation:MinLength=1
	BucketName string `json:"bucketName"`

	// BucketType is the bucket type (default is "default").
	BucketType string `json:"bucketType,omitempty"`

	// ReplicationFactor is the number of replicas.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	ReplicationFactor int32 `json:"replicationFactor,omitempty"`

	// AllowMulti allows multiple values for same key (siblings).
	AllowMulti bool `json:"allowMulti,omitempty"`

	// NVal is the number of replicas (n_val).
	// +kubebuilder:validation:Minimum=1
	NVal int32 `json:"nVal,omitempty"`

	// Properties contains custom bucket properties as key-value pairs.
	Properties map[string]string `json:"properties,omitempty"`
}

// BucketNodeRef represents a node that has this bucket.
type BucketNodeRef struct {
	// Name is the node name.
	Name string `json:"name,omitempty"`

	// Pod is the Kubernetes Pod name.
	Pod string `json:"pod,omitempty"`

	// Ready indicates if the node is ready.
	Ready bool `json:"ready,omitempty"`

	// Health is the health status of the node.
	Health string `json:"health,omitempty"`
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

// RiakBucketRef represents a bucket reference for a user.
type RiakBucketRef struct {
	// Name is the bucket name.
	Name string `json:"name,omitempty"`

	// Ready indicates if the bucket is ready.
	Ready bool `json:"ready,omitempty"`
}

// RiakUserRef represents a user reference for a bucket.
type RiakUserRef struct {
	// Name is the user name.
	Name string `json:"name,omitempty"`

	// Ready indicates if the user is ready.
	Ready bool `json:"ready,omitempty"`
}

// RiakBucketStatus defines the observed state of RiakBucket.
type RiakBucketStatus struct {
	// Phase is the current phase (Creating, Ready, Failed).
	Phase BucketPhase `json:"phase,omitempty"`

	// Created indicates if the bucket was successfully created.
	Created bool `json:"created,omitempty"`

	// LastUpdateTime is when the bucket was last updated.
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// Error contains error details if creation failed.
	Error string `json:"error,omitempty"`

	// BucketName is the actual bucket name created (may differ from spec if spec was invalid).
	BucketName string `json:"bucketName,omitempty"`

	// BucketType is the actual bucket type created.
	BucketType string `json:"bucketType,omitempty"`

	// ReplicationFactor is the actual replication factor created.
	ReplicationFactor int32 `json:"replicationFactor,omitempty"`

	// NVal is the actual n_val created.
	NVal int32 `json:"nVal,omitempty"`

	// Properties contains the actual bucket properties created.
	Properties map[string]string `json:"properties,omitempty"`

	// Nodes lists the nodes that have this bucket.
	Nodes []BucketNodeRef `json:"nodes,omitempty"`

	// Conditions contains detailed conditions for this bucket.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BucketNodeRef represents a node that has this bucket.
type BucketNodeRef struct {
	// Name is the node name.
	Name string `json:"name,omitempty"`

	// Pod is the Kubernetes Pod name.
	Pod string `json:"pod,omitempty"`

	// Ready indicates if the node is ready.
	Ready bool `json:"ready,omitempty"`

	// Health is the health status of the node.
	Health string `json:"health,omitempty"`
}

// BucketPhase is the phase of bucket lifecycle.
type BucketPhase string

const (
	BucketPhaseCreating BucketPhase = "Creating"
	BucketPhaseReady    BucketPhase = "Ready"
	BucketPhaseFailed   BucketPhase = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RiakBucket is the Schema for the riakbuckets API.
type RiakBucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RiakBucketSpec   `json:"spec,omitempty"`
	Status RiakBucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RiakBucketList contains a list of RiakBucket.
type RiakBucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RiakBucket `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RiakBucket{}, &RiakBucketList{})
}
