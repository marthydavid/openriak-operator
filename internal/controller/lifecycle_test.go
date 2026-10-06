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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/internal/riak"
)

// riakAdminRecorder is a kubectl runner that plays a Riak ring: nodes start as
// standalone one-member rings and only join the seed's ring on `cluster commit`.
// Every riak-admin command is recorded as "<pod>: <args>".
type riakAdminRecorder struct {
	cluster, ns string
	inRing      map[int]bool // pod ordinals in the seed's committed ring
	staged      map[int]bool // joins staged but not yet committed
	calls       []string
}

func newRiakAdminRecorder(cluster, ns string, ringSize int) *riakAdminRecorder {
	f := &riakAdminRecorder{cluster: cluster, ns: ns, inRing: map[int]bool{}, staged: map[int]bool{}}
	for i := 0; i < ringSize; i++ {
		f.inRing[i] = true
	}
	return f
}

func (f *riakAdminRecorder) node(i int) string {
	return fmt.Sprintf("riak@%s-%d.%s-headless.%s.svc.cluster.local", f.cluster, i, f.cluster, f.ns)
}

func (f *riakAdminRecorder) row(i int) string { return "valid 20.0% -- " + f.node(i) + "\n" }

// run implements the riak.Executor runner. kubectl args look like
// exec -n <ns> <pod> -c riak -- sh -c <script> riak-admin <riak-admin args...>.
func (f *riakAdminRecorder) run(_ context.Context, _ string, args ...string) (string, error) {
	pod := args[3]
	cmd := ""
	for i, a := range args {
		if a == "riak-admin" && i > 5 {
			cmd = strings.Join(args[i+1:], " ")
		}
	}
	f.calls = append(f.calls, pod+": "+cmd)

	var ordinal int
	_, _ = fmt.Sscanf(strings.TrimPrefix(pod, f.cluster+"-"), "%d", &ordinal)

	switch {
	case cmd == "member-status":
		if !f.inRing[ordinal] {
			return f.row(ordinal), nil // standalone ring
		}
		out := ""
		for i := 0; i < 16; i++ {
			if f.inRing[i] {
				out += f.row(i)
			}
		}
		return out, nil
	case strings.HasPrefix(cmd, "cluster join"):
		f.staged[ordinal] = true
	case cmd == "cluster commit":
		for i := range f.staged {
			f.inRing[i] = true
		}
		f.staged = map[int]bool{}
	}
	return "", nil
}

// matching returns the recorded calls that contain every fragment.
func (f *riakAdminRecorder) matching(fragments ...string) []string {
	var out []string
outer:
	for _, c := range f.calls {
		for _, frag := range fragments {
			if !strings.Contains(c, frag) {
				continue outer
			}
		}
		out = append(out, c)
	}
	return out
}

