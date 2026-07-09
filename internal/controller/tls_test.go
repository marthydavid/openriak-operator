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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

var _ = Describe("TLS certificate builders", func() {
	Context("buildClusterCertificate", func() {
		It("sets the expected fields for a cluster Certificate", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "mycluster", Namespace: "mynamespace"},
				Spec: riakv1.RiakClusterSpec{
					TLS: &riakv1.TLSConfig{
						Enabled: true,
						CertManager: &riakv1.CertManagerConfig{
							IssuerName: "my-issuer",
							IssuerKind: "ClusterIssuer",
						},
					},
				},
			}
			cert := buildClusterCertificate(cluster)

			Expect(cert.GetKind()).To(Equal("Certificate"))
			Expect(cert.GetAPIVersion()).To(Equal("cert-manager.io/v1"))
			Expect(cert.GetName()).To(Equal("mycluster-tls"))
			Expect(cert.GetNamespace()).To(Equal("mynamespace"))

			spec := unstructuredNestedMap(cert.Object, "spec")
			Expect(spec["secretName"]).To(Equal("mycluster-tls"))

			issuerRef := unstructuredNestedMap(spec, "issuerRef")
			Expect(issuerRef["name"]).To(Equal("my-issuer"))
			Expect(issuerRef["kind"]).To(Equal("ClusterIssuer"))

			dnsNames := unstructuredNestedStringSlice(spec, "dnsNames")
			Expect(dnsNames).To(ContainElement("*.mycluster-headless.mynamespace.svc.cluster.local"))
			Expect(dnsNames).To(ContainElement("mycluster-headless.mynamespace.svc.cluster.local"))
		})

		It("defaults IssuerKind to Issuer when not set", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
				Spec: riakv1.RiakClusterSpec{
					TLS: &riakv1.TLSConfig{
						Enabled:     true,
						CertManager: &riakv1.CertManagerConfig{IssuerName: "issuer"},
					},
				},
			}
			cert := buildClusterCertificate(cluster)
			spec := unstructuredNestedMap(cert.Object, "spec")
			issuerRef := unstructuredNestedMap(spec, "issuerRef")
			Expect(issuerRef["kind"]).To(Equal("Issuer"))
		})
	})

	Context("buildUserCertificate", func() {
		It("sets commonName to the Riak username and correct secret name", func() {
			certRef := &riakv1.UserCertificateRef{
				IssuerRef: riakv1.CertIssuerRef{Name: "ca-issuer", Kind: "Issuer"},
			}
			cert := buildUserCertificate("my-riak-user", "default", "riakuser1", certRef)

			Expect(cert.GetName()).To(Equal("my-riak-user-client-tls"))
			Expect(cert.GetNamespace()).To(Equal("default"))

			spec := unstructuredNestedMap(cert.Object, "spec")
			Expect(spec["commonName"]).To(Equal("riakuser1"))
			Expect(spec["secretName"]).To(Equal("my-riak-user-client-tls"))
		})

		It("uses explicit SecretName when provided", func() {
			certRef := &riakv1.UserCertificateRef{
				IssuerRef:  riakv1.CertIssuerRef{Name: "issuer"},
				SecretName: "custom-secret",
			}
			cert := buildUserCertificate("u", "ns", "alice", certRef)
			spec := unstructuredNestedMap(cert.Object, "spec")
			Expect(spec["secretName"]).To(Equal("custom-secret"))
		})

		It("defaults IssuerKind to Issuer when not set", func() {
			certRef := &riakv1.UserCertificateRef{
				IssuerRef: riakv1.CertIssuerRef{Name: "issuer"},
			}
			cert := buildUserCertificate("u", "ns", "alice", certRef)
			spec := unstructuredNestedMap(cert.Object, "spec")
			issuerRef := unstructuredNestedMap(spec, "issuerRef")
			Expect(issuerRef["kind"]).To(Equal("Issuer"))
		})
	})
})

// unstructuredNestedMap is a test helper that extracts a nested map from an interface{} map.
// It returns nil when the key is absent or holds a different type.
func unstructuredNestedMap(obj map[string]interface{}, key string) map[string]interface{} {
	m, _ := obj[key].(map[string]interface{})
	return m
}

// unstructuredNestedStringSlice extracts a []string from a nested interface{} slice.
// It returns nil when the key is absent or any element is not a string.
func unstructuredNestedStringSlice(obj map[string]interface{}, key string) []string {
	raw, ok := obj[key].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}
