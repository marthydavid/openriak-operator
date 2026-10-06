package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

func soakTestOpts() opts {
	return opts{
		namespace: "soak-ns", image: "riak:test", storage: "lvms-vg1", ringSize: 128, stressImage: "py:test",
		soak: soakOpts{
			enabled: true, duration: 4 * time.Hour, rate: 200, users: 10, buckets: 10, threads: 4, keyspace: 2000,
			readRatio: 0.5, valueSize: 16384, nVal: 3, pr: 2, pw: 2, storage: "300Gi", memory: "4Gi",
			maxMemory: "16Gi", cpu: "2", replicas: 3, maxReplicas: 5, check: 30 * time.Second, window: time.Minute,
			cooldown: 20 * time.Minute, p99Limit: 1000, maxErrRate: 0.005, minRateRatio: 0.9,
		},
	}
}

func TestSoakCRs(t *testing.T) {
	o := soakTestOpts()
	cluster, buckets, users := soakCRs(o)

	if cluster.Spec.Size != 3 || cluster.Spec.StorageSize.String() != "300Gi" ||
		cluster.Spec.StorageClassName != "lvms-vg1" {
		t.Fatalf("cluster: size %d storage %v class %s",
			cluster.Spec.Size, cluster.Spec.StorageSize, cluster.Spec.StorageClassName)
	}
	if got := cluster.Spec.Resources.Limits.Memory().String(); got != "4Gi" {
		t.Fatalf("memory limit %s: a limit is what makes an OOM kill possible and observable", got)
	}
	req := cluster.Spec.Resources.Requests
	if req.Memory().String() != "4Gi" || req.Cpu().String() != "2" {
		t.Fatalf("requests: %v", cluster.Spec.Resources.Requests)
	}
	if cluster.Spec.RiakConfig["ring_size"] != "128" {
		t.Fatalf("ring_size %q", cluster.Spec.RiakConfig["ring_size"])
	}
	if !cluster.Spec.TLS.Enabled || cluster.Spec.Monitoring == nil || !cluster.Spec.Monitoring.Enabled {
		t.Fatal("clients authenticate over TLS and the metrics are watched: both must be on")
	}

	if len(buckets) != 10 {
		t.Fatalf("%d buckets", len(buckets))
	}
	types := map[string]bool{}
	for _, b := range buckets {
		types[b.Spec.BucketType] = true
		props := b.Spec.Properties
		if b.Spec.NVal != 3 || props["pr"] != "2" || props["pw"] != "2" || b.Spec.ClusterName != "soak" {
			t.Fatalf("bucket %s: %+v", b.Name, b.Spec)
		}
	}
	if len(types) != 10 {
		t.Fatalf("every bucket needs its own bucket type, got %d distinct", len(types))
	}

	if len(users) != 10 {
		t.Fatalf("%d users", len(users))
	}
	names := map[string]bool{}
	for _, u := range users {
		names[u.Spec.Username] = true
		if u.Spec.CertificateRef == nil || u.Spec.CertificateRef.IssuerRef == nil {
			t.Fatalf("%s needs a cert-manager certificate", u.Name)
		}
		if len(u.Spec.Grants) != 10 {
			t.Fatalf("%s has %d grants, want one per bucket", u.Name, len(u.Spec.Grants))
		}
	}
	if len(names) != 10 {
		t.Fatalf("10 different users wanted, got %d distinct usernames", len(names))
	}
}

func TestSoakClientArgs(t *testing.T) {
	o := soakTestOpts()
	args := soakClientArgs(o, 3)
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		t.Fatalf("flag %s missing in %v", name, args)
		return ""
	}
	if flag("--rate") != "20" {
		t.Fatalf("200 ops/s over 10 users is 20 each, got %s", flag("--rate"))
	}
	if flag("--pr") != "2" || flag("--pw") != "2" || flag("--keyspace") != "2000" || flag("--window") != "60" {
		t.Fatalf("args: %v", args)
	}
	if flag("--duration") != "14400" || flag("--user") != "soak_u03" || flag("--key-prefix") != "u03-" {
		t.Fatalf("args: %v", args)
	}
	if n := len(strings.Split(flag("--bucket"), ",")); n != 10 {
		t.Fatalf("%d buckets", n)
	}
	if len(strings.Split(flag("--bucket-type"), ",")) != len(strings.Split(flag("--bucket"), ",")) {
		t.Fatal("one bucket type per bucket")
	}
	if !strings.HasSuffix(flag("--host"), "soak.soak-ns.svc.cluster.local") {
		t.Fatalf("host %s", flag("--host"))
	}
	// Distinct key prefixes keep one writer per key across clients.
	if soakClientArgs(o, 4)[len(soakClientArgs(o, 4))-3] == args[len(args)-3] {
		t.Fatal("clients must not share a key prefix")
	}
}

