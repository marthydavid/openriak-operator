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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const riakUserFinalizerName = "riak.openriak.io/user-finalizer"

// RiakUserReconciler reconciles a RiakUser object
type RiakUserReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Executor *riak.Executor // if nil, a real executor is created per reconcile
}

// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakusers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakusers/finalizers,verbs=update
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=riak.openriak.io,resources=riakclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reconcile creates and manages Riak users in a cluster.
func (r *RiakUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	user := &riakv1.RiakUser{}
	if err := r.Get(ctx, req.NamespacedName, user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !user.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(user, riakUserFinalizerName) {
			controllerutil.RemoveFinalizer(user, riakUserFinalizerName)
			if err := r.Update(ctx, user); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(user, riakUserFinalizerName) {
		controllerutil.AddFinalizer(user, riakUserFinalizerName)
		if err := r.Update(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status
	if user.Status.Phase == "" {
		user.Status.Phase = riakv1.UserPhaseCreating
		if err := r.Status().Update(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
	}

	// certificateRef is required by the CRD, but admission validation only
	// applies to new writes — RiakUsers stored before the field became required
	// can still lack it. Fail those explicitly instead of panicking, and do so
	// before any cluster dependency so the terminal state is reached even when
	// the referenced cluster is missing or not Ready. A failed status write is
	// returned so the update is retried rather than leaving the user Creating.
	if user.Spec.CertificateRef == nil {
		user.Status.Phase = riakv1.UserPhaseFailed
		user.Status.Error = "certificateRef is required: password authentication was removed; " +
			"recreate the user with spec.certificateRef"
		user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
		setCondition(&user.Status.Conditions, conditionReady, false, user.Generation,
			"CertificateRefMissing", user.Status.Error)
		if updateErr := r.Status().Update(ctx, user); updateErr != nil {
			log.Error(updateErr, "failed to update user status")
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	// Get the cluster
	cluster := &riakv1.RiakCluster{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: user.Namespace,
		Name:      user.Spec.ClusterName,
	}, cluster); err != nil {
		// Only a genuinely absent cluster is the user's problem. Any other lookup
		// failure (API server unavailable, RBAC, timeout) is returned so
		// controller-runtime retries it with backoff, instead of reporting the
		// cluster as missing.
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		log.Error(err, "failed to get cluster", "cluster", user.Spec.ClusterName)
		r.failUser(ctx, user, "ClusterNotFound", fmt.Sprintf("cluster not found: %v", err))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Wait for cluster to be ready
	if cluster.Status.Phase != riakv1.PhaseReady {
		log.V(2).Info("cluster not ready yet", "cluster", cluster.Name, "phase", cluster.Status.Phase)
		// Written only when the condition actually changes: this path requeues
		// every few seconds until the cluster comes up.
		if setCondition(&user.Status.Conditions, conditionReady, false, user.Generation,
			"ClusterNotReady", fmt.Sprintf("cluster %s is in phase %q", cluster.Name, cluster.Status.Phase)) {
			if err := r.Status().Update(ctx, user); err != nil {
				log.Error(err, "failed to update user status")
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	executor := r.Executor
	if executor == nil {
		executor = riak.NewExecutor(log)
	}
	manager := riak.NewManager(executor, r.Client, log)

	// Create the cert-manager Certificate for the user's client certificate.
	if err := r.reconcileUserCertificate(ctx, user); err != nil {
		log.Error(err, "failed to reconcile user certificate")
		user.Status.CertificateReady = false
		user.Status.CertificateError = err.Error()
		setCondition(&user.Status.Conditions, conditionCertificateReady, false, user.Generation,
			"CertificateRequestFailed", err.Error())
		r.failUser(ctx, user, "CertificateFailed", fmt.Sprintf("failed to reconcile certificate: %v", err))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Enable Riak security once per cluster, not per user. Repeatedly running
	// `riak-admin security enable` on a live node bounces its client listeners and
	// destabilises it under load, so guard it with the cluster's status flag.
	if !cluster.Status.SecurityEnabled {
		if err := manager.EnableSecurity(ctx, cluster); err != nil {
			log.Error(err, "failed to enable cluster security", "cluster", cluster.Name)
			r.failUser(ctx, user, "SecurityEnableFailed", fmt.Sprintf("failed to enable security: %v", err))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		cluster.Status.SecurityEnabled = true
		if err := r.Status().Update(ctx, cluster); err != nil {
			// The enable succeeded; only the flag write failed (e.g. a conflicting
			// cluster status update). Requeue to retry — EnableSecurity is idempotent,
			// so re-running it once on retry is harmless.
			log.Error(err, "failed to record cluster SecurityEnabled; requeuing")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if err := manager.CreateUserForCert(ctx, cluster, user.Spec.Username); err != nil {
		log.Error(err, "failed to create cert-auth user", "user", user.Spec.Username)
		r.failUser(ctx, user, "CreateUserFailed", fmt.Sprintf("failed to create user: %v", err))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := manager.AddSecuritySource(ctx, cluster, user.Spec.Username); err != nil {
		log.Error(err, "failed to set certificate security source", "user", user.Spec.Username)
		r.failUser(ctx, user, "SecuritySourceFailed", fmt.Sprintf("failed to set security source: %v", err))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Grant permissions, batched by target (one riak-admin call per distinct
	// resource/bucket rather than one per grant). A failed grant means the user
	// would silently lack the access the spec requested, so surface it as a
	// reconcile failure rather than reporting Ready.
	if err := manager.GrantUserPermissions(ctx, cluster, user.Spec.Username, user.Spec.Grants); err != nil {
		log.Error(err, "failed to grant permissions", "user", user.Spec.Username)
		r.failUser(ctx, user, "GrantFailed", fmt.Sprintf("failed to grant permissions: %v", err))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Certificate issuance is cert-manager's job and completes asynchronously, so
	// it is reported separately from the phase: Ready means the Riak-side identity
	// is provisioned, while certificateReady tells clients whether the client
	// certificate they authenticate with actually exists yet.
	certReady, certReason := fetchCertificateReadiness(ctx, r.Client, userCertName(user.Name), user.Namespace)

	// Snapshot what status already says: while the certificate is pending this
	// block runs every 30s, and an unchanged status must not be rewritten each
	// time.
	unchanged := user.Status.Phase == riakv1.UserPhaseReady &&
		user.Status.Created &&
		user.Status.Error == "" &&
		user.Status.Username == user.Spec.Username &&
		user.Status.ClusterName == cluster.Name &&
		user.Status.CertificateReady == certReady &&
		user.Status.CertificateError == certReason &&
		equalGrants(user.Status.Grants, user.Spec.Grants)

	user.Status.Phase = riakv1.UserPhaseReady
	user.Status.Created = true
	user.Status.Error = ""
	user.Status.Username = user.Spec.Username
	user.Status.ClusterName = cluster.Name
	user.Status.Grants = append([]riakv1.Grant(nil), user.Spec.Grants...)
	user.Status.CertificateReady = certReady
	user.Status.CertificateError = certReason

	var certChanged bool
	if certReady {
		certChanged = setCondition(&user.Status.Conditions, conditionCertificateReady, true, user.Generation,
			"CertificateIssued", "the client certificate has been issued")
	} else {
		certChanged = setCondition(&user.Status.Conditions, conditionCertificateReady, false, user.Generation,
			"CertificatePending", certReason)
	}
	readyChanged := setCondition(&user.Status.Conditions, conditionReady, true, user.Generation,
		"UserProvisioned", fmt.Sprintf("Riak user %q is provisioned on cluster %s", user.Spec.Username, cluster.Name))

	if !unchanged || certChanged || readyChanged {
		user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
		// The Riak-side identity exists now, so a failed status write is retried
		// rather than swallowed: every call above is idempotent.
		if err := r.Status().Update(ctx, user); err != nil {
			log.Error(err, "failed to update user status")
			return ctrl.Result{}, err
		}
	}

	// Requeue until the certificate is observed issued so status.certificateReady
	// converges; every Riak-side call above is idempotent, and the requeue stops
	// as soon as cert-manager reports the certificate Ready.
	if !certReady {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// equalGrants reports whether two grant lists are identical, element for element.
func equalGrants(a, b []riakv1.Grant) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// failUser records a failure on the user: phase, error message and the Ready
// condition, written in one status update. Repeating the same failure across
// requeues writes nothing, so a user blocked on e.g. a missing cluster does not
// generate a status update every few seconds.
func (r *RiakUserReconciler) failUser(ctx context.Context, user *riakv1.RiakUser, reason, message string) {
	same := user.Status.Phase == riakv1.UserPhaseFailed && user.Status.Error == message

	user.Status.Phase = riakv1.UserPhaseFailed
	user.Status.Error = message
	changed := setCondition(&user.Status.Conditions, conditionReady, false, user.Generation, reason, message)
	if same && !changed {
		return
	}
	user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}

	if err := r.Status().Update(ctx, user); err != nil {
		log.FromContext(ctx).Error(err, "failed to update user status")
	}
}

// SetupWithManager sets up the controller with the Manager. maxConcurrent sets
// MaxConcurrentReconciles; values < 1 fall back to controller-runtime's default.
func (r *RiakUserReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrent int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&riakv1.RiakUser{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controllerOptions(maxConcurrent)).
		Named("riakuser").
		Complete(r)
}

// reconcileUserCertificate creates a cert-manager Certificate for the RiakUser's client
// certificate when spec.certificateRef is set. It is idempotent: a second call does nothing
// if the Certificate already exists.
func (r *RiakUserReconciler) reconcileUserCertificate(ctx context.Context, user *riakv1.RiakUser) error {
	cert := buildUserCertificate(user.Name, user.Namespace, user.Spec.Username, user.Spec.CertificateRef)

	if err := controllerutil.SetControllerReference(user, cert, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference on user certificate: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   certManagerGroup,
		Version: certManagerVersion,
		Kind:    certManagerKind,
	})

	err := r.Get(ctx, client.ObjectKey{Name: cert.GetName(), Namespace: cert.GetNamespace()}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, cert)
	}
	return err
}
