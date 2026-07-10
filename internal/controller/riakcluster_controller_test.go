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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// reconcileCluster is a helper that creates the reconciler and calls Reconcile.
func reconcileCluster(ctx context.Context, name, namespace string) (*RiakClusterReconciler, error) { //nolint:unparam
	r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	return r, err
}

// reconcileClusterWithImage creates a reconciler with the given DefaultImage and calls Reconcile.
func reconcileClusterWithImage(ctx context.Context, name, namespace, defaultImage string) error {
	r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), DefaultImage: defaultImage}
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	return err
}

var _ = Describe("RiakCluster Controller", func() {
	const ns = "default"
	ctx := context.Background()

	// cleanupCluster deletes the cluster and runs a reconcile to drain the finalizer.
	cleanupCluster := func(name string) {
		cluster := &riakv1.RiakCluster{}
		nn := types.NamespacedName{Name: name, Namespace: ns}
		if err := k8sClient.Get(ctx, nn, cluster); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, cluster)
		_, _ = reconcileCluster(ctx, name, ns)
	}

	Context("basic reconciliation", func() {
		const clusterName = "test-cluster"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}

		BeforeEach(func() {
			existing := &riakv1.RiakCluster{}
			if err := k8sClient.Get(ctx, nn, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
					ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
					Spec:       riakv1.RiakClusterSpec{Size: 3, Image: "basho/riak-kv:latest"},
				})).To(Succeed())
			}
		})

		AfterEach(func() { cleanupCluster(clusterName) })

		It("creates StatefulSet, headless Service and client Service", func() {
			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("basho/riak-kv:latest"))

			headless := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-headless", Namespace: ns}, headless)).To(Succeed())
			Expect(headless.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))

			clientSvc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn, clientSvc)).To(Succeed())
			Expect(clientSvc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		})

		It("adds finalizer and sets phase", func() {
			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			cluster := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, nn, cluster)).To(Succeed())
			Expect(cluster.Finalizers).To(ContainElement(riakClusterFinalizerName))
			Expect(cluster.Status.Phase).NotTo(BeEmpty())
		})

		It("reconcile is idempotent on second call", func() {
			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("custom spec", func() {
		const clusterName = "custom-cluster"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}
		storageSize := resource.MustParse("20Gi")

		BeforeEach(func() {
			existing := &riakv1.RiakCluster{}
			if err := k8sClient.Get(ctx, nn, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
					ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
					Spec: riakv1.RiakClusterSpec{
						Size:        5,
						Image:       "basho/riak-kv:2.9",
						ServicePort: 8087,
						StorageSize: &storageSize,
						RiakConfig:  map[string]string{"ring_size": "64"},
						Resources: &corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() { cleanupCluster(clusterName) })

		It("honours replica count and custom image", func() {
			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(5)))
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("basho/riak-kv:2.9"))

			// RIAK_RING_SIZE env var should be injected from RiakConfig
			var found bool
			for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "RIAK_RING_SIZE" && e.Value == "64" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected RIAK_RING_SIZE env var")
		})
	})

	Context("cluster deletion", func() {
		const clusterName = "delete-cluster"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}

		It("removes the finalizer so the object can be deleted", func() {
			By("creating and reconciling the cluster")
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
			})).To(Succeed())

			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			cluster := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, nn, cluster)).To(Succeed())
			Expect(cluster.Finalizers).To(ContainElement(riakClusterFinalizerName))

			By("deleting and reconciling again")
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			// After deletion reconcile the object should be gone or have no finalizer.
			updated := &riakv1.RiakCluster{}
			err = k8sClient.Get(ctx, nn, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(riakClusterFinalizerName))
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}
		})
	})

	Context("status update with ready pods", func() {
		const clusterName = "ready-cluster"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}

		AfterEach(func() { cleanupCluster(clusterName) })

		It("sets phase to Ready when all pods are ready", func() {
			By("creating a size-1 cluster")
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
			})).To(Succeed())

			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			By("creating a pod with Ready condition")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + "-0",
					Namespace: ns,
					Labels:    map[string]string{"app": "riak", "cluster": clusterName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "riak", Image: "basho/riak-kv:latest"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			// Update the pod status sub-resource so conditions are persisted.
			pod.Status = corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			By("reconciling again to pick up the ready pod")
			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			cluster := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, nn, cluster)).To(Succeed())
			Expect(cluster.Status.Phase).To(Equal(riakv1.PhaseReady))
			Expect(cluster.Status.ReadyNodes).To(Equal(int32(1)))
		})
	})

	Context("default image fallback", func() {
		const clusterName = "default-image-cluster"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}

		AfterEach(func() { cleanupCluster(clusterName) })

		It("uses DefaultImage when spec.image is empty", func() {
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1},
			})).To(Succeed())

			Expect(reconcileClusterWithImage(ctx, clusterName, ns, "custom/riak:test")).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("custom/riak:test"))
		})

		It("uses built-in default when DefaultImage and spec.image are both empty", func() {
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1},
			})).To(Succeed())

			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal(defaultRiakImage))

			By("keeping stdin open so riak console does not exit on EOF")
			Expect(sts.Spec.Template.Spec.Containers[0].Stdin).To(BeTrue())
			Expect(sts.Spec.Template.Spec.Containers[0].TTY).To(BeTrue())
		})

		It("spec.image takes precedence over DefaultImage", func() {
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "explicit/riak:spec"},
			})).To(Succeed())

			Expect(reconcileClusterWithImage(ctx, clusterName, ns, "custom/riak:flag")).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("explicit/riak:spec"))
		})
	})

	Context("TLS enabled", func() {
		const clusterName = "tls-cluster"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}

		AfterEach(func() { cleanupCluster(clusterName) })

		It("adds TLS volume, volume mount and SSL env vars to the StatefulSet", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					Size:  1,
					Image: "basho/riak-kv:latest",
					TLS: &riakv1.TLSConfig{
						Enabled:     true,
						CertManager: &riakv1.CertManagerConfig{IssuerName: "test-issuer", IssuerKind: "Issuer"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			// Call reconcileStatefulSet directly to avoid needing cert-manager CRDs in envtest.
			r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())

			By("checking TLS Secret volume is present")
			var foundVolume bool
			for _, v := range sts.Spec.Template.Spec.Volumes {
				if v.Name == riakTLSVolumeName {
					foundVolume = true
					Expect(v.VolumeSource.Secret.SecretName).To(Equal(clusterTLSSecretName(clusterName)))
				}
			}
			Expect(foundVolume).To(BeTrue(), "expected riak-tls volume in StatefulSet")

			By("checking TLS volume mount is in the riak container")
			container := sts.Spec.Template.Spec.Containers[0]
			var foundMount bool
			for _, m := range container.VolumeMounts {
				if m.Name == riakTLSVolumeName {
					foundMount = true
					Expect(m.MountPath).To(Equal(riakTLSMountPath))
					Expect(m.ReadOnly).To(BeTrue())
				}
			}
			Expect(foundMount).To(BeTrue(), "expected riak-tls volume mount in riak container")

			By("checking SSL env vars are injected")
			envMap := make(map[string]string)
			for _, e := range container.Env {
				envMap[e.Name] = e.Value
			}
			Expect(envMap["RIAK_CONFIG_SSL__CERTFILE"]).To(Equal(riakTLSCertFile))
			Expect(envMap["RIAK_CONFIG_SSL__KEYFILE"]).To(Equal(riakTLSKeyFile))
			Expect(envMap["RIAK_CONFIG_SSL__CACERTFILE"]).To(Equal(riakTLSCACertFile))
			Expect(envMap["RIAK_CONFIG_LISTENER__HTTPS__INTERNAL"]).To(Equal("0.0.0.0:8443"))
			Expect(envMap["RIAK_CONFIG_CHECK_CRL"]).To(Equal("off"),
				"cert-manager certs have no CRL distribution point; Riak's CRL check must be off")

			By("checking HTTPS container port 8443 is added")
			var foundHTTPSPort bool
			for _, p := range container.Ports {
				if p.Name == riakTLSPortName && p.ContainerPort == riakTLSPort {
					foundHTTPSPort = true
				}
			}
			Expect(foundHTTPSPort).To(BeTrue(), "expected https container port 8443")
		})

		It("does not add TLS volume or SSL env vars when TLS is nil", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())

			for _, v := range sts.Spec.Template.Spec.Volumes {
				Expect(v.Name).NotTo(Equal(riakTLSVolumeName))
			}
			container := sts.Spec.Template.Spec.Containers[0]
			for _, e := range container.Env {
				Expect(e.Name).NotTo(HavePrefix("RIAK_CONFIG_SSL__"))
			}
			for _, p := range container.Ports {
				Expect(p.Name).NotTo(Equal(riakTLSPortName))
			}
		})
	})

	Context("Reconcile with TLS certificate error", func() {
		const clusterName = "tls-reconcile-err"

		AfterEach(func() { cleanupCluster(clusterName) })

		It("sets status to Failed and returns error when issuerName is missing", func() {
			// TLS is enabled but certManager.issuerName is empty, so
			// reconcileTLSCertificate returns an error that propagates through
			// Reconcile's TLS error path (status → Failed, error returned).
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					Size:  1,
					Image: "basho/riak-kv:latest",
					TLS: &riakv1.TLSConfig{
						Enabled:     true,
						CertManager: &riakv1.CertManagerConfig{},
					},
				},
			})).To(Succeed())

			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).To(HaveOccurred())

			cluster := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			Expect(cluster.Status.Phase).To(Equal(riakv1.PhaseFailed))
		})
	})

	Context("Reconcile with TLS enabled end to end", func() {
		const clusterName = "tls-reconcile-ok"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}

		clusterCert := func() *unstructured.Unstructured {
			cert := &unstructured.Unstructured{}
			cert.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   certManagerGroup,
				Version: certManagerVersion,
				Kind:    certManagerKind,
			})
			cert.SetName(clusterCertName(clusterName))
			cert.SetNamespace(ns)
			return cert
		}

		AfterEach(func() {
			cleanupCluster(clusterName)
			_ = k8sClient.Delete(ctx, clusterCert())
		})

		It("creates the cert-manager Certificate along with the StatefulSet", func() {
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					Size:  1,
					Image: "basho/riak-kv:latest",
					TLS: &riakv1.TLSConfig{
						Enabled:     true,
						CertManager: &riakv1.CertManagerConfig{IssuerName: "test-issuer", IssuerKind: "ClusterIssuer"},
					},
				},
			})).To(Succeed())

			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile: Certificate already exists — idempotent path.
			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Certificate spec")
			cert := clusterCert()
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterCertName(clusterName), Namespace: ns}, cert)).To(Succeed())
			secretName, _, err := unstructured.NestedString(cert.Object, "spec", "secretName")
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).To(Equal(clusterTLSSecretName(clusterName)))
			issuerKind, _, err := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
			Expect(err).NotTo(HaveOccurred())
			Expect(issuerKind).To(Equal("ClusterIssuer"))
			dnsNames, _, err := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
			Expect(err).NotTo(HaveOccurred())
			Expect(dnsNames).To(ContainElement("*." + clusterName + "-headless." + ns + ".svc.cluster.local"))

			By("verifying the StatefulSet was created too")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
		})
	})

	Context("reconcileTLSCertificate early-return paths", func() {
		r := &RiakClusterReconciler{Client: nil, Scheme: nil} // no API calls for these paths

		It("returns nil when spec.tls is nil", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "no-tls", Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1},
			}
			Expect(r.reconcileTLSCertificate(ctx, cluster)).To(Succeed())
		})

		It("returns nil when spec.tls.enabled is false", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "tls-disabled", Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					TLS: &riakv1.TLSConfig{Enabled: false},
				},
			}
			Expect(r.reconcileTLSCertificate(ctx, cluster)).To(Succeed())
		})

		It("returns error when tls.enabled but issuerName is empty", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "tls-no-issuer", Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					TLS: &riakv1.TLSConfig{Enabled: true, CertManager: &riakv1.CertManagerConfig{}},
				},
			}
			err := r.reconcileTLSCertificate(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("issuerName must be set"))
		})

		It("returns error when tls.enabled but CertManager is nil", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "tls-no-cm", Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					TLS: &riakv1.TLSConfig{Enabled: true, CertManager: nil},
				},
			}
			err := r.reconcileTLSCertificate(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("issuerName must be set"))
		})
	})

	Context("reconcileService with TLS", func() {
		const clusterName = "svc-tls-cluster"
		nn := types.NamespacedName{Name: clusterName, Namespace: ns}

		AfterEach(func() { cleanupCluster(clusterName) })

		It("adds https port to both services when TLS is enabled", func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					Size:  1,
					Image: "basho/riak-kv:latest",
					TLS: &riakv1.TLSConfig{
						Enabled:     true,
						CertManager: &riakv1.CertManagerConfig{IssuerName: "test-issuer"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileService(ctx, cluster)).To(Succeed())

			headless := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-headless", Namespace: ns}, headless)).To(Succeed())
			var foundHTTPS bool
			for _, p := range headless.Spec.Ports {
				if p.Name == riakTLSPortName && p.Port == riakTLSPort {
					foundHTTPS = true
				}
			}
			Expect(foundHTTPS).To(BeTrue(), "expected https port on headless service")

			clientSvc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn, clientSvc)).To(Succeed())
			foundHTTPS = false
			for _, p := range clientSvc.Spec.Ports {
				if p.Name == riakTLSPortName && p.Port == riakTLSPort {
					foundHTTPS = true
				}
			}
			Expect(foundHTTPS).To(BeTrue(), "expected https port on client service")
		})
	})

	Context("non-existent resource", func() {
		It("returns no error for a missing cluster", func() {
			_, err := reconcileCluster(ctx, "does-not-exist", ns)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("manager registration", func() {
		It("SetupWithManager registers without error", func() {
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			r := &RiakClusterReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
	})
})