func TestSoakJob(t *testing.T) {
	j := soakJob(soakTestOpts(), 7)
	if j.Name != "soak-client-07" || *j.Spec.BackoffLimit != 0 {
		t.Fatalf("job %s backoff %d", j.Name, *j.Spec.BackoffLimit)
	}
	if got := time.Duration(*j.Spec.ActiveDeadlineSeconds) * time.Second; got <= 4*time.Hour {
		t.Fatalf("the deadline %s must leave room beyond the 4h load", got)
	}
	if j.Spec.Template.Spec.Volumes[0].Secret.SecretName != "soak-u07-client-tls" {
		t.Fatalf("each client mounts its own user's certificate: %+v", j.Spec.Template.Spec.Volumes[0])
	}
	if j.Labels["app"] != "riak-soak" {
		t.Fatal("clients are found by their label")
	}
	if _, bad := j.Labels["cluster"]; bad {
		t.Fatal("the label key cluster collides with older StatefulSet pod anti-affinity (see stressLabels)")
	}
}

func TestValidateSoak(t *testing.T) {
	good := soakTestOpts().soak
	if err := validateSoak(good); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*soakOpts){
		"zero rate":          func(s *soakOpts) { s.rate = 0 },
		"too few replicas":   func(s *soakOpts) { s.replicas = 2 },
		"max below initial":  func(s *soakOpts) { s.maxReplicas = 2 },
		"pw above n_val":     func(s *soakOpts) { s.pw = 4 },
		"pr above n_val":     func(s *soakOpts) { s.pr = 4 },
		"too short":          func(s *soakOpts) { s.duration = time.Minute },
		"bad quantity":       func(s *soakOpts) { s.memory = "lots" },
		"max memory too low": func(s *soakOpts) { s.maxMemory = "1Gi" },
		"rate too low":       func(s *soakOpts) { s.rate = 1 },
		"no users":           func(s *soakOpts) { s.users = 0 },
		"bad storage":        func(s *soakOpts) { s.storage = "x" },
		"nval zero":          func(s *soakOpts) { s.nVal = 0 },
	} {
		s := good
		mod(&s)
		if err := validateSoak(s); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func riakPod(name string, restarts int32, reason string) corev1.Pod {
	cs := corev1.ContainerStatus{Name: "riak", RestartCount: restarts}
	if reason != "" {
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: reason, ExitCode: 137}
	}
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{
		Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs,
			{Name: "metrics-exporter", RestartCount: 9}}}}
}

func TestObservePods_countsEachOOMKillOnce(t *testing.T) {
	st := &soakState{seenOOM: map[string]bool{}, started: time.Now()}
	limit := q("4Gi")
	top := "soak-0 riak 800m 3481Mi\nsoak-1 riak 900m 1Gi\nsoak-0 metrics-exporter 3m 20Mi\n"

	pods := []corev1.Pod{riakPod("soak-0", 1, "OOMKilled"), riakPod("soak-1", 0, "")}
	pct := st.observePods(pods, limit, top)
	if st.oomKills != 1 || st.restarts != 1 {
		t.Fatalf("oom %d restarts %d (the exporter's restarts are not Riak's)", st.oomKills, st.restarts)
	}
	if pct < 84 || pct > 86 {
		t.Fatalf("3481Mi of 4Gi is about 85%%, got %.1f", pct)
	}
	st.observePods(pods, limit, top)
	if st.oomKills != 1 {
		t.Fatal("the same OOM kill must not be counted on every sample")
	}
	pods[0] = riakPod("soak-0", 2, "OOMKilled")
	st.observePods(pods, limit, top)
	if st.oomKills != 2 {
		t.Fatal("a second restart with OOMKilled is a new kill")
	}
	pods[1] = riakPod("soak-1", 1, "Error")
	st.observePods(pods, limit, top)
	if st.oomKills != 2 || st.restarts != 3 {
		t.Fatalf("a non-OOM restart counts as a restart only: oom %d restarts %d", st.oomKills, st.restarts)
	}
	if got := st.observePods(pods, limit, ""); got != 0 {
		t.Fatalf("no metrics means unknown memory (0), got %v", got)
	}
}

