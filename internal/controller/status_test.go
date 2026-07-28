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
	"errors"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/internal/riak"
)

// errGrantRejected is the failure a mocked riak-admin returns for a grant call.
var errGrantRejected = errors.New("{unknown_permission}")

// makeRiakPod creates a Riak node pod for a cluster and sets its status.
func makeRiakPod(ctx context.Context, ns, clusterName, podName string, status corev1.PodStatus, extraContainers ...corev1.Container) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels:    map[string]string{"app": "riak", "cluster": clusterName},
		},
		Spec: corev1.PodSpec{
			Containers: append([]corev1.Container{
				{Name: "riak", Image: "ghcr.io/marthydavid/riak:3.2.6"},
			}, extraContainers...),
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status = status
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	return pod
}

// readyPodStatus is a Running pod whose Ready condition is True.
func readyPodStatus() corev1.PodStatus {
	return corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
}

// markCertificateReady sets the cert-manager Ready condition on an issued Certificate.
// The envtest Certificate CRD has no status subresource, so status travels with the object.
func markCertificateReady(ctx context.Context, name, ns string) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cert)).To(Succeed())
	Expect(unstructured.SetNestedSlice(cert.Object, []interface{}{
		map[string]interface{}{
			"type":    "Ready",
			"status":  "True",
			"reason":  "Ready",
			"message": "Certificate is up to date and has not expired",
		},
	}, "status", "conditions")).To(Succeed())
	Expect(k8sClient.Update(ctx, cert)).To(Succeed())
}

