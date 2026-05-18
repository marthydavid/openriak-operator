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

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/internal/riak"
)

func reconcileBucket(ctx context.Context, name, namespace string) error {
	r := &RiakBucketReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	return err
}

var _ = Describe("RiakBucket Controller", func() {
	const ns = "default"
	ctx := context.Background()

	cleanupBucket := func(name string) {
		b := &riakv1.RiakBucket{}
		nn := types.NamespacedName{Name: name, Namespace: ns}
		if err := k8sClient.Get(ctx, nn, b); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, b)
		_ = reconcileBucket(ctx, name, ns)
	}

	Context("cluster does not exist", func() {
		const bucketName = "bucket-no-cluster"
		nn := types.NamespacedName{Name: bucketName, Namespace: ns}

		BeforeEach(func() {
			existing := &riakv1.RiakBucket{}
			if err := k8sClient.Get(ctx, nn, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakBucket{
					ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
					Spec: riakv1.RiakBucketSpec{
						ClusterName: "nonexistent-cluster",
						BucketName:  "my-bucket",
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() { cleanupBucket(bucketName) })

		It("adds the finalizer and sets status to Failed", func() {
			// First reconcile: adds finalizer
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())

			b := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, nn, b)).To(Succeed())
			Expect(b.Finalizers).To(ContainElement(riakBucketFinalizerName))

			// Second reconcile: tries to get cluster, fails, sets status
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())

			Expect(k8sClient.Get(ctx, nn, b)).To(Succeed())
			Expect(b.Status.Phase).To(Equal(riakv1.BucketPhaseFailed))
			Expect(b.Status.Error).To(ContainSubstring("cluster not found"))
		})
	})

	Context("cluster exists but not ready", func() {
		const bucketName = "bucket-cluster-not-ready"
		const clusterRefName = "bucket-test-cluster-notready"
		nn := types.NamespacedName{Name: bucketName, Namespace: ns}

		BeforeEach(func() {
			// Create a cluster that is in Creating phase (not Ready)
			existing := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
					ObjectMeta: metav1.ObjectMeta{Name: clusterRefName, Namespace: ns},
					Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
				})).To(Succeed())
			}
			existingB := &riakv1.RiakBucket{}
			if err := k8sClient.Get(ctx, nn, existingB); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakBucket{
					ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
					Spec: riakv1.RiakBucketSpec{
						ClusterName: clusterRefName,
						BucketName:  "my-bucket",
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			cleanupBucket(bucketName)
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		})

		It("requeues without error when cluster phase is not Ready", func() {
			// First reconcile: adds finalizer + initialises status
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())
			// Second reconcile: cluster exists but phase is not Ready → requeue
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())

			b := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, nn, b)).To(Succeed())
			// Phase should stay at Creating (not Failed) since cluster exists
			Expect(b.Status.Phase).NotTo(Equal(riakv1.BucketPhaseFailed))
		})
	})

	Context("cluster is Ready — bucket creation attempted", func() {
		const bucketName = "bucket-cluster-ready"
		const clusterRefName = "bucket-ready-cluster"
		nn := types.NamespacedName{Name: bucketName, Namespace: ns}

		BeforeEach(func() {
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			existing := &riakv1.RiakCluster{}
			if err := k8sClient.Get(ctx, cnn, existing); err != nil && errors.IsNotFound(err) {
				c := &riakv1.RiakCluster{
					ObjectMeta: metav1.ObjectMeta{Name: clusterRefName, Namespace: ns},
					Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
				}
				Expect(k8sClient.Create(ctx, c)).To(Succeed())
				c.Status.Phase = riakv1.PhaseReady
				c.Status.Members = []riakv1.RiakNodeMember{{Pod: clusterRefName + "-0", Name: clusterRefName + "-0"}}
				Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
			}
			existingB := &riakv1.RiakBucket{}
			if err := k8sClient.Get(ctx, nn, existingB); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakBucket{
					ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
					Spec: riakv1.RiakBucketSpec{
						ClusterName: clusterRefName,
						BucketName:  "mydata",
						BucketType:  "mytype",
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			cleanupBucket(bucketName)
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		})

		It("attempts bucket creation and records failure (kubectl unavailable in unit tests)", func() {
			// First reconcile: adds finalizer, initialises status
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())
			// Second reconcile: cluster is Ready, calls CreateBucketType which fails at kubectl
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())

			b := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, nn, b)).To(Succeed())
			// kubectl exec is not connected to a real cluster so creation fails
			Expect(b.Status.Phase).To(Equal(riakv1.BucketPhaseFailed))
			Expect(b.Status.Error).To(ContainSubstring("failed to create bucket"))
		})
	})

	Context("cluster is Ready — bucket creation succeeds (mock executor)", func() {
		const bucketName = "bucket-success"
		const clusterRefName = "bucket-success-cluster"
		nn := types.NamespacedName{Name: bucketName, Namespace: ns}

		noopRunner := func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil }

		BeforeEach(func() {
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			existing := &riakv1.RiakCluster{}
			if err := k8sClient.Get(ctx, cnn, existing); err != nil && errors.IsNotFound(err) {
				c := &riakv1.RiakCluster{
					ObjectMeta: metav1.ObjectMeta{Name: clusterRefName, Namespace: ns},
					Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
				}
				Expect(k8sClient.Create(ctx, c)).To(Succeed())
				c.Status.Phase = riakv1.PhaseReady
				c.Status.Members = []riakv1.RiakNodeMember{{Pod: clusterRefName + "-0", Name: clusterRefName + "-0"}}
				Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
			}
			existingB := &riakv1.RiakBucket{}
			if err := k8sClient.Get(ctx, nn, existingB); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakBucket{
					ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
					Spec: riakv1.RiakBucketSpec{
						ClusterName: clusterRefName,
						BucketName:  "data",
						BucketType:  "default",
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			b := &riakv1.RiakBucket{}
			if err := k8sClient.Get(ctx, nn, b); err == nil {
				_ = k8sClient.Delete(ctx, b)
				r := &RiakBucketReconciler{
					Client:   k8sClient,
					Scheme:   k8sClient.Scheme(),
					Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
				}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			}
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				cr := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = cr.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		})

		It("sets phase to Ready when bucket creation succeeds", func() {
			r := &RiakBucketReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
			}
			req := reconcile.Request{NamespacedName: nn}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			b := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, nn, b)).To(Succeed())
			Expect(b.Status.Phase).To(Equal(riakv1.BucketPhaseReady))
			Expect(b.Status.Created).To(BeTrue())
			Expect(b.Status.Error).To(BeEmpty())
		})
	})

	Context("deletion", func() {
		const bucketName = "bucket-to-delete"
		nn := types.NamespacedName{Name: bucketName, Namespace: ns}

		It("removes the finalizer on delete", func() {
			By("creating and reconciling the bucket")
			Expect(k8sClient.Create(ctx, &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
				Spec: riakv1.RiakBucketSpec{
					ClusterName: "some-cluster",
					BucketName:  "data",
				},
			})).To(Succeed())
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())

			b := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, nn, b)).To(Succeed())
			Expect(b.Finalizers).To(ContainElement(riakBucketFinalizerName))

			By("deleting the bucket and reconciling")
			Expect(k8sClient.Delete(ctx, b)).To(Succeed())
			Expect(reconcileBucket(ctx, bucketName, ns)).To(Succeed())

			updated := &riakv1.RiakBucket{}
			err := k8sClient.Get(ctx, nn, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(riakBucketFinalizerName))
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}
		})
	})

	Context("non-existent resource", func() {
		It("returns no error for a missing bucket", func() {
			Expect(reconcileBucket(ctx, "does-not-exist", ns)).To(Succeed())
		})
	})

	Context("manager registration", func() {
		It("SetupWithManager registers without error", func() {
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			r := &RiakBucketReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
	})
})
