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
	"math/rand"
	"os"
	"strconv"
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
	clusters   int
	users      int
	buckets    int
	namespace  string
	image      string
	storage    string
	timeout    time.Duration
	poll       time.Duration
	keep       bool
	ephemeral  bool
	replicas   int
	monitoring bool
	scrapeKind string

	verify      bool
	verifyOnly  bool
	mutate      bool
	deleteEvery int

	ringSize int

	operatorNamespace string

	stress          bool
	stressDuration  time.Duration
	stressThreads   int
	stressClients   int
	stressValueSize int
	stressReadRatio float64
	stressMaxErrors int64
	stressImage     string
	verifyWorkers   int
	verifyTimeout   time.Duration
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
	flag.IntVar(&o.replicas, "replicas", 1, "Riak nodes per RiakCluster (spec.size)")
	flag.BoolVar(&o.monitoring, "monitoring", false,
		"enable spec.monitoring (json_exporter sidecar) on every cluster and verify the riak_* metrics on every node")
	flag.StringVar(&o.scrapeKind, "scrape-kind", "",
		"with -monitoring: spec.monitoring.scrapeKind (PodMonitor, ServiceMonitor, None); default is the operator's")
	flag.BoolVar(&o.verify, "verify", true, "after convergence, check that what Riak holds equals what the CRs declare")
	flag.BoolVar(&o.verifyOnly, "verify-only", false, "only verify an existing namespace; create nothing")
	flag.BoolVar(&o.stress, "stress", false,
		"also stress-test Riak: run the example application (examples/stressapp) against every cluster over mTLS "+
			"and check its results and Riak's metrics (enables TLS on the clusters)")
	flag.DurationVar(&o.stressDuration, "stress-duration", time.Minute, "how long each stress client runs its timed load")
	flag.IntVar(&o.stressThreads, "stress-threads", 16, "connections (threads) per stress client")
	flag.IntVar(&o.stressClients, "stress-clients", 2, "stress client pods per cluster")
	flag.IntVar(&o.stressValueSize, "stress-value-size", 1024, "bytes per stored object")
	flag.Float64Var(&o.stressReadRatio, "stress-read-ratio", 0.7, "fraction of operations that are reads (0-1)")
	flag.Int64Var(&o.stressMaxErrors, "stress-max-errors", 0,
		"client errors tolerated per cluster before the stress test fails")
	flag.StringVar(&o.stressImage, "stress-image", stressDefaultImage,
		"image the stress clients run in (needs python3; the script uses only the standard library)")
	flag.StringVar(&o.operatorNamespace, "operator-namespace", "",
		"namespace of the operator pod, checked for restarts/OOMKills (default: find it by label in any namespace)")
	flag.IntVar(&o.ringSize, "ring-size", minRingSize,
		"Riak ring_size (a power of two, at least 128). Tiny rings cannot balance: 8 partitions over 3 nodes is 4/2/2")
	flag.IntVar(&o.verifyWorkers, "verify-workers", 6, "parallel kubectl exec calls while verifying")
	flag.DurationVar(&o.verifyTimeout, "verify-timeout", 10*time.Minute,
		"how long verification may retry before reporting mismatches "+
			"(the operator reconciles users serially, and Riak metadata gossips)")
	flag.BoolVar(&o.mutate, "mutate", false,
		"after verifying, change/remove grants on some users and change n_val/allow_mult on some buckets, then verify again")
	flag.IntVar(&o.deleteEvery, "delete-users-every", 0,
		"after verifying, delete every Nth RiakUser and verify again (0 = off)")
	flag.BoolVar(&o.ephemeral, "ephemeral", false,
		"use emptyDir (spec.ephemeralStorage) instead of PVCs; for clusters without a storage provisioner")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "scale test failed:", err)
		os.Exit(1)
	}
}