var _ = Describe("Resource lifecycle (scale up, add, delete)", func() {
	const ns = "default"
	ctx := context.Background()

	// newReadyCluster creates a RiakCluster and forces its status to Ready, the way
	// the bucket and user reconcilers expect to find it.
	newReadyCluster := func(name string, size int32) {
		c := &riakv1.RiakCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       riakv1.RiakClusterSpec{Size: size, Image: "basho/riak-kv:latest"},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		c.Status.Phase = riakv1.PhaseReady
		c.Status.Members = []riakv1.RiakNodeMember{{Pod: name + "-0", Name: name + "-0"}}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
	}

	deleteCluster := func(name string) {
		c := &riakv1.RiakCluster{}
		nn := types.NamespacedName{Name: name, Namespace: ns}
		if err := k8sClient.Get(ctx, nn, c); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, c)
		_, _ = reconcileCluster(ctx, name, ns)
	}

	// clusterStatus runs a cluster reconcile (which recomputes status from live
	// objects) and returns the refreshed cluster.
	clusterStatus := func(name string) *riakv1.RiakCluster {
		_, err := reconcileCluster(ctx, name, ns)
		Expect(err).NotTo(HaveOccurred())
		c := &riakv1.RiakCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, c)).To(Succeed())
		return c
	}

	Context("scaling a RiakCluster up", func() {
		const name = "scale-up-cluster"
		nn := types.NamespacedName{Name: name, Namespace: ns}

		AfterEach(func() {
			for i := 0; i < 5; i++ {
				_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%d", name, i), Namespace: ns}})
			}
			deleteCluster(name)
		})

		It("grows the StatefulSet and joins only the new nodes into the ring", func() {
			Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       riakv1.RiakClusterSpec{Size: 3, Image: "basho/riak-kv:latest"},
			})).To(Succeed())

			ring := newRiakAdminRecorder(name, ns, 3)
			r := &RiakClusterReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), ring.run),
			}
			reconcileOnce := func() *riakv1.RiakCluster {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
				Expect(err).NotTo(HaveOccurred())
				c := &riakv1.RiakCluster{}
				Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
				return c
			}
			stsReplicas := func() int32 {
				sts := &appsv1.StatefulSet{}
				Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
				return *sts.Spec.Replicas
			}

			By("starting with a formed three-node cluster")
			reconcileOnce() // creates the StatefulSet
			for i := 0; i < 3; i++ {
				makeRiakPod(ctx, ns, name, fmt.Sprintf("%s-%d", name, i), readyPodStatus())
			}
			c := reconcileOnce()
			Expect(stsReplicas()).To(Equal(int32(3)))
			Expect(c.Status.Phase).To(Equal(riakv1.PhaseReady))
			Expect(c.Status.TotalNodes).To(Equal(int32(3)))
			Expect(ring.matching("cluster join")).To(BeEmpty(), "a formed cluster must not be re-joined")

			By("raising spec.size to five")
			Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
			c.Spec.Size = 5
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			c = reconcileOnce()
			Expect(stsReplicas()).To(Equal(int32(5)))
			Expect(c.Status.TotalNodes).To(Equal(int32(5)))
			Expect(c.Status.Phase).To(Equal(riakv1.PhaseCreating), "Ready is withheld while pods are missing")
			Expect(c.Status.Conditions).To(ContainElement(HaveField("Reason", "AwaitingPods")))

			By("the two new pods starting as standalone rings")
			makeRiakPod(ctx, ns, name, name+"-3", readyPodStatus())
			makeRiakPod(ctx, ns, name, name+"-4", readyPodStatus())
			c = reconcileOnce()
			Expect(c.Status.Phase).To(Equal(riakv1.PhaseCreating))
			Expect(c.Status.Conditions).To(ContainElement(HaveField("Reason", "FormingCluster")))

			By("joining exactly the new nodes to the seed, then planning and committing once")
			seed := ring.node(0)
			Expect(ring.matching(name+"-3:", "cluster join "+seed)).To(HaveLen(1))
			Expect(ring.matching(name+"-4:", "cluster join "+seed)).To(HaveLen(1))
			Expect(ring.matching("cluster join")).To(HaveLen(2))
			Expect(ring.matching(name+"-0:", "cluster plan")).To(HaveLen(1))
			Expect(ring.matching(name+"-0:", "cluster commit")).To(HaveLen(1))

			By("becoming Ready once the ring holds all five nodes, without joining again")
			c = reconcileOnce()
			Expect(c.Status.Phase).To(Equal(riakv1.PhaseReady))
			Expect(c.Status.ReadyNodes).To(Equal(int32(5)))
			Expect(c.Status.Members).To(HaveLen(5))
			Expect(ring.matching("cluster join")).To(HaveLen(2))
		})
	})

	Context("adding and deleting RiakBuckets", func() {
		const clusterName = "lifecycle-bucket-cluster"
		var runner *riakAdminRecorder
		var r *RiakBucketReconciler

		reconcileBucket := func(name string) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
		}
		addBucket := func(name, bucket string, mutate func(*riakv1.RiakBucketSpec)) {
			spec := riakv1.RiakBucketSpec{ClusterName: clusterName, BucketName: bucket, BucketType: "default"}
			if mutate != nil {
				mutate(&spec)
			}
			Expect(k8sClient.Create(ctx, &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec,
			})).To(Succeed())
		}
		getBucket := func(name string) *riakv1.RiakBucket {
			b := &riakv1.RiakBucket{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, b)).To(Succeed())
			return b
		}
		bucketNames := func(c *riakv1.RiakCluster) []string {
			var out []string
			for _, b := range c.Status.Buckets {
				out = append(out, b.Name)
			}
			return out
		}

		BeforeEach(func() {
			runner = newRiakAdminRecorder(clusterName, ns, 1)
			r = &RiakBucketReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), runner.run),
			}
			newReadyCluster(clusterName, 1)
		})

		AfterEach(func() {
			for _, n := range []string{"lc-bucket-a", "lc-bucket-b"} {
				b := &riakv1.RiakBucket{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: ns}, b); err == nil {
					_ = k8sClient.Delete(ctx, b)
					_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: n, Namespace: ns}})
				}
			}
			deleteCluster(clusterName)
		})

		It("provisions each added bucket and lists them on the cluster status", func() {
			addBucket("lc-bucket-a", "sessions", nil)
			addBucket("lc-bucket-b", "events", func(s *riakv1.RiakBucketSpec) { s.NVal = 2 })
			reconcileBucket("lc-bucket-a") // adds the finalizer
			reconcileBucket("lc-bucket-a")
			reconcileBucket("lc-bucket-b")
			reconcileBucket("lc-bucket-b")

			Expect(getBucket("lc-bucket-a").Status.Phase).To(Equal(riakv1.BucketPhaseReady))
			b := getBucket("lc-bucket-b")
			Expect(b.Status.Phase).To(Equal(riakv1.BucketPhaseReady))
			Expect(b.Status.Properties).To(HaveKeyWithValue("n_val", "2"))
			Expect(runner.matching("bucket-type")).NotTo(BeEmpty(), "buckets are applied through riak-admin bucket-type")

			Expect(bucketNames(clusterStatus(clusterName))).To(ConsistOf("lc-bucket-a", "lc-bucket-b"))
		})

		It("re-applies the bucket when its spec changes", func() {
			addBucket("lc-bucket-a", "sessions", nil)
			reconcileBucket("lc-bucket-a")
			reconcileBucket("lc-bucket-a")
			Expect(getBucket("lc-bucket-a").Status.Properties).NotTo(HaveKey("n_val"))
			before := len(runner.matching("bucket-type"))

			b := getBucket("lc-bucket-a")
			b.Spec.NVal = 5
			Expect(k8sClient.Update(ctx, b)).To(Succeed())
			reconcileBucket("lc-bucket-a")

			Expect(getBucket("lc-bucket-a").Status.Properties).To(HaveKeyWithValue("n_val", "5"))
			Expect(len(runner.matching("bucket-type"))).To(BeNumerically(">", before))
			Expect(runner.matching("bucket-type", `"n_val":5`)).NotTo(BeEmpty(), "the new n_val reaches riak-admin")
		})

		It("drops a deleted bucket and removes it from the cluster status", func() {
			addBucket("lc-bucket-a", "sessions", nil)
			addBucket("lc-bucket-b", "events", nil)
			for _, n := range []string{"lc-bucket-a", "lc-bucket-b"} {
				reconcileBucket(n)
				reconcileBucket(n)
			}
			Expect(bucketNames(clusterStatus(clusterName))).To(HaveLen(2))

			Expect(k8sClient.Delete(ctx, getBucket("lc-bucket-a"))).To(Succeed())
			reconcileBucket("lc-bucket-a")

			err := k8sClient.Get(ctx, types.NamespacedName{Name: "lc-bucket-a", Namespace: ns}, &riakv1.RiakBucket{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the finalizer must be released")
			Expect(getBucket("lc-bucket-b").Status.Phase).To(Equal(riakv1.BucketPhaseReady), "other buckets are untouched")
			Expect(bucketNames(clusterStatus(clusterName))).To(ConsistOf("lc-bucket-b"))
		})
	})

	Context("adding and deleting RiakUsers", func() {
		const clusterName = "lifecycle-user-cluster"
		var runner *riakAdminRecorder
		var r *RiakUserReconciler

		reconcileUserTwice := func(name string) {
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
			for i := 0; i < 2; i++ {
				_, err := r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
			}
		}
		addUser := func(name, username string, grants ...riakv1.Grant) {
			Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterName,
					Username:    username,
					Grants:      grants,
					CertificateRef: &riakv1.UserCertificateRef{
						IssuerRef: riakv1.CertIssuerRef{Name: "test-issuer", Kind: "Issuer"},
					},
				},
			})).To(Succeed())
		}
		userNames := func(c *riakv1.RiakCluster) []string {
			var out []string
			for _, u := range c.Status.Users {
				out = append(out, u.Name)
			}
			return out
		}

		BeforeEach(func() {
			runner = newRiakAdminRecorder(clusterName, ns, 1)
			r = &RiakUserReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), runner.run),
			}
			newReadyCluster(clusterName, 1)
		})

		AfterEach(func() {
			for _, n := range []string{"lc-user-a", "lc-user-b"} {
				u := &riakv1.RiakUser{}
				nn := types.NamespacedName{Name: n, Namespace: ns}
				if err := k8sClient.Get(ctx, nn, u); err == nil {
					_ = k8sClient.Delete(ctx, u)
					_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
				}
			}
			deleteCluster(clusterName)
		})

		It("creates each added user in Riak with its grants and lists them on the cluster status", func() {
			addUser("lc-user-a", "alice", riakv1.Grant{Resource: "any", Permission: "read"})
			addUser("lc-user-b", "bob", riakv1.Grant{Resource: "bucket", BucketName: "events", Permission: "write"})
			reconcileUserTwice("lc-user-a")
			reconcileUserTwice("lc-user-b")

			for _, n := range []string{"lc-user-a", "lc-user-b"} {
				u := &riakv1.RiakUser{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: ns}, u)).To(Succeed())
				Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseReady), n)
			}
			Expect(runner.matching("security add-user alice")).NotTo(BeEmpty())
			Expect(runner.matching("security add-user bob")).NotTo(BeEmpty())
			Expect(runner.matching("security grant", "to bob")).NotTo(BeEmpty())

			Expect(userNames(clusterStatus(clusterName))).To(ConsistOf("lc-user-a", "lc-user-b"))
		})

		It("deletes the Riak user and releases the finalizer when the RiakUser is deleted", func() {
			addUser("lc-user-a", "alice")
			addUser("lc-user-b", "bob")
			reconcileUserTwice("lc-user-a")
			reconcileUserTwice("lc-user-b")
			Expect(userNames(clusterStatus(clusterName))).To(HaveLen(2))
			// The status refresh above recomputed phase and members from the (pod-less)
			// cluster; Riak-side cleanup only runs against a Ready cluster with members.
			c := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, c)).To(Succeed())
			c.Status.Phase = riakv1.PhaseReady
			c.Status.Members = []riakv1.RiakNodeMember{{Pod: clusterName + "-0", Name: clusterName + "-0"}}
			Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

			nn := types.NamespacedName{Name: "lc-user-a", Namespace: ns}
			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, nn, u)).To(Succeed())
			Expect(k8sClient.Delete(ctx, u)).To(Succeed())
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(runner.matching("security del-user alice")).To(HaveLen(1))
			Expect(runner.matching("security del-user bob")).To(BeEmpty(), "only the deleted user is removed from Riak")
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, nn, &riakv1.RiakUser{}))).To(BeTrue())
			Expect(userNames(clusterStatus(clusterName))).To(ConsistOf("lc-user-b"))
		})
	})
})