func TestApplyAction(t *testing.T) {
	ctx := context.Background()
	o := soakTestOpts()
	cluster, _, _ := soakCRs(o)
	c := fake.NewClientBuilder().WithScheme(certScheme(t)).WithObjects(cluster).Build()
	get := func() *riakv1.RiakCluster {
		cl := &riakv1.RiakCluster{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: soakCluster}, cl); err != nil {
			t.Fatal(err)
		}
		return cl
	}

	if err := applyAction(ctx, c, o, soakAction{Kind: actionMemory, Memory: q("6Gi")}); err != nil {
		t.Fatal(err)
	}
	cl := get()
	if cl.Spec.Resources.Limits.Memory().String() != "6Gi" || cl.Spec.Resources.Requests.Memory().String() != "6Gi" {
		t.Fatalf("memory request and limit must move together: %+v", cl.Spec.Resources)
	}
	if cl.Spec.Resources.Requests.Cpu().String() != "2" || cl.Spec.Size != 3 {
		t.Fatal("a memory change must leave CPU and size alone")
	}
	if err := applyAction(ctx, c, o, soakAction{Kind: actionScaleOut, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if cl = get(); cl.Spec.Size != 4 || cl.Spec.Resources.Limits.Memory().String() != "6Gi" {
		t.Fatalf("size %d", cl.Spec.Size)
	}
	if err := applyAction(ctx, c, o, soakAction{Kind: "bogus"}); err == nil {
		t.Fatal("an unknown action must fail")
	}
}

func TestApplyAction_withoutResources(t *testing.T) {
	ctx := context.Background()
	o := soakTestOpts()
	cluster := &riakv1.RiakCluster{ObjectMeta: metav1.ObjectMeta{Name: soakCluster, Namespace: o.namespace}}
	c := fake.NewClientBuilder().WithScheme(certScheme(t)).WithObjects(cluster).Build()
	if err := applyAction(ctx, c, o, soakAction{Kind: actionMemory, Memory: q("2Gi")}); err != nil {
		t.Fatal(err)
	}
	cl := &riakv1.RiakCluster{}
	_ = c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: soakCluster}, cl)
	if cl.Spec.Resources.Limits.Memory().String() != "2Gi" {
		t.Fatalf("%+v", cl.Spec.Resources)
	}
}

func TestSoakVerdict(t *testing.T) {
	o := soakTestOpts()
	ready := &riakv1.RiakCluster{Spec: riakv1.RiakClusterSpec{Size: 3},
		Status: riakv1.RiakClusterStatus{Phase: riakv1.PhaseReady, ReadyNodes: 3}}
	good := stressSummary{Total: stressResult{Puts: 1_440_000, Gets: 1_440_000, DurationS: 14400}}
	pods := []corev1.Pod{riakPod("soak-0", 0, "")}

	if bad := soakVerdict(o, good, 10, ready, pods); len(bad) != 0 {
		t.Fatalf("a clean run must pass: %v", bad)
	}
	check := func(name string, sum stressSummary, clients int, cl *riakv1.RiakCluster, want string) {
		t.Helper()
		bad := strings.Join(soakVerdict(o, sum, clients, cl, pods), "\n")
		if !strings.Contains(bad, want) {
			t.Errorf("%s: want %q in %q", name, want, bad)
		}
	}
	lost := good
	lost.Total.FinalLost = 2
	check("lost", lost, 10, ready, "2 values were lost")
	corrupt := good
	corrupt.Total.Corrupt = 1
	check("corrupt", corrupt, 10, ready, "1 values were corrupt")
	errs := good
	errs.Total.Errors = 100_000
	check("errors", errs, 10, ready, "operations failed")
	slow := good
	slow.Total.Puts, slow.Total.Gets = 100_000, 100_000
	check("throughput", slow, 10, ready, "below 90%")
	check("missing clients", good, 8, ready, "only 8 of 10 clients")
	notReady := &riakv1.RiakCluster{Spec: riakv1.RiakClusterSpec{Size: 4},
		Status: riakv1.RiakClusterStatus{Phase: riakv1.PhaseReady, ReadyNodes: 3}}
	check("not ready", good, 10, notReady, "ended not ready")
}

func TestTickLine(t *testing.T) {
	st := &soakState{started: time.Now().Add(-90 * time.Minute)}
	cl := &riakv1.RiakCluster{Spec: riakv1.RiakClusterSpec{Size: 3},
		Status: riakv1.RiakClusterStatus{ReadyNodes: 3}}
	line := st.tickLine(soakTestOpts(), soakSample{ClusterReady: true, Rate: 198.5, P99: 42, MemPct: 61, DiskPct: 3.2}, cl)
	for _, want := range []string{"nodes 3/3", "198 ops/s", "99% of 200", "p99 42 ms", "mem 61%", "disk 3.2%"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %q", want, line)
		}
	}
	unknown := st.tickLine(soakTestOpts(), soakSample{}, cl)
	if !strings.Contains(unknown, "no client data") || !strings.Contains(unknown, "mem ?") {
		t.Errorf("unknown values must say so: %q", unknown)
	}
}
