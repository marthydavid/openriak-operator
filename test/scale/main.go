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

// Command scale is a load-test harness for the OpenRiak operator. It creates a
// configurable number of RiakClusters, each with a number of cert-auth
// RiakUsers and RiakBuckets, then measures how long the operator takes to drive
// them all to Ready. It reports convergence time, throughput, and any resources
// stuck in Failed — the signals that matter at fleet scale (dozens of clusters,
// hundreds of users/buckets).
//
// It talks to whatever cluster your kubeconfig points at; the operator, CRDs,
// cert-manager and a usable operand image must already be installed there. It
// does NOT stand up a cluster — point it at a realistic environment.
//
//	go run ./test/scale -clusters 50 -users 4 -buckets 4
//
// Defaults are small so it can smoke-run against a kind e2e cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

type opts struct {
	clusters  int
	users     int
	buckets   int
	namespace string
	image     string
	storage   string
	timeout   time.Duration
	poll      time.Duration
	keep      bool
	ephemeral bool
}

func main() {
	o := opts{}
	flag.IntVar(&o.clusters, "clusters", 3, "number of RiakClusters to create")
	flag.IntVar(&o.users, "users", 5, "cert-auth RiakUsers per cluster")
	flag.IntVar(&o.buckets, "buckets", 5, "RiakBuckets per cluster")
	flag.StringVar(&o.namespace, "namespace", "scale-test", "namespace to create resources in")
	flag.StringVar(&o.image, "image", "ghcr.io/marthydavid/riak:3.2.6", "Riak operand image")
	flag.StringVar(&o.storage, "storage-class", "standard", "storage class for cluster PVCs")
	flag.DurationVar(&o.timeout, "timeout", 20*time.Minute, "overall deadline for everything to reach Ready")
	flag.DurationVar(&o.poll, "poll", 5*time.Second, "status poll interval")
	flag.BoolVar(&o.keep, "keep", false, "keep resources after the run instead of deleting them")
	flag.BoolVar(&o.ephemeral, "ephemeral", false,
		"use emptyDir (spec.ephemeralStorage) instead of PVCs; for clusters without a storage provisioner")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "scale test failed:", err)
		os.Exit(1)
	}
}

func run(o opts) error {
	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	sch := scheme.Scheme
	if err := riakv1.AddToScheme(sch); err != nil {
		return fmt.Errorf("add riak scheme: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}
	ctx := context.Background()

	total := o.clusters + o.clusters*o.users + o.clusters*o.buckets
	fmt.Printf("Scale test: %d clusters × (%d users + %d buckets) = %d resources in ns/%s\n",
		o.clusters, o.users, o.buckets, total, o.namespace)

	if err := ensureNamespace(ctx, c, o.namespace); err != nil {
		return err
	}
	// Register teardown right after the namespace exists so an error in any
	// later setup step (e.g. a missing cert-manager Issuer) still cleans up.
	if !o.keep {
		defer teardown(c, o)
	}
	// Users authenticate by client certificate, so they need a cert-manager
	// Issuer; only require it (and cert-manager) when creating users.
	if o.users > 0 {
		if err := ensureIssuer(ctx, c, o.namespace); err != nil {
			return err
		}
	}

	start := time.Now()
	if err := createAll(ctx, c, o); err != nil {
		return err
	}
	fmt.Printf("Applied %d resources in %s; waiting for Ready (deadline %s)...\n",
		total, time.Since(start).Round(time.Millisecond), o.timeout)

	return waitReady(ctx, c, o, start)
}

func ensureNamespace(ctx context.Context, c client.Client, ns string) error {
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := c.Create(ctx, n); err != nil && !apiAlreadyExists(err) {
		return fmt.Errorf("create namespace: %w", err)
	}
	return nil
}

// ensureIssuer creates a self-signed cert-manager Issuer used by every
// cert-auth RiakUser in the run.
func ensureIssuer(ctx context.Context, c client.Client, ns string) error {
	iss := &unstructured.Unstructured{}
	iss.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cert-manager.io", Version: "v1", Kind: "Issuer"})
	iss.SetName("scale-issuer")
	iss.SetNamespace(ns)
	iss.Object["spec"] = map[string]interface{}{"selfSigned": map[string]interface{}{}}
	if err := c.Create(ctx, iss); err != nil && !apiAlreadyExists(err) {
		return fmt.Errorf("create issuer: %w", err)
	}
	return nil
}

func createAll(ctx context.Context, c client.Client, o opts) error {
	for i := 0; i < o.clusters; i++ {
		cl := fmt.Sprintf("scale-c%03d", i)
		size := int32(1)
		spec := riakv1.RiakClusterSpec{
			Size:       size,
			Image:      o.image,
			RiakConfig: map[string]string{"ring_size": "8"},
		}
		if o.ephemeral {
			spec.EphemeralStorage = true
		} else {
			spec.StorageClassName = o.storage
		}
		cluster := &riakv1.RiakCluster{
			ObjectMeta: metav1.ObjectMeta{Name: cl, Namespace: o.namespace},
			Spec:       spec,
		}
		if err := c.Create(ctx, cluster); err != nil && !apiAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", cl, err)
		}
		for u := 0; u < o.users; u++ {
			user := &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-u%03d", cl, u), Namespace: o.namespace},
				Spec: riakv1.RiakUserSpec{
					ClusterName: cl,
					Username:    fmt.Sprintf("%s_u%03d", cl, u),
					CertificateRef: &riakv1.UserCertificateRef{
						IssuerRef: riakv1.CertIssuerRef{Name: "scale-issuer", Kind: "Issuer"},
					},
					Grants: []riakv1.Grant{{Resource: "any", Permission: "read"}},
				},
			}
			if err := c.Create(ctx, user); err != nil && !apiAlreadyExists(err) {
				return fmt.Errorf("create user: %w", err)
			}
		}
		for b := 0; b < o.buckets; b++ {
			bucket := &riakv1.RiakBucket{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-b%03d", cl, b), Namespace: o.namespace},
				Spec: riakv1.RiakBucketSpec{
					ClusterName: cl,
					BucketName:  fmt.Sprintf("bucket-%03d", b),
					BucketType:  fmt.Sprintf("%s-t%03d", cl, b),
				},
			}
			if err := c.Create(ctx, bucket); err != nil && !apiAlreadyExists(err) {
				return fmt.Errorf("create bucket: %w", err)
			}
		}
	}
	return nil
}

