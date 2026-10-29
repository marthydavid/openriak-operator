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
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/internal/riak"
)

var _ = Describe("rollout decision", func() {
	pod := func(ord int32, rev string, ready bool) rolloutPod {
		return rolloutPod{ordinal: ord, name: fmt.Sprintf("c-%d", ord), revision: rev, ready: ready}
	}

	It("starts with the highest ordinal when nothing is updated yet", func() {
		next, why := rolloutNext(3, 3, "new", []rolloutPod{pod(0, "old", true), pod(1, "old", true), pod(2, "old", true)})
		Expect(next).To(Equal(int32(2)))
		Expect(why).To(BeEmpty())
	})

	It("moves on only when every already-updated pod is on the new revision and Ready", func() {
		pods := []rolloutPod{pod(0, "old", true), pod(1, "old", true), pod(2, "new", true)}
		next, why := rolloutNext(2, 3, "new", pods)
		Expect(next).To(Equal(int32(1)))
		Expect(why).To(BeEmpty())

		pods[2] = pod(2, "old", true)
		next, why = rolloutNext(2, 3, "new", pods)
		Expect(next).To(Equal(int32(2)), "the restarted pod is not on the new revision yet")
		Expect(why).To(ContainSubstring("c-2 is not yet on the new revision"))

		pods[2] = pod(2, "new", false)
		next, why = rolloutNext(2, 3, "new", pods)
		Expect(next).To(Equal(int32(2)))
		Expect(why).To(ContainSubstring("c-2 is not Ready"))
	})

	It("waits for a pod that does not exist yet", func() {
		next, why := rolloutNext(2, 3, "new", []rolloutPod{pod(0, "old", true), pod(1, "old", true)})
		Expect(next).To(Equal(int32(2)))
		Expect(why).To(ContainSubstring("not created yet"))
	})

	It("has nothing left to step at partition 0", func() {
		next, why := rolloutNext(0, 3, "new", nil)
		Expect(next).To(BeZero())
		Expect(why).To(BeEmpty())
	})

	It("restarts the seed pod last", func() {
		pods := []rolloutPod{pod(0, "old", true), pod(1, "new", true), pod(2, "new", true)}
		next, _ := rolloutNext(1, 3, "new", pods)
		Expect(next).To(BeZero(), "pod 0 is the last ordinal to be restarted")
	})

	It("parses pod ordinals", func() {
		o, ok := podOrdinal("my-cluster-12")
		Expect(ok).To(BeTrue())
		Expect(o).To(Equal(int32(12)))
		_, ok = podOrdinal("no-ordinal-x")
		Expect(ok).To(BeFalse())
	})

	It("holds an existing StatefulSet at the replica count and keeps progress during a rollout", func() {
		Expect(stsPartition(&appsv1.StatefulSet{}, 3)).To(BeZero(), "a new StatefulSet has nothing to protect")

		steady := &appsv1.StatefulSet{}
		steady.UID = "x"
		steady.Status.CurrentRevision, steady.Status.UpdateRevision = "a", "a"
		Expect(stsPartition(steady, 3)).To(Equal(int32(3)))

		rolling := steady.DeepCopy()
		rolling.Status.UpdateRevision = "b"
		two := int32(2)
		rolling.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: &two}
		Expect(stsPartition(rolling, 3)).To(Equal(int32(2)))
		Expect(stsPartition(rolling, 1)).To(Equal(int32(1)), "clamped after a scale-down")
	})
})

