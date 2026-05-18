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