// minRingSize is the smallest ring the harness will test. Small rings cannot be
// spread evenly over a few nodes, which makes ring-balance results meaningless.
const minRingSize = 128

// validateOpts rejects option combinations that cannot work, before touching the cluster.
func validateOpts(o opts) error {
	if o.ringSize < minRingSize || o.ringSize&(o.ringSize-1) != 0 {
		return fmt.Errorf("-ring-size %d: must be a power of two >= %d", o.ringSize, minRingSize)
	}
	if o.stress && (o.stressReadRatio < 0 || o.stressReadRatio > 1 || o.stressThreads < 1 ||
		o.stressClients < 1 || o.stressDuration < 5*time.Second) {
		return fmt.Errorf("-stress needs -stress-read-ratio in [0,1], -stress-threads >= 1, " +
			"-stress-clients >= 1 and -stress-duration >= 5s")
	}
	return nil
}

func newClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	sch := scheme.Scheme
	if err := riakv1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("add riak scheme: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	return c, nil
}

func run(o opts) error {
	if err := validateOpts(o); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if o.verifyOnly {
		return runVerifyOnly(ctx, c, o)
	}

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
	if o.users > 0 || o.stress {
		if err := ensureIssuer(ctx, c, o.namespace, o.stress); err != nil {
			return err
		}
	}

	start := time.Now()
	if err := createAll(ctx, c, o); err != nil {
		return err
	}
	fmt.Printf("Applied %d resources in %s; waiting for Ready (deadline %s)...\n",
		total, time.Since(start).Round(time.Millisecond), o.timeout)

	if err := waitReady(ctx, c, o, start); err != nil {
		// A crash-looping operator shows up as slow convergence, not as an error:
		// say so when convergence fails (issue #48).
		_ = verifyOperatorHealthy(ctx, c, o)
		return err
	}
	if !o.verify {
		return verifyOperatorHealthy(ctx, c, o)
	}
	return runStages(ctx, c, o)
}

// runVerifyOnly checks an existing namespace without creating anything.
func runVerifyOnly(ctx context.Context, c client.Client, o opts) error {
	if err := verifyEventually(ctx, c, o, "existing state"); err != nil {
		return err
	}
	if o.monitoring {
		if err := verifyMetricsEventually(ctx, c, o); err != nil {
			return err
		}
		if err := exerciseMetrics(ctx, c, o); err != nil {
			return err
		}
	}
	if o.stress {
		if err := runStress(ctx, c, o); err != nil {
			return err
		}
	}
	return verifyHealth(ctx, c, o)
}

// verifyHealth checks that neither the Riak nodes nor the operator restarted during the run.
func verifyHealth(ctx context.Context, c client.Client, o opts) error {
	if err := verifyRiakPods(ctx, c, o); err != nil {
		return err
	}
	return verifyOperatorHealthy(ctx, c, o)
}

