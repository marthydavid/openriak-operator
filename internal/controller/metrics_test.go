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
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

func metricsTestObjects() []client.Object {
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	return []client.Object{
		&riakv1.RiakCluster{
			ObjectMeta: meta("ns1", "c1"),
			Status: riakv1.RiakClusterStatus{
				Phase: riakv1.PhaseReady, ReadyNodes: 3, TotalNodes: 3,
				TLSStatus:        riakv1.TLSStatus{Enabled: true, ClientReady: true},
				MonitoringStatus: riakv1.MonitoringStatus{Enabled: true, ExporterReady: true},
			},
		},
		&riakv1.RiakCluster{ObjectMeta: meta("ns1", "c2")}, // no status yet
		&riakv1.RiakBucket{
			ObjectMeta: meta("ns1", "b1"), Spec: riakv1.RiakBucketSpec{ClusterName: "c1"},
			Status: riakv1.RiakBucketStatus{Phase: riakv1.BucketPhaseReady},
		},
		&riakv1.RiakBucket{
			ObjectMeta: meta("ns1", "b2"), Spec: riakv1.RiakBucketSpec{ClusterName: "c1"},
			Status: riakv1.RiakBucketStatus{Phase: riakv1.BucketPhaseReady},
		},
		&riakv1.RiakBucket{ObjectMeta: meta("ns1", "b3"), Spec: riakv1.RiakBucketSpec{ClusterName: "c1"}},
		&riakv1.RiakUser{
			ObjectMeta: meta("ns1", "u1"), Spec: riakv1.RiakUserSpec{ClusterName: "c1"},
			Status: riakv1.RiakUserStatus{Phase: riakv1.UserPhaseReady, CertificateReady: true},
		},
		&riakv1.RiakUser{
			ObjectMeta: meta("ns1", "u2"), Spec: riakv1.RiakUserSpec{ClusterName: "c1"},
			Status: riakv1.RiakUserStatus{Phase: riakv1.UserPhaseFailed},
		},
	}
}

func metricsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := riakv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCRStateCollector(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(metricsTestScheme(t)).WithObjects(metricsTestObjects()...).Build()
	col := NewCRStateCollector(c, logr.Discard())

	want := `
# HELP openriak_riakcluster_phase 1 for the current phase of a RiakCluster, 0 for the other phases.
# TYPE openriak_riakcluster_phase gauge
openriak_riakcluster_phase{name="c1",namespace="ns1",phase="Creating"} 0
openriak_riakcluster_phase{name="c1",namespace="ns1",phase="Failed"} 0
openriak_riakcluster_phase{name="c1",namespace="ns1",phase="Ready"} 1
openriak_riakcluster_phase{name="c1",namespace="ns1",phase="Updating"} 0
openriak_riakcluster_phase{name="c2",namespace="ns1",phase="Creating"} 0
openriak_riakcluster_phase{name="c2",namespace="ns1",phase="Failed"} 0
openriak_riakcluster_phase{name="c2",namespace="ns1",phase="Ready"} 0
openriak_riakcluster_phase{name="c2",namespace="ns1",phase="Updating"} 0
# HELP openriak_riakcluster_nodes Number of RiakCluster nodes by state (ready, total).
# TYPE openriak_riakcluster_nodes gauge
openriak_riakcluster_nodes{name="c1",namespace="ns1",state="ready"} 3
openriak_riakcluster_nodes{name="c1",namespace="ns1",state="total"} 3
openriak_riakcluster_nodes{name="c2",namespace="ns1",state="ready"} 0
openriak_riakcluster_nodes{name="c2",namespace="ns1",state="total"} 0
# HELP openriak_riakcluster_monitoring_ready 1 when spec.monitoring is enabled and every node's exporter sidecar is ready.
# TYPE openriak_riakcluster_monitoring_ready gauge
openriak_riakcluster_monitoring_ready{name="c1",namespace="ns1"} 1
openriak_riakcluster_monitoring_ready{name="c2",namespace="ns1"} 0
# HELP openriak_riakcluster_tls_ready 1 when TLS is enabled and client TLS is ready.
# TYPE openriak_riakcluster_tls_ready gauge
openriak_riakcluster_tls_ready{name="c1",namespace="ns1"} 1
openriak_riakcluster_tls_ready{name="c2",namespace="ns1"} 0
# HELP openriak_riakbuckets Number of RiakBuckets by namespace, target cluster and phase.
# TYPE openriak_riakbuckets gauge
openriak_riakbuckets{cluster="c1",namespace="ns1",phase="Pending"} 1
openriak_riakbuckets{cluster="c1",namespace="ns1",phase="Ready"} 2
# HELP openriak_riakusers Number of RiakUsers by namespace, target cluster and phase.
# TYPE openriak_riakusers gauge
openriak_riakusers{cluster="c1",namespace="ns1",phase="Failed"} 1
openriak_riakusers{cluster="c1",namespace="ns1",phase="Ready"} 1
# HELP openriak_riakuser_certificates Number of RiakUsers by namespace, target cluster and whether the client certificate is issued.
# TYPE openriak_riakuser_certificates gauge
openriak_riakuser_certificates{cluster="c1",namespace="ns1",ready="false"} 1
openriak_riakuser_certificates{cluster="c1",namespace="ns1",ready="true"} 1
# HELP openriak_resources Number of custom resources by kind and phase, across all namespaces.
# TYPE openriak_resources gauge
openriak_resources{kind="RiakBucket",phase="Creating"} 0
openriak_resources{kind="RiakBucket",phase="Failed"} 0
openriak_resources{kind="RiakBucket",phase="Pending"} 1
openriak_resources{kind="RiakBucket",phase="Ready"} 2
openriak_resources{kind="RiakCluster",phase="Creating"} 0
openriak_resources{kind="RiakCluster",phase="Failed"} 0
openriak_resources{kind="RiakCluster",phase="Pending"} 1
openriak_resources{kind="RiakCluster",phase="Ready"} 1
openriak_resources{kind="RiakCluster",phase="Updating"} 0
openriak_resources{kind="RiakUser",phase="Creating"} 0
openriak_resources{kind="RiakUser",phase="Failed"} 1
openriak_resources{kind="RiakUser",phase="Ready"} 1
`
	if err := testutil.CollectAndCompare(col, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestCRStateCollectorEmptyAndLint(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(metricsTestScheme(t)).Build()
	col := NewCRStateCollector(c, logr.Discard())
	problems, err := testutil.CollectAndLint(col)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("lint problems: %v", problems)
	}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(col); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
}

type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("boom")
}

func TestCRStateCollectorListErrors(t *testing.T) {
	col := NewCRStateCollector(failingReader{}, logr.Discard())
	if n := testutil.CollectAndCount(col); n != 0 {
		t.Fatalf("expected no series on list errors, got %d", n)
	}
}

func TestRegisterCRStateMetrics(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(metricsTestScheme(t)).Build()
	if err := RegisterCRStateMetrics(c, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	// A second registration of the same descriptors must be rejected, not panic.
	if err := RegisterCRStateMetrics(c, logr.Discard()); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}
