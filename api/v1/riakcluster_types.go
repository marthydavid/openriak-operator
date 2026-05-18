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

	// Image is the Riak Docker image to use (e.g., ghcr.io/marthydavid/riak:3.2.6)
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

	// RiakConfig is the riak.conf configuration for all nodes.
	RiakConfig map[string]string `json:"riakConfig,omitempty"`

	// TLS configuration for inter-node communication.
	TLS *TLSConfig `json:"tls,omitempty"`

	// ServicePort is the port for the Riak protocol service.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	ServicePort int32 `json:"servicePort,omitempty"`

	// NodeSelector for node affinity.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// TLSConfig defines TLS settings for the cluster.
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

	// Conditions represent the latest observed conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastUpdateTime is the last time the cluster was updated.
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// Members is a list of current cluster members.
	Members []RiakNodeMember `json:"members,omitempty"`
}

// ClusterPhase is the phase of cluster lifecycle.
type ClusterPhase string

const (
	PhaseCreating ClusterPhase = "Creating"
	PhaseReady    ClusterPhase = "Ready"
	PhaseUpdating ClusterPhase = "Updating"
	PhaseFailed   ClusterPhase = "Failed"
)

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