// runStages runs the verification stages after the resources converged: verify, metrics,
// stress, then change and delete things and verify again.
func runStages(ctx context.Context, c client.Client, o opts) error {
	if err := verifyEventually(ctx, c, o, "after convergence"); err != nil {
		return err
	}
	if o.monitoring {
		if err := verifyMetricsEventually(ctx, c, o); err != nil {
			return err
		}
		if err := exerciseMetrics(ctx, c, o); err != nil {
			return err
		}
	}
	if o.stress {
		if err := runStress(ctx, c, o); err != nil {
			return err
		}
		if err := verifyEventually(ctx, c, o, "after the stress test"); err != nil {
			return err
		}
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	if o.mutate {
		if err := runMutations(ctx, c, o, rng); err != nil {
			return err
		}
	}
	if o.deleteEvery > 0 {
		n, err := deleteUsers(ctx, c, o, o.deleteEvery, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("delete users: %w", err)
		}
		fmt.Printf("deleted %d RiakUsers\n", n)
		if err := verifyEventually(ctx, c, o, "after deleting users"); err != nil {
			return err
		}
	}
	return verifyHealth(ctx, c, o)
}

// runMutations changes grants and bucket properties, waits for the operator to act, verifies.
func runMutations(ctx context.Context, c client.Client, o opts, rng *rand.Rand) error {
	n, err := mutateGrants(ctx, c, o, rng)
	if err != nil {
		return fmt.Errorf("mutate grants: %w", err)
	}
	fmt.Printf("changed or removed the grants of %d users\n", n)
	nb, err := mutateBuckets(ctx, c, o, rng)
	if err != nil {
		return fmt.Errorf("mutate buckets: %w", err)
	}
	fmt.Printf("changed the properties of %d buckets\n", nb)
	if err := waitObserved(ctx, c, o, 5*time.Minute); err != nil {
		return err
	}
	return verifyEventually(ctx, c, o, "after changing grants")
}

// verifyEventually re-runs verifyAll until Riak matches the CRs or the verify
// timeout passes: cluster metadata (users, grants, bucket types) gossips between nodes, so a
// single immediate read can legitimately lag.
func verifyEventually(ctx context.Context, c client.Client, o opts, stage string) error {
	fmt.Printf("\n── verifying Riak against the CRs (%s) ──\n", stage)
	deadline := time.Now().Add(o.verifyTimeout)
	for {
		problems, err := verifyAll(ctx, c, o)
		if err != nil {
			return err
		}
		if len(problems) == 0 {
			fmt.Println("MATCH: Riak holds exactly what the CRs declare")
			return nil
		}
		if time.Now().After(deadline) {
			for i, p := range problems {
				if i == 25 {
					fmt.Printf("  ... and %d more\n", len(problems)-25)
					break
				}
				fmt.Println("  MISMATCH:", p)
			}
			return fmt.Errorf("%d mismatches between Riak and the CRs (%s)", len(problems), stage)
		}
		fmt.Printf("  %d mismatches, retrying (gossip may lag)...\n", len(problems))
		time.Sleep(10 * time.Second)
	}
}

func ensureNamespace(ctx context.Context, c client.Client, ns string) error {
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := c.Create(ctx, n); err != nil && !apiAlreadyExists(err) {
		return fmt.Errorf("create namespace: %w", err)
	}
	return nil
}

// ensureIssuer creates the cert-manager Issuer "scale-issuer" used by every cert-auth
// RiakUser in the run. Normally it is self-signed. With ca (stress mode) it is backed by a
// CA, because the stress clients verify the server certificate with the ca.crt in their own
// client certificate Secret: both must come from the same CA.
func ensureIssuer(ctx context.Context, c client.Client, ns string, ca bool) error {
	cmGVK := func(kind string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: kind}
	}
	create := func(kind, name string, spec map[string]interface{}) error {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(cmGVK(kind))
		u.SetName(name)
		u.SetNamespace(ns)
		u.Object["spec"] = spec
		if err := c.Create(ctx, u); err != nil && !apiAlreadyExists(err) {
			return fmt.Errorf("create %s %s: %w", kind, name, err)
		}
		return nil
	}
	if !ca {
		return create("Issuer", "scale-issuer", map[string]interface{}{"selfSigned": map[string]interface{}{}})
	}
	if err := create("Issuer", "scale-root", map[string]interface{}{"selfSigned": map[string]interface{}{}}); err != nil {
		return err
	}
	if err := create("Certificate", "scale-ca", map[string]interface{}{
		"isCA": true, "commonName": "openriak-scale-ca", "secretName": "scale-ca-secret",
		"issuerRef": map[string]interface{}{"name": "scale-root", "kind": "Issuer"},
	}); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "scale-ca-secret"}, &corev1.Secret{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the CA Secret scale-ca-secret was not issued: %w", err)
		}
		time.Sleep(time.Second)
	}
	return create("Issuer", "scale-issuer", map[string]interface{}{
		"ca": map[string]interface{}{"secretName": "scale-ca-secret"}})
}

