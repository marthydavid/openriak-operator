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
	"time"

	"github.com/marthydavid/openriak-operator/internal/riak"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const riakBucketFinalizerName = "riak.openriak.io/bucket-finalizer"

// RiakBucketReconciler reconciles a RiakBucket object
type RiakBucketReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Executor *riak.Executor // if nil, a real executor is created per reconcile
}

// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakbuckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakbuckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakbuckets/finalizers,verbs=update
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create

// Reconcile creates and manages Riak buckets in a cluster.
func (r *RiakBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	bucket := &riakv1.RiakBucket{}
	if err := r.Get(ctx, req.NamespacedName, bucket); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !bucket.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(bucket, riakBucketFinalizerName) {
			controllerutil.RemoveFinalizer(bucket, riakBucketFinalizerName)
			if err := r.Update(ctx, bucket); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(bucket, riakBucketFinalizerName) {
		controllerutil.AddFinalizer(bucket, riakBucketFinalizerName)
		if err := r.Update(ctx, bucket); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status
	if bucket.Status.Phase == "" {
		bucket.Status.Phase = riakv1.BucketPhaseCreating
		if err := r.Status().Update(ctx, bucket); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get the cluster
	cluster := &riakv1.RiakCluster{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: bucket.Namespace,
		Name:      bucket.Spec.ClusterName,
	}, cluster); err != nil {
		// Only a genuinely absent cluster is the bucket's problem. Any other lookup
		// failure (API server unavailable, RBAC, timeout) is returned so
		// controller-runtime retries it with backoff, instead of reporting the
		// cluster as missing.
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		log.Error(err, "failed to get cluster", "cluster", bucket.Spec.ClusterName)
		r.failBucket(ctx, bucket, "ClusterNotFound", fmt.Sprintf("cluster not found: %v", err))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Wait for cluster to be ready
	if cluster.Status.Phase != riakv1.PhaseReady {
		log.V(2).Info("cluster not ready yet", "cluster", cluster.Name, "phase", cluster.Status.Phase)
		// Written only when the condition actually changes: this path requeues
		// every few seconds until the cluster comes up.
		if setCondition(&bucket.Status.Conditions, conditionReady, false, bucket.Generation,
			"ClusterNotReady", fmt.Sprintf("cluster %s is in phase %q", cluster.Name, cluster.Status.Phase)) {
			if err := r.Status().Update(ctx, bucket); err != nil {
				log.Error(err, "failed to update bucket status")
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Create bucket
	executor := r.Executor
	if executor == nil {
		executor = riak.NewExecutor(log)
	}
	manager := riak.NewManager(executor, r.Client, log)

	bucketType := bucket.Spec.BucketType
	if bucketType == "" {
		bucketType = "default"
	}

	properties, nVal := effectiveBucketProperties(bucket.Spec)

	if err := manager.CreateBucketType(ctx, cluster, bucketType, properties); err != nil {
		log.Error(err, "failed to create bucket", "bucket", bucket.Spec.BucketName)
		r.failBucket(ctx, bucket, "CreateFailed", fmt.Sprintf("failed to create bucket: %v", err))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Record what was actually applied to Riak, not merely what was requested.
	bucket.Status.Phase = riakv1.BucketPhaseReady
	bucket.Status.Created = true
	bucket.Status.Error = ""
	bucket.Status.BucketName = bucket.Spec.BucketName
	bucket.Status.BucketType = bucketType
	bucket.Status.NVal = nVal
	bucket.Status.ReplicationFactor = nVal
	bucket.Status.Properties = properties
	bucket.Status.Nodes = bucketNodes(cluster)
	bucket.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
	setCondition(&bucket.Status.Conditions, conditionReady, true, bucket.Generation,
		"BucketCreated", fmt.Sprintf("bucket type %q is active on cluster %s", bucketType, cluster.Name))

	// The bucket exists in Riak now, so a failed status write must be retried:
	// returning the error requeues, and re-running the idempotent create is
	// cheaper than leaving the recorded state behind reality. This path does not
	// requeue on success, so it writes once per reconcile event rather than on a
	// timer.
	if err := r.Status().Update(ctx, bucket); err != nil {
		log.Error(err, "failed to update bucket status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// failBucket records a failure on the bucket: phase, error message and the Ready
// condition, written in one status update. Repeating the same failure across
// requeues writes nothing, so a bucket blocked on e.g. a missing cluster does not
// generate a status update every few seconds.
func (r *RiakBucketReconciler) failBucket(ctx context.Context, bucket *riakv1.RiakBucket, reason, message string) {
	same := bucket.Status.Phase == riakv1.BucketPhaseFailed && bucket.Status.Error == message

	bucket.Status.Phase = riakv1.BucketPhaseFailed
	bucket.Status.Error = message
	changed := setCondition(&bucket.Status.Conditions, conditionReady, false, bucket.Generation, reason, message)
	if same && !changed {
		return
	}
	bucket.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}

	if err := r.Status().Update(ctx, bucket); err != nil {
		log.FromContext(ctx).Error(err, "failed to update bucket status")
	}
}

// effectiveBucketProperties resolves the bucket-type properties the operator sends
// to Riak. spec.properties is the base; the typed fields are layered on top, since
// they are the validated, documented API (use spec.properties to set anything the
// CRD does not model). It also returns the n_val that ends up in that map — from
// the typed fields, or from spec.properties when only that supplies one — so
// status reports the value actually applied. It is 0 when nothing sets n_val and
// Riak's own default applies.
//
// allow_mult is only written when spec.allowMulti is true: the field is a plain
// bool, so "false" cannot be told apart from "unset", and writing it out would
// silently override a bucket type's own default. Set properties["allow_mult"] to
// force it off.
func effectiveBucketProperties(spec riakv1.RiakBucketSpec) (map[string]string, int32) {
	properties := make(map[string]string, len(spec.Properties)+2)
	for k, v := range spec.Properties {
		properties[k] = v
	}

	// Riak's replication factor is n_val; replicationFactor is the spelling used
	// when nVal is not given.
	nVal := spec.NVal
	if nVal == 0 {
		nVal = spec.ReplicationFactor
	}
	switch {
	case nVal > 0:
		properties["n_val"] = strconv.Itoa(int(nVal))
	default:
		// No typed value, so whatever spec.properties carries is what Riak gets.
		// Report it rather than leaving status.nVal at 0.
		if parsed, err := strconv.Atoi(properties["n_val"]); err == nil && parsed > 0 {
			nVal = int32(parsed)
		}
	}
	if spec.AllowMulti {
		properties["allow_mult"] = "true"
	}

	return properties, nVal
}

// bucketNodes maps the cluster's members to the nodes serving this bucket. Bucket
// types are cluster-wide in Riak, so every member serves it.
func bucketNodes(cluster *riakv1.RiakCluster) []riakv1.BucketNodeRef {
	nodes := make([]riakv1.BucketNodeRef, 0, len(cluster.Status.Members))
	for _, m := range cluster.Status.Members {
		nodes = append(nodes, riakv1.BucketNodeRef{
			Name:   m.Name,
			Pod:    m.Pod,
			Ready:  m.Ready,
			Health: m.Health,
		})
	}
	return nodes
}

// SetupWithManager sets up the controller with the Manager. maxConcurrent sets
// MaxConcurrentReconciles; values < 1 fall back to controller-runtime's default.
func (r *RiakBucketReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrent int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&riakv1.RiakBucket{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controllerOptions(maxConcurrent)).
		Named("riakbucket").
		Complete(r)
}
