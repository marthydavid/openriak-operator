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
	"time"

	"github.com/marthydavid/openriak-operator/internal/riak"
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
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakbuckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakbuckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakbuckets/finalizers,verbs=update
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakclusters,verbs=get;list;watch

// Reconcile creates and manages Riak buckets in a cluster.
func (r *RiakBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	bucket := &riakv1.RiakBucket{}
	if err := r.Get(ctx, req.NamespacedName, bucket); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
		log.Error(err, "failed to get cluster", "cluster", bucket.Spec.ClusterName)
		bucket.Status.Phase = riakv1.BucketPhaseFailed
		bucket.Status.Error = fmt.Sprintf("cluster not found: %v", err)
		r.Status().Update(ctx, bucket)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Wait for cluster to be ready
	if cluster.Status.Phase != riakv1.PhaseReady {
		log.V(2).Info("cluster not ready yet", "cluster", cluster.Name, "phase", cluster.Status.Phase)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Create bucket
	executor := riak.NewExecutor(log)
	manager := riak.NewManager(executor, r.Client, log)

	bucketType := bucket.Spec.BucketType
	if bucketType == "" {
		bucketType = "default"
	}

	if err := manager.CreateBucketType(ctx, cluster, bucketType, bucket.Spec.Properties); err != nil {
		log.Error(err, "failed to create bucket", "bucket", bucket.Spec.BucketName)
		bucket.Status.Phase = riakv1.BucketPhaseFailed
		bucket.Status.Error = fmt.Sprintf("failed to create bucket: %v", err)
		bucket.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
		r.Status().Update(ctx, bucket)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	bucket.Status.Phase = riakv1.BucketPhaseReady
	bucket.Status.Created = true
	bucket.Status.Error = ""
	bucket.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}

	if err := r.Status().Update(ctx, bucket); err != nil {
		log.Error(err, "failed to update bucket status")
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RiakBucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&riakv1.RiakBucket{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("riakbucket").
		Complete(r)
}
