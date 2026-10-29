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
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/internal/riak"
)

// Rolling restart of the Riak nodes.
//
// The StatefulSet's own rolling update moves on as soon as a restarted pod is Ready, which for
// Riak only means the protobuf port accepts connections: handoff to the node that just came back
// may still be running, so restarting the next node would take a second replica of some
// partitions away. Like Strimzi (brokers only restart when partitions are in sync) and CNPG (one
// instance at a time, only when the others are healthy), the operator therefore drives the
// rollout itself:
//
//   - the StatefulSet's rollingUpdate.partition is held at spec.size, so a template change
//     restarts nothing on its own (stsPartition);
//   - reconcileRollout lowers it by one at a time, highest ordinal first (the seed <name>-0 is
//     restarted last), and only when the cluster is settled (clusterSettled).
const (
	conditionUpdateInProgress = "UpdateInProgress"
	conditionRolloutBlocked   = "RolloutBlocked"
	conditionRolloutStalled   = "RolloutStalled"

	statefulSetRevisionLabel = "controller-revision-hash"
	defaultRolloutStall      = 30 * time.Minute
)

// rolloutPod is what the rollout needs to know about one Riak pod.
type rolloutPod struct {
	ordinal  int32
	name     string
	revision string
	ready    bool
}

// rolloutInProgress reports whether the StatefulSet has a pending revision it has not finished
// rolling out. Both revisions are empty until the controller has seen the object once.
func rolloutInProgress(sts *appsv1.StatefulSet) bool {
	return sts.Status.UpdateRevision != "" && sts.Status.UpdateRevision != sts.Status.CurrentRevision
}

// livePartition is the partition currently set on an existing StatefulSet (0 when unset).
func livePartition(sts *appsv1.StatefulSet) int32 {
	if ru := sts.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		return *ru.Partition
	}
	return 0
}

// stsPartition is the partition the StatefulSet spec is written with on every reconcile.
//
// A new StatefulSet has no pods to protect (0). For an existing one the partition is held at
// the replica count, so the pod template can change without a single pod restarting, and only
// reconcileRollout lowers it. While a rollout is in progress the live value is kept, so the
// reconcile that re-applies the spec does not undo the operator's progress. It is clamped to
// the replica count, which also covers a scale-down mid-rollout.
func stsPartition(existing *appsv1.StatefulSet, replicas int32) int32 {
	if existing.UID == "" {
		return 0
	}
	if rolloutInProgress(existing) {
		return min(livePartition(existing), replicas)
	}
	return replicas
}

// rolloutNext decides the next partition. It returns partition unchanged with a reason while
// the pods already updated (ordinal >= partition) are not all at the update revision and Ready;
// otherwise partition-1, which restarts that ordinal. At partition 0 there is nothing left to
// step: the StatefulSet controller finishes the last pod by itself.
func rolloutNext(partition int32, replicas int32, updateRevision string, pods []rolloutPod) (int32, string) {
	if partition <= 0 {
		return 0, ""
	}
	byOrdinal := make(map[int32]rolloutPod, len(pods))
	for _, p := range pods {
		byOrdinal[p.ordinal] = p
	}
	for ord := partition; ord < replicas; ord++ {
		p, ok := byOrdinal[ord]
		switch {
		case !ok:
			return partition, fmt.Sprintf("pod %d is not created yet", ord)
		case p.revision != updateRevision:
			return partition, fmt.Sprintf("pod %s is not yet on the new revision", p.name)
		case !p.ready:
			return partition, fmt.Sprintf("pod %s is not Ready", p.name)
		}
	}
	return partition - 1, ""
}

func podOrdinal(name string) (int32, bool) {
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(name[i+1:], 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// reconcileRollout moves a pending pod-template change forward by one node when the cluster is
// settled, and reports where it stands in the UpdateInProgress, RolloutBlocked and
// RolloutStalled conditions. Errors are not fatal for the reconcile: it is retried on the next
// requeue, and nothing restarts while it fails.
func (r *RiakClusterReconciler) reconcileRollout(ctx context.Context, cluster *riakv1.RiakCluster) error {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}, sts); err != nil {
		return client.IgnoreNotFound(err)
	}
	gen := cluster.Generation
	if !rolloutInProgress(sts) {
		clearRolloutConditions(cluster)
		return nil
	}

	replicas := cluster.Spec.Size
	partition := livePartition(sts)
	pods, err := r.rolloutPods(ctx, cluster)
	if err != nil {
		return err
	}

	next, blocked := rolloutNext(partition, replicas, sts.Status.UpdateRevision, pods)
	if partition <= 0 {
		setCondition(&cluster.Status.Conditions, conditionUpdateInProgress, true, gen, "Finishing",
			"the last node is restarting on the new revision")
		clearBlocked(cluster)
		return nil
	}
	if cluster.Spec.Rollout != nil && cluster.Spec.Rollout.Paused {
		r.markBlocked(cluster, "Paused", "the rollout is paused (spec.rollout.paused)", false)
		return nil
	}
	if blocked == "" {
		if settled, why := r.clusterSettled(ctx, cluster, pods); !settled {
			blocked = why
		}
	}
	if blocked != "" {
		r.markBlocked(cluster, "WaitingForCluster", blocked, true)
		return nil
	}

	patch := client.MergeFrom(sts.DeepCopy())
	sts.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: &next}
	if err := r.Patch(ctx, sts, patch); err != nil {
		return err
	}
	log.FromContext(ctx).Info("rolling update: restarting next Riak node", "cluster", cluster.Name,
		"pod", fmt.Sprintf("%s-%d", cluster.Name, next), "partition", next)
	setCondition(&cluster.Status.Conditions, conditionUpdateInProgress, true, gen, "Restarting",
		fmt.Sprintf("restarting %s-%d (%d of %d nodes still on the old revision)", cluster.Name, next, next+1, replicas))
	clearBlocked(cluster)
	return nil
}