var _ = Describe("rolling update of a RiakCluster", func() {
	const (
		ns          = "default"
		clusterName = "rollout-cluster"
	)
	nn := types.NamespacedName{Name: clusterName, Namespace: ns}

	var transfersIdle bool
	var memberRows int
	reconciler := func() *RiakClusterReconciler {
		node := func(i int) string {
			return fmt.Sprintf("riak@%s-%d.%s-headless.%s.svc.cluster.local", clusterName, i, clusterName, ns)
		}
		runner := func(_ context.Context, _ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.HasSuffix(joined, "member-status"):
				out := ""
				for i := 0; i < memberRows; i++ {
					out += "valid 33.3% -- " + node(i) + "\n"
				}
				return out, nil
			case strings.HasSuffix(joined, "transfers"):
				if transfersIdle {
					return "No transfers active", nil
				}
				return "'riak@x' waiting to handoff 4 partitions\n\nActive Transfers:\n", nil
			}
			return "", nil
		}
		return &RiakClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Executor: riak.NewExecutorWithRunner(logr.Discard(), runner),
		}
	}
	reconcileOnce := func(r *RiakClusterReconciler) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
	}
	getSTS := func() *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
		return sts
	}
	partition := func() int32 { return livePartition(getSTS()) }
	getCluster := func() *riakv1.RiakCluster {
		c := &riakv1.RiakCluster{}
		Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
		return c
	}
	setPodRevision := func(i int, rev string) {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-%d", clusterName, i), Namespace: ns}, pod)).To(Succeed())
		pod.Labels[statefulSetRevisionLabel] = rev
		Expect(k8sClient.Update(ctx, pod)).To(Succeed())
	}
	setRevisions := func(current, update string) {
		sts := getSTS()
		sts.Status.CurrentRevision, sts.Status.UpdateRevision = current, update
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}

	// A three-node cluster with three Ready pods on revision "rev-a", whose StatefulSet then
	// gets a new revision "rev-b" (what the StatefulSet controller does when the template changes).
	startRollout := func(spec func(*riakv1.RiakClusterSpec)) *RiakClusterReconciler {
		transfersIdle, memberRows = true, 3
		c := &riakv1.RiakCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       riakv1.RiakClusterSpec{Size: 3, Image: "basho/riak-kv:latest"},
		}
		if spec != nil {
			spec(&c.Spec)
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		r := reconciler()
		reconcileOnce(r)
		for i := 0; i < 3; i++ {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%d", clusterName, i), Namespace: ns,
					Labels: map[string]string{"app": "riak", "cluster": clusterName, statefulSetRevisionLabel: "rev-a"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "riak", Image: "basho/riak-kv:latest"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			pod.Status = corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}
		setRevisions("rev-a", "rev-a")
		reconcileOnce(r)
		return r
	}

	AfterEach(func() {
		c := &riakv1.RiakCluster{}
		if err := k8sClient.Get(ctx, nn, c); err == nil {
			_ = k8sClient.Delete(ctx, c)
			_, _ = reconcileCluster(ctx, clusterName, ns)
		}
		for i := 0; i < 3; i++ {
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-%d", clusterName, i), Namespace: ns}}, client.GracePeriodSeconds(0))
		}
	})

	It("holds the partition at the replica count so a template change restarts nothing by itself", func() {
		r := startRollout(nil)
		Expect(partition()).To(Equal(int32(3)))

		c := getCluster()
		c.Spec.Image = "basho/riak-kv:other"
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(r)
		Expect(getSTS().Spec.Template.Spec.Containers[0].Image).To(Equal("basho/riak-kv:other"))
		Expect(partition()).To(Equal(int32(3)), "the new template must not roll any pod on its own")
	})

	It("restarts one node at a time, only after transfers have finished, seed last", func() {
		r := startRollout(nil)
		setRevisions("rev-a", "rev-b")

		By("waiting while transfers are running")
		transfersIdle = false
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(3)))
		blocked := meta.FindStatusCondition(getCluster().Status.Conditions, conditionRolloutBlocked)
		Expect(blocked).NotTo(BeNil())
		Expect(blocked.Status).To(Equal(metav1.ConditionTrue))
		Expect(blocked.Message).To(ContainSubstring("transfers"))
		Expect(meta.IsStatusConditionTrue(getCluster().Status.Conditions, conditionUpdateInProgress)).To(BeTrue())

		By("restarting the highest ordinal once the cluster is idle")
		transfersIdle = true
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(2)))
		Expect(meta.FindStatusCondition(getCluster().Status.Conditions, conditionRolloutBlocked)).To(BeNil())

		By("not moving on until the restarted pod is on the new revision")
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(2)))
		setPodRevision(2, "rev-b")
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(1)))

		By("restarting the seed pod last, and finishing")
		setPodRevision(1, "rev-b")
		reconcileOnce(r)
		Expect(partition()).To(BeZero())
		setPodRevision(0, "rev-b")
		setRevisions("rev-b", "rev-b")
		reconcileOnce(r)
		Expect(meta.FindStatusCondition(getCluster().Status.Conditions, conditionUpdateInProgress)).To(BeNil())
		Expect(partition()).To(Equal(int32(3)), "back to holding the template steady")
	})

	It("waits while a node is not a valid ring member", func() {
		r := startRollout(nil)
		setRevisions("rev-a", "rev-b")
		memberRows = 2
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(3)))
		blocked := meta.FindStatusCondition(getCluster().Status.Conditions, conditionRolloutBlocked)
		Expect(blocked).NotTo(BeNil())
		Expect(blocked.Message).To(ContainSubstring("valid ring members"))
	})

	It("does not wait for transfers when spec.rollout.waitForTransfers is false", func() {
		off := false
		r := startRollout(func(s *riakv1.RiakClusterSpec) { s.Rollout = &riakv1.RolloutConfig{WaitForTransfers: &off} })
		setRevisions("rev-a", "rev-b")
		transfersIdle = false
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(2)))
	})

	It("stops after the current node while spec.rollout.paused is set", func() {
		r := startRollout(func(s *riakv1.RiakClusterSpec) { s.Rollout = &riakv1.RolloutConfig{Paused: true} })
		setRevisions("rev-a", "rev-b")
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(3)))
		Expect(meta.FindStatusCondition(getCluster().Status.Conditions, conditionRolloutBlocked).Reason).To(Equal("Paused"))

		c := getCluster()
		c.Spec.Rollout.Paused = false
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(r)
		Expect(partition()).To(Equal(int32(2)))
	})

	It("raises RolloutStalled after spec.rollout.stallTimeout but never forces the restart", func() {
		r := startRollout(func(s *riakv1.RiakClusterSpec) {
			s.Rollout = &riakv1.RolloutConfig{StallTimeout: &metav1.Duration{Duration: time.Nanosecond}}
		})
		setRevisions("rev-a", "rev-b")
		transfersIdle = false
		reconcileOnce(r)
		reconcileOnce(r)
		Expect(meta.IsStatusConditionTrue(getCluster().Status.Conditions, conditionRolloutStalled)).To(BeTrue())
		Expect(partition()).To(Equal(int32(3)), "a stalled rollout keeps waiting")
	})

	It("restarts a single-node cluster without asking Riak anything", func() {
		r := startRollout(func(s *riakv1.RiakClusterSpec) { s.Size = 1 })
		transfersIdle = false
		setRevisions("rev-a", "rev-b")
		reconcileOnce(r)
		Expect(partition()).To(BeZero())
	})

	It("keeps the pods' termination grace period configurable, 300 s by default", func() {
		r := startRollout(nil)
		Expect(*getSTS().Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(300)))

		c := getCluster()
		grace := int64(900)
		c.Spec.TerminationGracePeriodSeconds = &grace
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(r)
		Expect(*getSTS().Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(900)))
	})

	It("creates a PodDisruptionBudget allowing one unavailable pod, and removes it when disabled", func() {
		r := startRollout(nil)
		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, nn, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(1))
		Expect(pdb.Spec.Selector.MatchLabels).To(Equal(map[string]string{"app": "riak", "cluster": clusterName}))

		c := getCluster()
		off := false
		c.Spec.PodDisruptionBudget = &riakv1.PodDisruptionBudgetConfig{Enabled: &off}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(r)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, nn, &policyv1.PodDisruptionBudget{}))).To(BeTrue())
	})
})