var _ = Describe("Resource status reporting", func() {
	const ns = "default"
	ctx := context.Background()

	Context("RiakCluster status", func() {
		It("reports nodes, storage, monitoring, TLS and dependent resources", func() {
			const clusterName = "status-full-cluster"
			nn := types.NamespacedName{Name: clusterName, Namespace: ns}

			By("creating an ephemeral, monitored, TLS-enabled cluster")
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					Size:             1,
					Image:            "ghcr.io/marthydavid/riak:3.2.6",
					EphemeralStorage: true,
					Monitoring:       &riakv1.MonitoringConfig{Enabled: true},
					TLS: &riakv1.TLSConfig{
						Enabled:     true,
						CertManager: &riakv1.CertManagerConfig{IssuerName: "test-issuer"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
				_, _ = reconcileCluster(ctx, clusterName, ns)
			})

			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			By("creating a bucket and a user targeting the cluster")
			bucket := &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: "status-full-bucket", Namespace: ns},
				Spec:       riakv1.RiakBucketSpec{ClusterName: clusterName, BucketName: "orders"},
			}
			Expect(k8sClient.Create(ctx, bucket)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, bucket) })
			bucket.Status.Phase = riakv1.BucketPhaseReady
			Expect(k8sClient.Status().Update(ctx, bucket)).To(Succeed())

			user := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: "status-full-user", Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterName,
					Username:    "ivan",
					CertificateRef: &riakv1.UserCertificateRef{
						IssuerRef: riakv1.CertIssuerRef{Name: "test-issuer"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, user) })

			By("creating a ready node pod whose exporter sidecar is also ready")
			pod := makeRiakPod(ctx, ns, clusterName, clusterName+"-0", corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "riak", Ready: true},
					{Name: exporterContainerName, Ready: true},
				},
			}, corev1.Container{Name: exporterContainerName, Image: defaultExporterImage})
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			observed := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, nn, observed)).To(Succeed())

			By("reporting node counts and per-node state")
			Expect(observed.Status.Phase).To(Equal(riakv1.PhaseReady))
			Expect(observed.Status.ReadyNodes).To(Equal(int32(1)))
			Expect(observed.Status.TotalNodes).To(Equal(int32(1)))

			Expect(observed.Status.Members).To(HaveLen(1))
			member := observed.Status.Members[0]
			Expect(member.Name).To(Equal(clusterName + "-0"))
			Expect(member.Pod).To(Equal(clusterName + "-0"))
			Expect(member.Ready).To(BeTrue())
			Expect(member.Health).To(Equal(riakv1.NodeHealthy))
			Expect(member.Phase).To(Equal(string(corev1.PodRunning)))
			Expect(member.StorageReady).To(BeTrue(), "emptyDir storage is ready once the pod runs")
			Expect(meta.IsStatusConditionTrue(member.Conditions, conditionReady)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(member.Conditions, conditionStorageReady)).To(BeTrue())

			Expect(observed.Status.NodeConditions).To(HaveLen(1))
			nodeCond := observed.Status.NodeConditions[0]
			Expect(nodeCond.PodName).To(Equal(clusterName + "-0"))
			Expect(nodeCond.Health).To(Equal(riakv1.NodeHealthy))
			Expect(nodeCond.StorageReady).To(BeTrue())

			By("reporting storage configuration")
			Expect(observed.Status.EphemeralStorage).To(BeTrue())
			Expect(observed.Status.StorageClassName).To(BeEmpty())
			Expect(observed.Status.StorageSize).To(BeEmpty())

			By("reporting monitoring state")
			Expect(observed.Status.MonitoringStatus.Enabled).To(BeTrue())
			Expect(observed.Status.MonitoringStatus.ExporterReady).To(BeTrue())
			Expect(observed.Status.MonitoringStatus.ExporterError).To(BeEmpty())
			Expect(observed.Status.MonitoringStatus.ServiceMonitorReady).To(BeTrue())

			By("reporting TLS as pending while cert-manager has not issued the certificate")
			Expect(observed.Status.TLSStatus.Enabled).To(BeTrue())
			Expect(observed.Status.TLSStatus.CertManagerReady).To(BeFalse())
			Expect(observed.Status.TLSStatus.CertManagerError).To(ContainSubstring("not been issued"))
			Expect(observed.Status.TLSStatus.InterNodeReady).To(BeFalse())
			Expect(observed.Status.TLSStatus.ClientReady).To(BeFalse())

			By("reporting the dependent buckets and users")
			Expect(observed.Status.Buckets).To(Equal([]riakv1.RiakBucketRef{
				{Name: "status-full-bucket", Ready: true},
			}))
			Expect(observed.Status.Users).To(Equal([]riakv1.RiakUserRef{
				{Name: "status-full-user", Ready: false},
			}))

			By("flipping TLS to ready once the Certificate reports Ready")
			markCertificateReady(ctx, clusterCertName(clusterName), ns)
			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, observed)).To(Succeed())
			Expect(observed.Status.TLSStatus.CertManagerReady).To(BeTrue())
			Expect(observed.Status.TLSStatus.CertManagerError).To(BeEmpty())
			Expect(observed.Status.TLSStatus.InterNodeReady).To(BeTrue())
			Expect(observed.Status.TLSStatus.ClientReady).To(BeTrue())
		})

		It("reports durable storage and PVC-backed node readiness", func() {
			const clusterName = "status-durable-cluster"
			nn := types.NamespacedName{Name: clusterName, Namespace: ns}
			size := resource.MustParse("5Gi")

			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					Size:             1,
					Image:            "ghcr.io/marthydavid/riak:3.2.6",
					StorageClassName: "fast-local",
					StorageSize:      &size,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
				_, _ = reconcileCluster(ctx, clusterName, ns)
			})
			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			By("reporting storage as not ready while the PVC does not exist")
			pod := makeRiakPod(ctx, ns, clusterName, clusterName+"-0", readyPodStatus())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			observed := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, nn, observed)).To(Succeed())
			Expect(observed.Status.EphemeralStorage).To(BeFalse())
			Expect(observed.Status.StorageClassName).To(Equal("fast-local"))
			Expect(observed.Status.StorageSize).To(Equal("5Gi"))
			Expect(observed.Status.Members[0].StorageReady).To(BeFalse())
			Expect(observed.Status.NodeConditions[0].StorageClassName).To(Equal("fast-local"))
			Expect(observed.Status.NodeConditions[0].StorageSize).To(Equal("5Gi"))
			Expect(meta.IsStatusConditionTrue(observed.Status.Members[0].Conditions, conditionStorageReady)).To(BeFalse())

			By("reporting storage as ready once the node's PVC is Bound")
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "data-" + clusterName + "-0", Namespace: ns},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: size},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pvc) })
			pvc.Status.Phase = corev1.ClaimBound
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())

			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, observed)).To(Succeed())
			Expect(observed.Status.Members[0].StorageReady).To(BeTrue())
			Expect(observed.Status.NodeConditions[0].StorageReady).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(observed.Status.Members[0].Conditions, conditionStorageReady)).To(BeTrue())
		})

		It("reports an unhealthy node and a missing exporter", func() {
			const clusterName = "status-unhealthy-cluster"
			nn := types.NamespacedName{Name: clusterName, Namespace: ns}

			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: riakv1.RiakClusterSpec{
					Size:             2,
					Image:            "ghcr.io/marthydavid/riak:3.2.6",
					EphemeralStorage: true,
					Monitoring:       &riakv1.MonitoringConfig{Enabled: true},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
				_, _ = reconcileCluster(ctx, clusterName, ns)
			})
			_, err := reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			pod := makeRiakPod(ctx, ns, clusterName, clusterName+"-0", corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  exporterContainerName,
						Ready: false,
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
						},
					},
				},
			}, corev1.Container{Name: exporterContainerName, Image: defaultExporterImage})
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

			_, err = reconcileCluster(ctx, clusterName, ns)
			Expect(err).NotTo(HaveOccurred())

			observed := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, nn, observed)).To(Succeed())
			Expect(observed.Status.Phase).To(Equal(riakv1.PhaseCreating))
			Expect(observed.Status.ReadyNodes).To(Equal(int32(0)))
			Expect(observed.Status.TotalNodes).To(Equal(int32(2)))
			Expect(observed.Status.Members[0].Health).To(Equal(riakv1.NodeUnhealthy))
			Expect(observed.Status.Members[0].Ready).To(BeFalse())
			Expect(observed.Status.MonitoringStatus.ExporterReady).To(BeFalse())
			Expect(observed.Status.MonitoringStatus.ExporterError).To(ContainSubstring("CrashLoopBackOff"))
			// TLS is off, so its status stays zeroed rather than claiming readiness.
			Expect(observed.Status.TLSStatus).To(Equal(riakv1.TLSStatus{}))
		})
	})

	Context("RiakBucket status", func() {
		const clusterName = "status-bucket-cluster"
		noopRunner := func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil }

		reconcileBucketWithMock := func(name string) reconcile.Result {
			r := &RiakBucketReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
			// First reconcile adds the finalizer and initialises status; the second
			// does the work whose result the caller inspects.
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			res, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			return res
		}

		BeforeEach(func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "ghcr.io/marthydavid/riak:3.2.6"},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			cluster.Status.Phase = riakv1.PhaseReady
			cluster.Status.Members = []riakv1.RiakNodeMember{{
				Name: clusterName + "-0", Pod: clusterName + "-0",
				Ready: true, Health: riakv1.NodeHealthy, Phase: string(corev1.PodRunning),
			}}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
				_, _ = reconcileCluster(ctx, clusterName, ns)
			})
		})

		It("records the applied bucket type, properties and serving nodes", func() {
			const bucketName = "status-bucket-applied"
			bucket := &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
				Spec: riakv1.RiakBucketSpec{
					ClusterName: clusterName,
					BucketName:  "orders",
					BucketType:  "orders-type",
					NVal:        5,
					AllowMulti:  true,
					Properties:  map[string]string{"backend": "leveldb"},
				},
			}
			Expect(k8sClient.Create(ctx, bucket)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, bucket) })

			reconcileBucketWithMock(bucketName)

			observed := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bucketName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.Status.Phase).To(Equal(riakv1.BucketPhaseReady))
			Expect(observed.Status.Created).To(BeTrue())
			Expect(observed.Status.BucketName).To(Equal("orders"))
			Expect(observed.Status.BucketType).To(Equal("orders-type"))
			Expect(observed.Status.NVal).To(Equal(int32(5)))
			Expect(observed.Status.ReplicationFactor).To(Equal(int32(5)))
			Expect(observed.Status.Properties).To(Equal(map[string]string{
				"backend": "leveldb", "n_val": "5", "allow_mult": "true",
			}))
			Expect(observed.Status.Nodes).To(Equal([]riakv1.BucketNodeRef{{
				Name: clusterName + "-0", Pod: clusterName + "-0",
				Ready: true, Health: riakv1.NodeHealthy,
			}}))
			Expect(meta.IsStatusConditionTrue(observed.Status.Conditions, conditionReady)).To(BeTrue())
		})

		It("defaults the bucket type and takes n_val from replicationFactor", func() {
			const bucketName = "status-bucket-defaults"
			bucket := &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
				Spec: riakv1.RiakBucketSpec{
					ClusterName:       clusterName,
					BucketName:        "events",
					ReplicationFactor: 3,
				},
			}
			Expect(k8sClient.Create(ctx, bucket)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, bucket) })

			reconcileBucketWithMock(bucketName)

			observed := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bucketName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.Status.BucketType).To(Equal("default"))
			Expect(observed.Status.NVal).To(Equal(int32(3)))
			Expect(observed.Status.Properties).To(Equal(map[string]string{"n_val": "3"}))
		})

		It("records a Ready=False condition when the cluster is missing", func() {
			const bucketName = "status-bucket-no-cluster"
			bucket := &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
				Spec: riakv1.RiakBucketSpec{
					ClusterName: "cluster-that-does-not-exist",
					BucketName:  "orphan",
				},
			}
			Expect(k8sClient.Create(ctx, bucket)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, bucket)
				_ = reconcileBucket(ctx, bucketName, ns)
			})

			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())

			observed := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bucketName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.Status.Phase).To(Equal(riakv1.BucketPhaseFailed))
			cond := meta.FindStatusCondition(observed.Status.Conditions, conditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ClusterNotFound"))

			By("not rewriting the status while the same failure repeats")
			before := observed.ResourceVersion
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bucketName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.ResourceVersion).To(Equal(before))
		})

		It("returns the error when the cluster lookup fails for a reason other than NotFound", func() {
			const bucketName = "status-bucket-lookup-error"
			bucket := &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
				Spec:       riakv1.RiakBucketSpec{ClusterName: clusterName, BucketName: "flaky"},
			}
			Expect(k8sClient.Create(ctx, bucket)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, bucket)
				_ = reconcileBucket(ctx, bucketName, ns)
			})

			// A cluster lookup that fails for any reason other than "absent" is a
			// transient problem, not a missing cluster: it must surface as a
			// reconcile error rather than a Failed bucket.
			wc, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			flaky := interceptor.NewClient(wc, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*riakv1.RiakCluster); ok {
						return apierrors.NewServiceUnavailable("api server is down")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})

			r := &RiakBucketReconciler{Client: flaky, Scheme: k8sClient.Scheme()}
			_, err = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: bucketName, Namespace: ns},
			})
			Expect(err).To(HaveOccurred())

			observed := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bucketName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.Status.Phase).NotTo(Equal(riakv1.BucketPhaseFailed))
		})
	})

	Context("RiakUser status", func() {
		const clusterName = "status-user-cluster"
		noopRunner := func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil }

		newUserReconciler := func() *RiakUserReconciler {
			return &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
			}
		}

		BeforeEach(func() {
			cluster := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "ghcr.io/marthydavid/riak:3.2.6"},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			cluster.Status.Phase = riakv1.PhaseReady
			cluster.Status.Members = []riakv1.RiakNodeMember{{
				Name: clusterName + "-0", Pod: clusterName + "-0", Ready: true,
			}}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
				_, _ = reconcileCluster(ctx, clusterName, ns)
			})
		})

		It("records identity, grants and certificate readiness", func() {
			const userName = "status-user-grants"
			grants := []riakv1.Grant{
				{Resource: "any", Permission: "read"},
				{Resource: "bucket", BucketName: "orders", Permission: "write"},
			}
			user := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterName,
					Username:    "judy",
					Grants:      grants,
					CertificateRef: &riakv1.UserCertificateRef{
						IssuerRef: riakv1.CertIssuerRef{Name: "test-issuer"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			r := newUserReconciler()
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}}
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, user)
				_, _ = r.Reconcile(ctx, req)
				cert := &unstructured.Unstructured{}
				cert.SetGroupVersionKind(certificateGVK)
				cert.SetName(userCertName(userName))
				cert.SetNamespace(ns)
				_ = k8sClient.Delete(ctx, cert)
			})

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			res, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			observed := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.Status.Phase).To(Equal(riakv1.UserPhaseReady))
			Expect(observed.Status.Username).To(Equal("judy"))
			Expect(observed.Status.ClusterName).To(Equal(clusterName))
			Expect(observed.Status.Grants).To(Equal(grants))

			By("reporting the certificate as pending, and requeuing until it is issued")
			Expect(observed.Status.CertificateReady).To(BeFalse())
			Expect(observed.Status.CertificateError).To(ContainSubstring("not been issued"))
			Expect(meta.IsStatusConditionTrue(observed.Status.Conditions, conditionCertificateReady)).To(BeFalse())
			Expect(meta.IsStatusConditionTrue(observed.Status.Conditions, conditionReady)).To(BeTrue())
			Expect(res.RequeueAfter).NotTo(BeZero())

			By("not rewriting the status while the certificate stays pending")
			before := observed.ResourceVersion
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.ResourceVersion).To(Equal(before))

			By("reporting the certificate as ready once cert-manager issues it")
			markCertificateReady(ctx, userCertName(userName), ns)
			res, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.Status.CertificateReady).To(BeTrue())
			Expect(observed.Status.CertificateError).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(observed.Status.Conditions, conditionCertificateReady)).To(BeTrue())
		})

		It("records a Ready=False condition when a grant fails", func() {
			const userName = "status-user-grant-fail"
			user := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterName,
					Username:    "mallory",
					Grants:      []riakv1.Grant{{Resource: "bucket", BucketName: "orders", Permission: "write"}},
					CertificateRef: &riakv1.UserCertificateRef{
						IssuerRef: riakv1.CertIssuerRef{Name: "test-issuer"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			failGrant := func(_ context.Context, _ string, args ...string) (string, error) {
				for _, a := range args {
					if a == "grant" {
						return "", errGrantRejected
					}
				}
				return "", nil
			}
			r := &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), failGrant),
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}}
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, user)
				_, _ = r.Reconcile(ctx, req)
				cert := &unstructured.Unstructured{}
				cert.SetGroupVersionKind(certificateGVK)
				cert.SetName(userCertName(userName))
				cert.SetNamespace(ns)
				_ = k8sClient.Delete(ctx, cert)
			})

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			observed := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, observed)).To(Succeed())
			Expect(observed.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			cond := meta.FindStatusCondition(observed.Status.Conditions, conditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("GrantFailed"))
		})
	})

	Context("status helpers", func() {
		It("resolves bucket properties with typed fields taking precedence", func() {
			props, nVal := effectiveBucketProperties(riakv1.RiakBucketSpec{
				NVal:              4,
				ReplicationFactor: 2,
				Properties:        map[string]string{"n_val": "9", "backend": "bitcask"},
			})
			Expect(nVal).To(Equal(int32(4)))
			Expect(props).To(Equal(map[string]string{"n_val": "4", "backend": "bitcask"}))

			props, nVal = effectiveBucketProperties(riakv1.RiakBucketSpec{})
			Expect(nVal).To(BeZero())
			Expect(props).To(BeEmpty(), "an empty spec leaves every property to Riak's defaults")

			// n_val supplied only through the free-form map still reaches Riak, so
			// status has to report it rather than claiming 0.
			props, nVal = effectiveBucketProperties(riakv1.RiakBucketSpec{
				Properties: map[string]string{"n_val": "7"},
			})
			Expect(nVal).To(Equal(int32(7)))
			Expect(props).To(HaveKeyWithValue("n_val", "7"))

			_, nVal = effectiveBucketProperties(riakv1.RiakBucketSpec{
				Properties: map[string]string{"n_val": "not-a-number"},
			})
			Expect(nVal).To(BeZero(), "an unparseable n_val is left for Riak to reject")

			props, _ = effectiveBucketProperties(riakv1.RiakBucketSpec{
				Properties: map[string]string{"allow_mult": "false"},
			})
			Expect(props).To(HaveKeyWithValue("allow_mult", "false"),
				"allow_mult is not written unless spec.allowMulti is true")
		})

		It("classifies pod health", func() {
			running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			Expect(podHealth(running, true)).To(Equal(riakv1.NodeHealthy))
			Expect(podHealth(running, false)).To(Equal(riakv1.NodeUnhealthy))
			Expect(podHealth(&corev1.Pod{}, false)).To(Equal(riakv1.NodeHealthUnknown))
			Expect(podHealth(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodUnknown}}, false)).
				To(Equal(riakv1.NodeHealthUnknown))
		})

		It("reads container readiness with a reason", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "riak-0"},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
					{Name: "riak", Ready: true},
					{
						Name: "terminated",
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{Reason: "Error"},
						},
					},
					{Name: "starting"},
				}},
			}

			ready, reason := containerReady(pod, "riak")
			Expect(ready).To(BeTrue())
			Expect(reason).To(BeEmpty())

			ready, reason = containerReady(pod, "terminated")
			Expect(ready).To(BeFalse())
			Expect(reason).To(Equal("Error"))

			ready, reason = containerReady(pod, "starting")
			Expect(ready).To(BeFalse())
			Expect(reason).To(Equal("container is not ready"))

			ready, reason = containerReady(pod, "absent")
			Expect(ready).To(BeFalse())
			Expect(reason).To(ContainSubstring("not found in pod riak-0"))
		})

		It("reads cert-manager Certificate readiness", func() {
			notIssued := &unstructured.Unstructured{Object: map[string]interface{}{}}
			ready, reason := certificateReady(notIssued)
			Expect(ready).To(BeFalse())
			Expect(reason).To(ContainSubstring("not been issued"))

			failing := &unstructured.Unstructured{Object: map[string]interface{}{
				"status": map[string]interface{}{"conditions": []interface{}{
					map[string]interface{}{"type": "Issuing", "status": "True"},
					map[string]interface{}{"type": "Ready", "status": "False", "message": "issuer not found"},
				}},
			}}
			ready, reason = certificateReady(failing)
			Expect(ready).To(BeFalse())
			Expect(reason).To(Equal("issuer not found"))

			issued := &unstructured.Unstructured{Object: map[string]interface{}{
				"status": map[string]interface{}{"conditions": []interface{}{
					map[string]interface{}{"type": "Ready", "status": "True"},
				}},
			}}
			ready, reason = certificateReady(issued)
			Expect(ready).To(BeTrue())
			Expect(reason).To(BeEmpty())
		})

		It("reports missing certificates and CRDs without erroring", func() {
			ready, reason := fetchCertificateReadiness(ctx, k8sClient, "no-such-cert", ns)
			Expect(ready).To(BeFalse())
			Expect(reason).To(ContainSubstring("does not exist yet"))
		})

		It("resolves the storage reported for a cluster", func() {
			size := resource.MustParse("20Gi")
			class, reported := clusterStorage(&riakv1.RiakCluster{
				Spec: riakv1.RiakClusterSpec{StorageClassName: "gp3", StorageSize: &size},
			})
			Expect(class).To(Equal("gp3"))
			Expect(reported).To(Equal("20Gi"))

			class, reported = clusterStorage(&riakv1.RiakCluster{})
			Expect(class).To(BeEmpty())
			Expect(reported).To(Equal(defaultStorageSize))

			class, reported = clusterStorage(&riakv1.RiakCluster{
				Spec: riakv1.RiakClusterSpec{EphemeralStorage: true, StorageClassName: "gp3"},
			})
			Expect(class).To(BeEmpty())
			Expect(reported).To(BeEmpty())
		})
	})
})
