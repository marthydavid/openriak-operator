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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// TestReconcileServiceMonitor_ownerRefError covers the SetControllerReference
// failure branch: with a scheme that does not know the RiakCluster type, the
// owner reference cannot be set and the reconcile must surface that error
// rather than attempting to create the ServiceMonitor.
func TestReconcileServiceMonitor_ownerRefError(t *testing.T) {
	// Scheme deliberately missing riakv1, so SetControllerReference fails.
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &RiakClusterReconciler{Client: c, Scheme: scheme}
	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
	}

	// CreateOrUpdate runs the mutate (which sets the owner reference) and returns
	// its error; with RiakCluster absent from the scheme the owner reference
	// cannot be resolved, so the reconcile fails rather than writing the object.
	err := r.reconcileServiceMonitor(context.Background(), cluster)
	if err == nil || !strings.Contains(err.Error(), "RiakCluster") {
		t.Fatalf("expected owner-reference error mentioning RiakCluster, got %v", err)
	}
}
