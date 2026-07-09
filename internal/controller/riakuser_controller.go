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
	corev1 "k8s.io/api/core/v1"
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
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
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

	// Get the cluster
	cluster := &riakv1.RiakCluster{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: user.Namespace,
		Name:      user.Spec.ClusterName,
	}, cluster); err != nil {
		log.Error(err, "failed to get cluster", "cluster", user.Spec.ClusterName)
		user.Status.Phase = riakv1.UserPhaseFailed
		user.Status.Error = fmt.Sprintf("cluster not found: %v", err)
		if updateErr := r.Status().Update(ctx, user); updateErr != nil {
			log.Error(updateErr, "failed to update user status")
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Wait for cluster to be ready
	if cluster.Status.Phase != riakv1.PhaseReady {
		log.V(2).Info("cluster not ready yet", "cluster", cluster.Name, "phase", cluster.Status.Phase)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	executor := r.Executor
	if executor == nil {
		executor = riak.NewExecutor(log)
	}
	manager := riak.NewManager(executor, r.Client, log)

	if user.Spec.CertificateRef != nil {
		// --- Certificate-based authentication path ---
		// Create the cert-manager Certificate for the user's client certificate.
		if err := r.reconcileUserCertificate(ctx, user); err != nil {
			log.Error(err, "failed to reconcile user certificate")
			user.Status.Phase = riakv1.UserPhaseFailed
			user.Status.Error = fmt.Sprintf("failed to reconcile certificate: %v", err)
			user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
			if updateErr := r.Status().Update(ctx, user); updateErr != nil {
				log.Error(updateErr, "failed to update user status")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		if err := manager.CreateUserForCert(ctx, cluster, user.Spec.Username); err != nil {
			log.Error(err, "failed to create cert-auth user", "user", user.Spec.Username)
			user.Status.Phase = riakv1.UserPhaseFailed
			user.Status.Error = fmt.Sprintf("failed to create user: %v", err)
			user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
			if updateErr := r.Status().Update(ctx, user); updateErr != nil {
				log.Error(updateErr, "failed to update user status")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		if err := manager.AddSecuritySource(ctx, cluster, user.Spec.Username, "certificate"); err != nil {
			log.Error(err, "failed to set certificate security source", "user", user.Spec.Username)
			user.Status.Phase = riakv1.UserPhaseFailed
			user.Status.Error = fmt.Sprintf("failed to set security source: %v", err)
			user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
			if updateErr := r.Status().Update(ctx, user); updateErr != nil {
				log.Error(updateErr, "failed to update user status")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	} else {
		// --- Password-based authentication path ---
		password, err := r.resolvePassword(ctx, user)
		if err != nil {
			log.Error(err, "failed to resolve password")
			user.Status.Phase = riakv1.UserPhaseFailed
			user.Status.Error = err.Error()
			if updateErr := r.Status().Update(ctx, user); updateErr != nil {
				log.Error(updateErr, "failed to update user status")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		if err := manager.CreateUser(ctx, cluster, user.Spec.Username, password); err != nil {
			log.Error(err, "failed to create user", "user", user.Spec.Username)
			user.Status.Phase = riakv1.UserPhaseFailed
			user.Status.Error = fmt.Sprintf("failed to create user: %v", err)
			user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
			if updateErr := r.Status().Update(ctx, user); updateErr != nil {
				log.Error(updateErr, "failed to update user status")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// Grant permissions (applies to both auth paths)
	for _, grant := range user.Spec.Grants {
		if err := manager.GrantUserPermission(ctx, cluster, user.Spec.Username, grant.Resource, grant.Permission, grant.BucketName); err != nil {
			log.Error(err, "failed to grant permission", "user", user.Spec.Username, "permission", grant.Permission)
		}
	}

	user.Status.Phase = riakv1.UserPhaseReady
	user.Status.Created = true
	user.Status.Error = ""
	user.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}

	if err := r.Status().Update(ctx, user); err != nil {
		log.Error(err, "failed to update user status")
	}

	return ctrl.Result{}, nil
}

// resolvePassword returns the password for a password-authenticated user. When
// spec.passwordSecret is not set, the insecure default "changeme" is used.
func (r *RiakUserReconciler) resolvePassword(ctx context.Context, user *riakv1.RiakUser) (string, error) {
	if user.Spec.PasswordSecret == nil {
		return "changeme", nil // Default password
	}

	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: user.Namespace,
		Name:      user.Spec.PasswordSecret.Name,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return "", fmt.Errorf("password secret not found: %w", err)
	}

	key := user.Spec.PasswordSecret.Key
	if key == "" {
		key = "password"
	}

	pwd, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("password key not found in secret: %s", key)
	}
	return string(pwd), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RiakUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&riakv1.RiakUser{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
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