// waitReady polls every resource kind until all are Ready or the deadline hits,
// recording the wall-clock time each kind fully converged.
func waitReady(ctx context.Context, c client.Client, o opts, start time.Time) error {
	deadline := start.Add(o.timeout)
	var clustersReady, usersReady, bucketsReady time.Duration
	wantC, wantU, wantB := o.clusters, o.clusters*o.users, o.clusters*o.buckets

	for {
		nc, fc := countPhase(ctx, c, o.namespace, "RiakClusterList")
		nu, fu := countPhase(ctx, c, o.namespace, "RiakUserList")
		nb, fb := countPhase(ctx, c, o.namespace, "RiakBucketList")

		if clustersReady == 0 && nc >= wantC {
			clustersReady = time.Since(start)
		}
		if usersReady == 0 && nu >= wantU {
			usersReady = time.Since(start)
		}
		if bucketsReady == 0 && nb >= wantB {
			bucketsReady = time.Since(start)
		}

		fmt.Printf("  [%6s] clusters %d/%d (fail %d)  users %d/%d (fail %d)  buckets %d/%d (fail %d)\n",
			time.Since(start).Round(time.Second), nc, wantC, fc, nu, wantU, fu, nb, wantB, fb)

		if nc >= wantC && nu >= wantU && nb >= wantB {
			report(o, clustersReady, usersReady, bucketsReady, start)
			return nil
		}
		if time.Now().After(deadline) {
			report(o, clustersReady, usersReady, bucketsReady, start)
			return fmt.Errorf("deadline exceeded: clusters %d/%d users %d/%d buckets %d/%d (failed: %d/%d/%d)",
				nc, wantC, nu, wantU, nb, wantB, fc, fu, fb)
		}
		time.Sleep(o.poll)
	}
}

// countPhase lists resources of the given kind and returns (readyCount, failedCount).
// A List error is surfaced to stderr rather than swallowed: for a diagnostic
// harness, "the API is unreachable" must not look like "nothing is Ready yet".
func countPhase(ctx context.Context, c client.Client, ns, listKind string) (ready, failed int) {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "riak.openriak.io", Version: "v1", Kind: listKind})
	if err := c.List(ctx, l, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: listing %s failed: %v\n", listKind, err)
		return 0, 0
	}
	for _, item := range l.Items {
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		switch phase {
		case "Ready":
			ready++
		case "Failed":
			failed++
		}
	}
	return ready, failed
}

func report(o opts, clusters, users, buckets time.Duration, start time.Time) {
	fmt.Println("\n──────── scale test results ────────")
	fmt.Printf("resources:   %d clusters, %d users, %d buckets\n",
		o.clusters, o.clusters*o.users, o.clusters*o.buckets)
	line := func(label string, d time.Duration, n int) {
		if d == 0 {
			fmt.Printf("%-18s NOT CONVERGED within %s\n", label, o.timeout)
			return
		}
		fmt.Printf("%-18s %s  (%.1f/s)\n", label, d.Round(time.Second), float64(n)/d.Seconds())
	}
	line("clusters Ready:", clusters, o.clusters)
	line("users Ready:", users, o.clusters*o.users)
	line("buckets Ready:", buckets, o.clusters*o.buckets)
	fmt.Printf("total wall clock:  %s\n", time.Since(start).Round(time.Second))
	fmt.Println("Tip: scrape the operator's Prometheus /metrics for reconcile latency")
	fmt.Println("(controller_runtime_reconcile_time_seconds) and workqueue depth.")
	fmt.Println("────────────────────────────────────")
}

func teardown(c client.Client, o opts) {
	ctx := context.Background()
	fmt.Println("Tearing down (use -keep to skip)...")
	for _, k := range []string{"RiakUserList", "RiakBucketList", "RiakClusterList"} {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(schema.GroupVersionKind{Group: "riak.openriak.io", Version: "v1", Kind: k})
		_ = c.List(ctx, l, client.InNamespace(o.namespace))
		for i := range l.Items {
			_ = c.Delete(ctx, &l.Items[i])
		}
	}
	iss := &unstructured.Unstructured{}
	iss.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Issuer"})
	iss.SetName("scale-issuer")
	iss.SetNamespace(o.namespace)
	_ = c.Delete(ctx, iss)
	_ = c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: o.namespace}})
}

func apiAlreadyExists(err error) bool {
	return apierrors.IsAlreadyExists(err)
}
