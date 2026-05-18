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