// rolloutPods lists the cluster's pods with their ordinal, revision and readiness.
func (r *RiakClusterReconciler) rolloutPods(ctx context.Context, cluster *riakv1.RiakCluster) ([]rolloutPod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"app": "riak", "cluster": cluster.Name}); err != nil {
		return nil, err
	}
	out := make([]rolloutPod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		ord, ok := podOrdinal(pod.Name)
		if !ok || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		out = append(out, rolloutPod{ordinal: ord, name: pod.Name,
			revision: pod.Labels[statefulSetRevisionLabel], ready: podIsReady(pod)})
	}
	return out, nil
}

// clusterSettled reports whether it is safe to restart another node: every pod is Ready, every
// node is a valid member of one ring and (unless spec.rollout.waitForTransfers is false) no
// handoff or ownership transfer is running. The reason names what is being waited for.
func (r *RiakClusterReconciler) clusterSettled(ctx context.Context, cluster *riakv1.RiakCluster, pods []rolloutPod) (bool, string) {
	if int32(len(pods)) < cluster.Spec.Size {
		return false, fmt.Sprintf("%d of %d pods exist", len(pods), cluster.Spec.Size)
	}
	probe := ""
	for _, p := range pods {
		if !p.ready {
			return false, fmt.Sprintf("pod %s is not Ready", p.name)
		}
		if probe == "" || p.ordinal < podOrdinalOrMax(probe) {
			probe = p.name
		}
	}
	if cluster.Spec.Size == 1 || probe == "" {
		return true, ""
	}

	executor := r.Executor
	if executor == nil {
		executor = riak.NewExecutor(log.FromContext(ctx))
	}
	members, err := executor.MemberStatus(ctx, cluster.Namespace, probe, "riak")
	if err != nil {
		return false, fmt.Sprintf("member-status on %s failed: %v", probe, err)
	}
	valid := int32(0)
	for _, m := range members {
		if m.Status == "valid" {
			valid++
		}
	}
	if valid != cluster.Spec.Size || int32(len(members)) != cluster.Spec.Size {
		return false, fmt.Sprintf("%d of %d nodes are valid ring members", valid, cluster.Spec.Size)
	}

	if cluster.Spec.Rollout == nil || cluster.Spec.Rollout.WaitForTransfers == nil || *cluster.Spec.Rollout.WaitForTransfers {
		idle, detail, err := executor.TransfersIdle(ctx, cluster.Namespace, probe, "riak")
		if err != nil {
			return false, fmt.Sprintf("riak-admin transfers on %s failed: %v", probe, err)
		}
		if !idle {
			return false, "waiting for handoff and ownership transfers to finish: " + detail
		}
	}
	return true, ""
}

func podOrdinalOrMax(name string) int32 {
	if o, ok := podOrdinal(name); ok {
		return o
	}
	return 1<<31 - 1
}

// markBlocked records why the rollout is waiting and, once it has waited longer than
// spec.rollout.stallTimeout, raises RolloutStalled. It never forces the restart.
func (r *RiakClusterReconciler) markBlocked(cluster *riakv1.RiakCluster, reason, message string, canStall bool) {
	gen := cluster.Generation
	setCondition(&cluster.Status.Conditions, conditionUpdateInProgress, true, gen, "Waiting", message)
	setCondition(&cluster.Status.Conditions, conditionRolloutBlocked, true, gen, reason, message)
	stalled := false
	if canStall {
		if c := meta.FindStatusCondition(cluster.Status.Conditions, conditionRolloutBlocked); c != nil {
			timeout := defaultRolloutStall
			if cluster.Spec.Rollout != nil && cluster.Spec.Rollout.StallTimeout != nil {
				timeout = cluster.Spec.Rollout.StallTimeout.Duration
			}
			stalled = time.Since(c.LastTransitionTime.Time) > timeout
		}
	}
	if stalled {
		setCondition(&cluster.Status.Conditions, conditionRolloutStalled, true, gen, "StallTimeout",
			"the rollout has been waiting longer than spec.rollout.stallTimeout: "+message)
	} else {
		setCondition(&cluster.Status.Conditions, conditionRolloutStalled, false, gen, "Progressing", "")
	}
}

func clearBlocked(cluster *riakv1.RiakCluster) {
	meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionRolloutBlocked)
	meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionRolloutStalled)
}

func clearRolloutConditions(cluster *riakv1.RiakCluster) {
	meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionUpdateInProgress)
	clearBlocked(cluster)
}

// reconcilePodDisruptionBudget keeps a PodDisruptionBudget allowing one unavailable Riak pod,
// so a node drain or a cluster upgrade cannot take two Riak nodes down together. It is removed
// when spec.podDisruptionBudget.enabled is false.
func (r *RiakClusterReconciler) reconcilePodDisruptionBudget(ctx context.Context, cluster *riakv1.RiakCluster) error {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace},
	}
	enabled := cluster.Spec.PodDisruptionBudget == nil || cluster.Spec.PodDisruptionBudget.Enabled == nil ||
		*cluster.Spec.PodDisruptionBudget.Enabled
	if !enabled {
		return client.IgnoreNotFound(r.Delete(ctx, pdb))
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		maxUnavailable := intstr.FromInt32(1)
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "riak", "cluster": cluster.Name},
		}
		return controllerutil.SetControllerReference(cluster, pdb, r.Scheme)
	})
	return err
}
