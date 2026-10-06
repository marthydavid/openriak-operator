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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const (
	certManagerGroup   = "cert-manager.io"
	certManagerVersion = "v1"
	certManagerKind    = "Certificate"

	riakTLSPortName   = "https"
	riakTLSPort       = int32(8443)
	riakTLSVolumeName = "riak-tls"
	riakTLSMountPath  = "/etc/riak/certs"
	riakTLSCertFile   = "/etc/riak/certs/tls.crt"
	riakTLSKeyFile    = "/etc/riak/certs/tls.key"
	riakTLSCACertFile = "/etc/riak/certs/ca.crt"
)

// certificateGVK identifies the cert-manager Certificate resource the operator
// manages as an unstructured object.
var certificateGVK = schema.GroupVersionKind{
	Group:   certManagerGroup,
	Version: certManagerVersion,
	Kind:    certManagerKind,
}

// clusterTLSSecretName returns the name of the Secret cert-manager creates for cluster TLS.
func clusterTLSSecretName(clusterName string) string {
	return clusterName + "-tls"
}

// userClientTLSSecretName returns the default Secret name for a user's client certificate.
func userClientTLSSecretName(riakUserName string) string {
	return riakUserName + "-client-tls"
}

// clusterCertName returns the name of the cert-manager Certificate for a cluster.
func clusterCertName(clusterName string) string {
	return clusterName + "-tls"
}

// userCertName returns the name of the cert-manager Certificate for a user.
func userCertName(riakUserName string) string {
	return riakUserName + "-client-tls"
}

// buildClusterCertificate constructs a cert-manager Certificate unstructured object for
// inter-node and client-facing TLS. The certificate covers wildcard SANs for all pods in
// the StatefulSet's headless service.
func buildClusterCertificate(cluster *riakv1.RiakCluster) *unstructured.Unstructured {
	cm := cluster.Spec.TLS.CertManager
	issuerKind := cm.IssuerKind
	if issuerKind == "" {
		issuerKind = "Issuer"
	}

	dnsNames := []interface{}{
		// Wildcard covering every pod FQDN via the headless service
		fmt.Sprintf("*.%s-headless.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
		fmt.Sprintf("%s-headless.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
		// Client-facing service
		fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
		// Short names for in-cluster resolution
		cluster.Name + "-headless",
		cluster.Name,
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": certManagerGroup + "/" + certManagerVersion,
			"kind":       certManagerKind,
			"metadata": map[string]interface{}{
				"name":      clusterCertName(cluster.Name),
				"namespace": cluster.Namespace,
			},
			"spec": map[string]interface{}{
				"secretName": clusterTLSSecretName(cluster.Name),
				"issuerRef": map[string]interface{}{
					"name": cm.IssuerName,
					"kind": issuerKind,
				},
				"dnsNames": dnsNames,
				"usages": []interface{}{
					"server auth",
					"client auth",
					"digital signature",
					"key encipherment",
				},
			},
		},
	}
}

// buildUserCertificate constructs a cert-manager Certificate unstructured object for a
// RiakUser's client certificate. The CommonName is set to the Riak username so that Riak's
// certificate security source can match it.
func buildUserCertificate(riakUserName, namespace, riakUsername string, certRef *riakv1.UserCertificateRef) *unstructured.Unstructured {
	// Only called for cert-manager issued certificates (issuerRef set).
	issuerKind := certRef.IssuerRef.Kind
	if issuerKind == "" {
		issuerKind = "Issuer"
	}

	secretName := certRef.SecretName
	if secretName == "" {
		secretName = userClientTLSSecretName(riakUserName)
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": certManagerGroup + "/" + certManagerVersion,
			"kind":       certManagerKind,
			"metadata": map[string]interface{}{
				"name":      userCertName(riakUserName),
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"secretName": secretName,
				// CN must match the Riak username for certificate-based auth
				"commonName": riakUsername,
				"issuerRef": map[string]interface{}{
					"name": certRef.IssuerRef.Name,
					"kind": issuerKind,
				},
				"usages": []interface{}{
					"client auth",
					"digital signature",
					"key encipherment",
				},
			},
		},
	}
}

// tlsVolume returns the volume mounted at riakTLSMountPath. Normally it is the
// cert-manager Secret as is. With spec.tls.additionalClientCAs it is a projected
// volume taking tls.crt and tls.key from that Secret and ca.crt from the
// operator-owned trust bundle (cluster CA + additional CAs), so Riak's
// ssl.cacertfile trusts both while the file names stay the same.
func tlsVolume(cluster *riakv1.RiakCluster) corev1.Volume {
	if !hasAdditionalClientCAs(cluster) {
		return corev1.Volume{
			Name: riakTLSVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: clusterTLSSecretName(cluster.Name)},
			},
		}
	}
	return corev1.Volume{
		Name: riakTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: clusterTLSSecretName(cluster.Name)},
						Items: []corev1.KeyToPath{
							{Key: "tls.crt", Path: "tls.crt"},
							{Key: "tls.key", Path: "tls.key"},
						},
					}},
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: clusterTrustSecretName(cluster.Name)},
						Items:                []corev1.KeyToPath{{Key: trustBundleKey, Path: "ca.crt"}},
					}},
				},
			},
		},
	}
}