// randomizeBucketProps gives a bucket a random n_val (1-3, via either typed
// spelling) and allow_mult (via the typed field or spec.properties), so
// verification covers every path by which the spec reaches the bucket type.
func randomizeBucketProps(rng *rand.Rand, spec *riakv1.RiakBucketSpec) {
	spec.NVal, spec.ReplicationFactor, spec.AllowMulti, spec.Properties = 0, 0, false, nil
	n := int32(1 + rng.Intn(3))
	if rng.Intn(2) == 0 {
		spec.NVal = n
	} else {
		spec.ReplicationFactor = n
	}
	switch rng.Intn(3) {
	case 0:
		spec.AllowMulti = true
	case 1:
		spec.Properties = map[string]string{"allow_mult": "false"}
	default:
		spec.Properties = map[string]string{"allow_mult": "true"}
	}
}

// randomGrants returns 1-4 distinct grants with random permissions against the
// cluster's real bucket types (<cluster>-tNNN): mostly a whole type, sometimes a
// bucket inside a type, occasionally `any`. With no buckets it falls back to a
// read-any grant.
func randomGrants(rng *rand.Rand, cluster string, buckets int) []riakv1.Grant {
	if buckets == 0 {
		return []riakv1.Grant{{Resource: "any", Permission: "read"}}
	}
	perms := []string{"read", "write", "delete", "list", "admin"}
	n := 1 + rng.Intn(4)
	seen := map[string]bool{}
	var grants []riakv1.Grant
	for len(grants) < n {
		g := riakv1.Grant{Permission: perms[rng.Intn(len(perms))]}
		b := rng.Intn(buckets)
		switch r := rng.Intn(10); {
		case r == 0:
			g.Resource = "any"
		case r < 3:
			g.Resource = "bucket"
			g.BucketName = fmt.Sprintf("%s-t%03d bucket-%03d", cluster, b, b)
		default:
			g.Resource = "bucket"
			g.BucketName = fmt.Sprintf("%s-t%03d", cluster, b)
		}
		if k := grantKey(g) + "/" + g.Permission; !seen[k] {
			seen[k] = true
			grants = append(grants, g)
		}
	}
	return grants
}

func createAll(ctx context.Context, c client.Client, o opts) error {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < o.clusters; i++ {
		cl := fmt.Sprintf("scale-c%03d", i)
		size := int32(o.replicas)
		spec := riakv1.RiakClusterSpec{
			Size:       size,
			Image:      o.image,
			RiakConfig: map[string]string{"ring_size": strconv.Itoa(o.ringSize)},
		}
		if o.monitoring {
			spec.Monitoring = &riakv1.MonitoringConfig{Enabled: true, ScrapeKind: o.scrapeKind}
		}
		if o.stress {
			// The stress clients authenticate over TLS with client certificates.
			spec.TLS = &riakv1.TLSConfig{
				Enabled:     true,
				CertManager: &riakv1.CertManagerConfig{IssuerName: "scale-issuer", IssuerKind: "Issuer"},
			}
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
					Grants: randomGrants(rng, cl, o.buckets),
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
			randomizeBucketProps(rng, &bucket.Spec)
			if err := c.Create(ctx, bucket); err != nil && !apiAlreadyExists(err) {
				return fmt.Errorf("create bucket: %w", err)
			}
		}
		if o.stress {
			if err := c.Create(ctx, stressBucket(o.namespace, cl)); err != nil && !apiAlreadyExists(err) {
				return fmt.Errorf("create stress bucket: %w", err)
			}
			if err := c.Create(ctx, stressUser(o.namespace, cl)); err != nil && !apiAlreadyExists(err) {
				return fmt.Errorf("create stress user: %w", err)
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
	if o.stress { // one extra stress user and bucket per cluster
		wantU += o.clusters
		wantB += o.clusters
	}

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
