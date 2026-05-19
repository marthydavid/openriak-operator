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

// RiakUserSpec defines the desired state of RiakUser.
type RiakUserSpec struct {
	// ClusterName is the name of the RiakCluster this user belongs to.
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`

	// Username is the username.
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`

	// Password is the user password (stored in a Secret).
	// PasswordSecret references a Secret containing the password.
	PasswordSecret *PasswordSecretRef `json:"passwordSecret,omitempty"`

	// Grants are the permissions to grant to this user.
	Grants []Grant `json:"grants,omitempty"`
}

// PasswordSecretRef references a Secret containing user password.
type PasswordSecretRef struct {
	// Name is the Secret name.
	Name string `json:"name"`

	// Key is the key in the Secret containing the password.
	Key string `json:"key,omitempty"`
}

// Grant represents a permission grant for a user.
type Grant struct {
	// Resource is what is being granted (bucket, any).
	// +kubebuilder:validation:Enum=bucket;any
	Resource string `json:"resource"`

	// BucketName is the bucket name (if resource is "bucket").
	BucketName string `json:"bucketName,omitempty"`

	// Permission is the permission type.
	// +kubebuilder:validation:Enum=read;write;delete;list;admin
	Permission string `json:"permission"`
}

// RiakUserStatus defines the observed state of RiakUser.
type RiakUserStatus struct {
	// Phase is the current phase (Creating, Ready, Failed).
	Phase UserPhase `json:"phase,omitempty"`

	// Created indicates if the user was successfully created.
	Created bool `json:"created,omitempty"`

	// LastUpdateTime is when the user was last updated.
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// Error contains error details if creation failed.
	Error string `json:"error,omitempty"`
}

// UserPhase is the phase of user lifecycle.
type UserPhase string

const (
	UserPhaseCreating UserPhase = "Creating"
	UserPhaseReady    UserPhase = "Ready"
	UserPhaseFailed   UserPhase = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RiakUser is the Schema for the riakusers API.
type RiakUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RiakUserSpec   `json:"spec,omitempty"`
	Status RiakUserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RiakUserList contains a list of RiakUser.
type RiakUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RiakUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RiakUser{}, &RiakUserList{})
}
