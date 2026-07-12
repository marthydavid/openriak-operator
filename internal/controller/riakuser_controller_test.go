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
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/internal/riak"
)

func reconcileUser(ctx context.Context, name, namespace string) error { //nolint:unparam
	r := &RiakUserReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	return err
}

var _ = Describe("RiakUser Controller", func() {
	const ns = "default"
	ctx := context.Background()

	cleanupUser := func(name string) {
		u := &riakv1.RiakUser{}
		nn := types.NamespacedName{Name: name, Namespace: ns}
		if err := k8sClient.Get(ctx, nn, u); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, u)
		_ = reconcileUser(ctx, name, ns)
	}

	Context("cluster does not exist", func() {
		const userName = "user-no-cluster"
		nn := types.NamespacedName{Name: userName, Namespace: ns}

		BeforeEach(func() {
			existing := &riakv1.RiakUser{}
			if err := k8sClient.Get(ctx, nn, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
					ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
					Spec: riakv1.RiakUserSpec{
						ClusterName: "nonexistent-cluster",
						Username:    "alice",
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() { cleanupUser(userName) })

		It("adds the finalizer and sets status to Failed", func() {
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			Expect(u.Finalizers).To(ContainElement(riakUserFinalizerName))

			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			Expect(u.Status.Error).To(ContainSubstring("cluster not found"))
		})
	})

	Context("cluster exists but not ready", func() {
		const userName = "user-cluster-not-ready"
		const clusterRefName = "user-test-cluster-notready"
		nn := types.NamespacedName{Name: userName, Namespace: ns}

		BeforeEach(func() {
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			existing := &riakv1.RiakCluster{}
			if err := k8sClient.Get(ctx, cnn, existing); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
					ObjectMeta: metav1.ObjectMeta{Name: clusterRefName, Namespace: ns},
					Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
				})).To(Succeed())
			}
			existingU := &riakv1.RiakUser{}
			if err := k8sClient.Get(ctx, nn, existingU); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
					ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
					Spec: riakv1.RiakUserSpec{
						ClusterName: clusterRefName,
						Username:    "bob",
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			cleanupUser(userName)
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		})

		It("requeues without error when cluster phase is not Ready", func() {
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			Expect(u.Status.Phase).NotTo(Equal(riakv1.UserPhaseFailed))
		})
	})

	Context("missing password secret", func() {
		const userName = "user-bad-secret"
		const clusterRefName = "user-cluster-ready-for-secret-test"
		nn := types.NamespacedName{Name: userName, Namespace: ns}

		BeforeEach(func() {
			// Create a cluster and mark it Ready so the controller proceeds to user creation.
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			existing := &riakv1.RiakCluster{}
			if err := k8sClient.Get(ctx, cnn, existing); err != nil && errors.IsNotFound(err) {
				c := &riakv1.RiakCluster{
					ObjectMeta: metav1.ObjectMeta{Name: clusterRefName, Namespace: ns},
					Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "basho/riak-kv:latest"},
				}
				Expect(k8sClient.Create(ctx, c)).To(Succeed())
				// Force Ready phase in status
				c.Status.Phase = riakv1.PhaseReady
				c.Status.Members = []riakv1.RiakNodeMember{{Pod: clusterRefName + "-0", Name: clusterRefName + "-0"}}
				Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
			}
			existingU := &riakv1.RiakUser{}
			if err := k8sClient.Get(ctx, nn, existingU); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
					ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
					Spec: riakv1.RiakUserSpec{
						ClusterName: clusterRefName,
						Username:    "carol",
						PasswordSecret: &riakv1.PasswordSecretRef{
							Name: "nonexistent-secret",
							Key:  "password",
						},
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			cleanupUser(userName)
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		})

		It("sets status to Failed when password secret is missing", func() {
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())
			// Second reconcile processes the cluster-ready path
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			Expect(u.Status.Error).To(ContainSubstring("password secret not found"))
		})
	})

	Context("password key not found in secret", func() {
		const userName = "user-wrong-key"
		const clusterRefName = "user-cluster-wrongkey"
		const secretName = "riak-user-secret-wrongkey"
		nn := types.NamespacedName{Name: userName, Namespace: ns}

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
			snn := types.NamespacedName{Name: secretName, Namespace: ns}
			existingS := &corev1.Secret{}
			if err := k8sClient.Get(ctx, snn, existingS); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
					Data:       map[string][]byte{"otherkey": []byte("somevalue")},
				})).To(Succeed())
			}
			existingU := &riakv1.RiakUser{}
			if err := k8sClient.Get(ctx, nn, existingU); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
					ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
					Spec: riakv1.RiakUserSpec{
						ClusterName: clusterRefName,
						Username:    "erroruser",
						PasswordSecret: &riakv1.PasswordSecretRef{
							Name: secretName,
							Key:  "", // empty → defaults to "password", which doesn't exist in the secret
						},
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			cleanupUser(userName)
			s := &corev1.Secret{}
			snn := types.NamespacedName{Name: secretName, Namespace: ns}
			if err := k8sClient.Get(ctx, snn, s); err == nil {
				_ = k8sClient.Delete(ctx, s)
			}
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		})

		It("sets status to Failed when password key is not found in secret", func() {
			// First reconcile: adds finalizer, initialises status
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())
			// Second reconcile: cluster Ready, secret found, but key missing → Failed
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			Expect(u.Status.Error).To(ContainSubstring("password key not found"))
		})
	})

	Context("password secret key present", func() {
		const userName = "user-with-secret"
		const clusterRefName = "user-cluster-ready-secret-ok"
		const secretName = "riak-user-secret"
		nn := types.NamespacedName{Name: userName, Namespace: ns}

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
			snn := types.NamespacedName{Name: secretName, Namespace: ns}
			existingS := &corev1.Secret{}
			if err := k8sClient.Get(ctx, snn, existingS); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
					Data:       map[string][]byte{"password": []byte("mysecretpw")},
				})).To(Succeed())
			}
			existingU := &riakv1.RiakUser{}
			if err := k8sClient.Get(ctx, nn, existingU); err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
					ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
					Spec: riakv1.RiakUserSpec{
						ClusterName: clusterRefName,
						Username:    "dave",
						PasswordSecret: &riakv1.PasswordSecretRef{
							Name: secretName,
							Key:  "password",
						},
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			cleanupUser(userName)
			s := &corev1.Secret{}
			snn := types.NamespacedName{Name: secretName, Namespace: ns}
			if err := k8sClient.Get(ctx, snn, s); err == nil {
				_ = k8sClient.Delete(ctx, s)
			}
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				r := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		})

		It("reads the secret and attempts user creation (fails at kubectl, sets Failed)", func() {
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			// kubectl exec fails (no real cluster) → Failed with "failed to create user"
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			Expect(u.Status.Error).To(ContainSubstring("failed to create user"))
			Expect(u.Status.Error).NotTo(ContainSubstring("password secret not found"))
		})
	})

	Context("cluster is Ready — user creation succeeds (mock executor)", func() {
		const clusterRefName = "user-success-cluster"
		noopRunner := func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil }

		readyCluster := func() {
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
		}

		cleanupReadyCluster := func() {
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				cr := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = cr.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		}

		reconcileWithMock := func(userName string) {
			r := &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}

		cleanupUserMock := func(userName string) {
			u := &riakv1.RiakUser{}
			nn := types.NamespacedName{Name: userName, Namespace: ns}
			if err := k8sClient.Get(ctx, nn, u); err == nil {
				_ = k8sClient.Delete(ctx, u)
				r := &RiakUserReconciler{
					Client:   k8sClient,
					Scheme:   k8sClient.Scheme(),
					Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
				}
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			}
		}

		It("sets phase to Ready with default password (no PasswordSecret)", func() {
			const userName = "user-success-default-pw"
			readyCluster()
			defer cleanupReadyCluster()

			Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec:       riakv1.RiakUserSpec{ClusterName: clusterRefName, Username: "frank"},
			})).To(Succeed())
			defer cleanupUserMock(userName)

			reconcileWithMock(userName)

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseReady))
			Expect(u.Status.Created).To(BeTrue())
		})

		It("sets phase to Ready and processes grants", func() {
			const userName = "user-success-with-grants"
			readyCluster()
			defer cleanupReadyCluster()

			Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterRefName,
					Username:    "grace",
					Grants: []riakv1.Grant{
						{Resource: "any", Permission: "read"},
						{Resource: "bucket", Permission: "write", BucketName: "mydata"},
					},
				},
			})).To(Succeed())
			defer cleanupUserMock(userName)

			reconcileWithMock(userName)

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseReady))
		})

		It("sets phase to Failed when a grant fails", func() {
			const userName = "user-grant-fails"
			readyCluster()
			defer cleanupReadyCluster()

			Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterRefName,
					Username:    "heidi",
					Grants: []riakv1.Grant{
						{Resource: "bucket", Permission: "write", BucketName: "e2e"},
					},
				},
			})).To(Succeed())
			defer cleanupUserMock(userName)

			failGrant := func(_ context.Context, _ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "security grant") {
					return "", fmt.Errorf("{unknown_permission}")
				}
				return "", nil
			}
			r := &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), failGrant),
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}}
			_, err := r.Reconcile(ctx, req) // finalizer + status Creating
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req) // user created, grant fails → Failed
			Expect(err).NotTo(HaveOccurred())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			Expect(u.Status.Error).To(ContainSubstring("failed to grant"))
		})
	})

	Context("deletion", func() {
		const userName = "user-to-delete"
		nn := types.NamespacedName{Name: userName, Namespace: ns}

		It("removes the finalizer on delete", func() {
			By("creating and reconciling the user")
			Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec:       riakv1.RiakUserSpec{ClusterName: "some-cluster", Username: "eve"},
			})).To(Succeed())
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			Expect(u.Finalizers).To(ContainElement(riakUserFinalizerName))

			By("deleting the user and reconciling")
			Expect(k8sClient.Delete(ctx, u)).To(Succeed())
			Expect(reconcileUser(ctx, userName, ns)).To(Succeed())

			updated := &riakv1.RiakUser{}
			err := k8sClient.Get(ctx, nn, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(riakUserFinalizerName))
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}
		})
	})

	Context("Status().Update() fails in error paths", func() {
		// interceptor.NewClient requires client.WithWatch; create one from the test cfg.
		newFailStatusClient := func() client.Client {
			wc, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			return interceptor.NewClient(wc, interceptor.Funcs{
				SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
					return fmt.Errorf("status update injected failure")
				},
			})
		}
		reconcileWith := func(c client.Client, name string) error {
			r := &RiakUserReconciler{Client: c, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
			})
			return err
		}
		makeReadyCluster := func(clusterName string) *riakv1.RiakCluster {
			c := &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 1, Image: "ghcr.io/marthydavid/riak:3.2.6"},
			}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			c.Status.Phase = riakv1.PhaseReady
			c.Status.Members = []riakv1.RiakNodeMember{{Pod: clusterName + "-0", Name: clusterName + "-0"}}
			Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
			return c
		}

		// Pre-initialize status via k8sClient so the controller's first
		// Status().Update() (phase → Creating) is skipped, letting us reach
		// the specific error path that calls Status().Update() with the
		// intercepted (failing) client.
		initStatus := func(u *riakv1.RiakUser) {
			u.Status.Phase = riakv1.UserPhaseCreating
			Expect(k8sClient.Status().Update(ctx, u)).To(Succeed())
		}

		It("logs update error when cluster not found", func() {
			userName := "user-sfail-no-cluster"
			u := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec:       riakv1.RiakUserSpec{ClusterName: "missing-cluster", Username: "alice"},
			}
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, u) })
			initStatus(u)
			Expect(reconcileWith(newFailStatusClient(), userName)).To(Succeed())
		})

		It("logs update error when password secret not found", func() {
			clusterName := "user-sfail-no-secret-c"
			userName := "user-sfail-no-secret"
			c := makeReadyCluster(clusterName)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })

			u := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterName,
					Username:    "bob",
					PasswordSecret: &riakv1.PasswordSecretRef{
						Name: "missing-secret",
						Key:  "password",
					},
				},
			}
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, u) })
			initStatus(u)
			Expect(reconcileWith(newFailStatusClient(), userName)).To(Succeed())
		})

		It("logs update error when password key not found in secret", func() {
			clusterName := "user-sfail-bad-key-c"
			userName := "user-sfail-bad-key"
			c := makeReadyCluster(clusterName)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "sfail-bad-key-secret", Namespace: ns},
				Data:       map[string][]byte{"otherkey": []byte("value")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

			u := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterName,
					Username:    "carol",
					PasswordSecret: &riakv1.PasswordSecretRef{
						Name: "sfail-bad-key-secret",
						Key:  "password",
					},
				},
			}
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, u) })
			initStatus(u)
			Expect(reconcileWith(newFailStatusClient(), userName)).To(Succeed())
		})

		It("logs update error when CreateUser fails", func() {
			clusterName := "user-sfail-create-c"
			userName := "user-sfail-create"
			c := makeReadyCluster(clusterName)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })

			u := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec:       riakv1.RiakUserSpec{ClusterName: clusterName, Username: "dave"},
			}
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, u) })
			initStatus(u)

			failRunner := func(_ context.Context, _ string, _ ...string) (string, error) {
				return "", fmt.Errorf("riak-admin exec failed")
			}
			r := &RiakUserReconciler{
				Client:   newFailStatusClient(),
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), failRunner),
			}
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: userName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("certificate-auth — CertificateRef set", func() {
		const clusterRefName = "cert-auth-cluster"
		noopRunner := func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil }

		readyCertCluster := func() {
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
		}

		cleanupCertCluster := func() {
			c := &riakv1.RiakCluster{}
			cnn := types.NamespacedName{Name: clusterRefName, Namespace: ns}
			if err := k8sClient.Get(ctx, cnn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				cr := &RiakClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, _ = cr.Reconcile(ctx, reconcile.Request{NamespacedName: cnn})
			}
		}

		newUserCert := func(userName string) *unstructured.Unstructured {
			cert := &unstructured.Unstructured{}
			cert.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   certManagerGroup,
				Version: certManagerVersion,
				Kind:    certManagerKind,
			})
			cert.SetName(userCertName(userName))
			cert.SetNamespace(ns)
			return cert
		}

		cleanupCertUser := func(r *RiakUserReconciler, userName string) {
			u := &riakv1.RiakUser{}
			nn := types.NamespacedName{Name: userName, Namespace: ns}
			if err := k8sClient.Get(ctx, nn, u); err == nil {
				_ = k8sClient.Delete(ctx, u)
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			}
			_ = k8sClient.Delete(ctx, newUserCert(userName))
		}

		makeCertUser := func(userName, username string) {
			Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterRefName,
					Username:    username,
					CertificateRef: &riakv1.UserCertificateRef{
						IssuerRef: riakv1.CertIssuerRef{Name: "test-issuer", Kind: "Issuer"},
					},
				},
			})).To(Succeed())
		}

		It("creates the Certificate and sets phase to Ready when the executor succeeds", func() {
			const userName = "user-cert-auth"
			readyCertCluster()
			defer cleanupCertCluster()

			makeCertUser(userName, "certuser")

			r := &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
			}
			defer cleanupCertUser(r, userName)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}}
			// First reconcile: adds finalizer, sets status Creating
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// Second reconcile: creates the Certificate, cert-auth user and security source
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// Third reconcile: Certificate already exists — exercises the idempotent path
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseReady))
			Expect(u.Status.Created).To(BeTrue())

			By("verifying the cert-manager Certificate was created with CN = Riak username")
			cert := newUserCert(userName)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userCertName(userName), Namespace: ns}, cert)).To(Succeed())
			cn, _, err := unstructured.NestedString(cert.Object, "spec", "commonName")
			Expect(err).NotTo(HaveOccurred())
			Expect(cn).To(Equal("certuser"))
		})

		It("sets phase to Failed when cert-auth user creation fails", func() {
			const userName = "user-cert-auth-adduser-fail"
			readyCertCluster()
			defer cleanupCertCluster()

			makeCertUser(userName, "certfailuser")

			failAddUser := func(_ context.Context, _ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "add-user") {
					return "", fmt.Errorf("add-user exec failed")
				}
				return "", nil
			}
			r := &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), failAddUser),
			}
			defer cleanupCertUser(r, userName)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			Expect(u.Status.Error).To(ContainSubstring("failed to create user"))
		})

		It("sets phase to Failed when setting the certificate security source fails", func() {
			const userName = "user-cert-auth-source-fail"
			readyCertCluster()
			defer cleanupCertCluster()

			makeCertUser(userName, "certsourceuser")

			failAddSource := func(_ context.Context, _ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "add-source") {
					return "", fmt.Errorf("add-source exec failed")
				}
				return "", nil
			}
			r := &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), failAddSource),
			}
			defer cleanupCertUser(r, userName)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, u)).To(Succeed())
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseFailed))
			Expect(u.Status.Error).To(ContainSubstring("failed to set security source"))
		})
	})

	Context("non-existent resource", func() {
		It("returns no error for a missing user", func() {
			Expect(reconcileUser(ctx, "does-not-exist", ns)).To(Succeed())
		})
	})

	Context("manager registration", func() {
		It("SetupWithManager registers without error", func() {
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			r := &RiakUserReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
	})
})
