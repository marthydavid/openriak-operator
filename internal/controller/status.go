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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// Condition types reported on RiakCluster, RiakBucket and RiakUser status.
const (
	conditionReady            = "Ready"
	conditionStorageReady     = "StorageReady"
	conditionCertificateReady = "CertificateReady"
)

// setCondition upserts a condition, leaving LastTransitionTime untouched when the
// status has not flipped (meta.SetStatusCondition's behaviour), so a status that
// is rewritten on every reconcile does not fabricate transitions. It reports
// whether the condition changed, which callers use to skip needless status writes
// while a resource sits in the same state across requeues.
func setCondition(conds *[]metav1.Condition, condType string, ok bool, generation int64, reason, message string) bool {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	return meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
	})
}

// podIsReady reports whether the pod's Ready condition is True.
func podIsReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podHealth summarises a Riak node pod's serving state for status reporting.
func podHealth(pod *corev1.Pod, ready bool) string {
	switch {
	case ready:
		return riakv1.NodeHealthy
	case pod.Status.Phase == "" || pod.Status.Phase == corev1.PodUnknown:
		return riakv1.NodeHealthUnknown
	default:
		return riakv1.NodeUnhealthy
	}
}

// containerReady reports whether the named container in the pod is ready, plus a
// human-readable reason when it is not.
func containerReady(pod *corev1.Pod, name string) (bool, string) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != name {
			continue
		}
		if cs.Ready {
			return true, ""
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return false, cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return false, cs.State.Terminated.Reason
		}
		return false, "container is not ready"
	}
	return false, "container " + name + " not found in pod " + pod.Name
}

// certificateReady reads the Ready condition of a cert-manager Certificate held as
// an unstructured object. The second return value carries the reason it is not
// ready, and is empty when it is.
func certificateReady(cert *unstructured.Unstructured) (bool, string) {
	conds, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if err != nil || !found {
		return false, "certificate has not been issued yet"
	}
	for _, raw := range conds {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if condType, _ := cond["type"].(string); condType != "Ready" {
			continue
		}
		if status, _ := cond["status"].(string); status == string(metav1.ConditionTrue) {
			return true, ""
		}
		if message, _ := cond["message"].(string); message != "" {
			return false, message
		}
		return false, "certificate is not ready"
	}
	return false, "certificate has not been issued yet"
}

// fetchCertificateReadiness looks up a cert-manager Certificate and reports whether
// it has been issued. A missing Certificate or a cluster without the cert-manager
// CRDs is reported as "not ready" with an explanatory message rather than an error:
// certificate provisioning is asynchronous and must not fail the reconcile.
func fetchCertificateReadiness(ctx context.Context, c client.Client, name, namespace string) (bool, string) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)

	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cert)
	switch {
	case apierrors.IsNotFound(err):
		return false, "cert-manager Certificate " + name + " does not exist yet"
	case meta.IsNoMatchError(err):
		return false, "cert-manager CRDs are not installed"
	case err != nil:
		return false, err.Error()
	}
	return certificateReady(cert)
}
