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

	// Grants are the permissions to grant to this user.
	Grants []Grant `json:"grants,omitempty"`

	// CertificateRef configures mTLS client-certificate authentication via cert-manager.
	// It is required: users always authenticate by client certificate (password auth is
	// not supported). Riak authenticates the user by the certificate's CommonName, which
	// the operator sets to spec.username.
	// +kubebuilder:validation:Required
	CertificateRef *UserCertificateRef `json:"certificateRef"`
}

// UserCertificateRef says where the user's client certificate comes from: issued
// by cert-manager (issuerRef) or already present in a Secret from an external CA
// (externalSecretName). Riak authenticates the user by client certificate; the
// certificate's CommonName must match spec.username.
// +kubebuilder:validation:XValidation:rule="has(self.issuerRef) != has(self.externalSecretName)",message="set exactly one of issuerRef or externalSecretName"
// +kubebuilder:validation:XValidation:rule="!(has(self.externalSecretName) && has(self.secretName))",message="secretName only applies to issuerRef; an external Secret is named by externalSecretName"
type UserCertificateRef struct {
	// IssuerRef references the cert-manager Issuer or ClusterIssuer to sign the certificate.
	// +optional
	IssuerRef *CertIssuerRef `json:"issuerRef,omitempty"`

	// SecretName is the Kubernetes Secret where cert-manager stores the issued certificate.
	// Defaults to <riakuser-name>-client-tls. Only with issuerRef.
	SecretName string `json:"secretName,omitempty"`

	// ExternalSecretName names an existing Secret (same namespace) holding the
	// user's certificate, issued by an external CA, in tls.crt (leaf first, then
	// any intermediates). The operator creates no Certificate; it validates the
	// Secret (CommonName == spec.username, client-auth usage, not expired, chains
	// to a CA Riak trusts) and reports the result in status.certificateReady. The
	// issuing CA must be trusted by the cluster: list it in
	// RiakCluster spec.tls.additionalClientCAs.
	// +kubebuilder:validation:MinLength=1
	// +optional
	ExternalSecretName string `json:"externalSecretName,omitempty"`
}

// CertIssuerRef identifies a cert-manager Issuer or ClusterIssuer.
type CertIssuerRef struct {
	// Name is the issuer name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Kind is the issuer kind: Issuer or ClusterIssuer. Defaults to Issuer.
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	Kind string `json:"kind,omitempty"`
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

	// Username is the Riak username that was created.
	Username string `json:"username,omitempty"`

	// CertificateReady indicates if cert-manager has issued the mTLS client
	// certificate. It is reported separately from Phase because issuance is
	// asynchronous: a Ready user whose certificate is still pending cannot
	// authenticate yet.
	CertificateReady bool `json:"certificateReady,omitempty"`

	// CertificateError explains why the client certificate is not ready.
	CertificateError string `json:"certificateError,omitempty"`

	// ClusterName is the cluster the user was created on.
	ClusterName string `json:"clusterName,omitempty"`

	// Grants lists the grants applied to this user in Riak.
	Grants []Grant `json:"grants,omitempty"`

	// Conditions contains detailed conditions for this user.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Cert",type=boolean,JSONPath=`.status.certificateReady`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.clusterName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
