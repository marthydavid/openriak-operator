/*
Copyright 2026.

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

// Package manifests guards properties of the shipped manifests that are easy to
// regress and expensive to find at runtime.
package manifests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// Floors for the operator's default resources, see
// https://github.com/marthydavid/openriak-operator/issues/48: at 128Mi the
// manager was OOMKilled in a loop while resyncing 9 nodes / 60 users / 60
// buckets (peak ~180Mi), which stalled the whole fleet.
var (
	minMemoryLimit   = resource.MustParse("512Mi")
	minMemoryRequest = resource.MustParse("128Mi")
)

// kustomizeManagerResources returns the manager container's resources from
// config/manager/manager.yaml.
func kustomizeManagerResources(t *testing.T) corev1.ResourceRequirements {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "manager", "manager.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		dep := &appsv1.Deployment{}
		if err := yaml.Unmarshal([]byte(doc), dep); err != nil || dep.Kind != "Deployment" {
			continue
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == "manager" {
				return c.Resources
			}
		}
	}
	t.Fatal("no manager container found in config/manager/manager.yaml")
	return corev1.ResourceRequirements{}
}

// chartResources returns .resources from the Helm chart's values.yaml.
func chartResources(t *testing.T) corev1.ResourceRequirements {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "openriak-operator", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Resources corev1.ResourceRequirements `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	return values.Resources
}

func TestOperatorMemoryDefaults(t *testing.T) {
	sources := map[string]corev1.ResourceRequirements{
		"config/manager/manager.yaml":          kustomizeManagerResources(t),
		"charts/openriak-operator/values.yaml": chartResources(t),
	}
	for name, res := range sources {
		limit, request := res.Limits[corev1.ResourceMemory], res.Requests[corev1.ResourceMemory]
		if limit.Cmp(minMemoryLimit) < 0 {
			t.Errorf("%s: memory limit %s is below %s; the operator is OOMKilled while resyncing a small fleet (issue #48)",
				name, limit.String(), minMemoryLimit.String())
		}
		if request.Cmp(minMemoryRequest) < 0 {
			t.Errorf("%s: memory request %s is below %s", name, request.String(), minMemoryRequest.String())
		}
		if request.Cmp(limit) > 0 {
			t.Errorf("%s: memory request %s exceeds the limit %s", name, request.String(), limit.String())
		}
	}
}

// The kustomize manifest and the chart are two ways to install the same
// operator; their defaults must not drift apart.
func TestOperatorResourceDefaultsMatchBetweenManifestAndChart(t *testing.T) {
	m, c := kustomizeManagerResources(t), chartResources(t)
	for _, name := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceCPU} {
		ml, cl := m.Limits[name], c.Limits[name]
		if !ml.Equal(cl) {
			t.Errorf("%s limit differs: manifest %s, chart %s", name, ml.String(), cl.String())
		}
		mr, cr := m.Requests[name], c.Requests[name]
		if !mr.Equal(cr) {
			t.Errorf("%s request differs: manifest %s, chart %s", name, mr.String(), cr.String())
		}
	}
}
