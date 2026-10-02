package main

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// sampleLog is what riak_stress.py prints: progress lines, then one RESULT line.
const sampleLog = "PROGRESS 10s ops=100 (10/s) errors=0\n" +
	`RESULT {"corrupt": 0, "duration_s": 60.1, "error_kinds": {}, "errors": 0, ` +
	`"final_corrupt": 0, "final_lost": 0, "gets": 700, "latency_ms": ` +
	`{"get": {"max": 40, "p50": 2.1, "p95": 5, "p99": 9}, "put": {"max": 80, "p50": 4, "p95": 9, "p99": 15}}, ` +
	`"lost": 0, "ops": 1000, "ops_per_s": 16.6, "puts": 300, "siblings": 0, "verified": 300}` + "\n"

func TestParseStressResult(t *testing.T) {
	r, err := parseStressResult(sampleLog)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ops != 1000 || r.Puts != 300 || r.Gets != 700 || r.Verified != 300 || r.OpsPerS != 16.6 {
		t.Errorf("unexpected parse: %+v", r)
	}
	if r.Latency["put"].P99 != 15 || r.Latency["get"].P50 != 2.1 {
		t.Errorf("latency not parsed: %+v", r.Latency)
	}
	if _, err := parseStressResult("no result here\n"); err == nil {
		t.Error("a log without a RESULT line must be an error")
	}
	if _, err := parseStressResult("RESULT {not json}\n"); err == nil {
		t.Error("a malformed RESULT line must be an error")
	}
}

func TestSummarizeAndProblems(t *testing.T) {
	a := stressResult{Ops: 100, Puts: 30, Gets: 70, OpsPerS: 10, DurationS: 10, Verified: 30,
		Latency: map[string]stressLatency{"put": {P99: 5}, "get": {P99: 3}}}
	b := stressResult{Ops: 50, Puts: 20, Gets: 30, OpsPerS: 5, DurationS: 12, Verified: 20,
		Latency: map[string]stressLatency{"put": {P99: 9}, "get": {P99: 1}}}
	s := summarize([]stressResult{a, b})
	if s.Clients != 2 || s.Total.Ops != 150 || s.Total.OpsPerS != 15 || s.Total.DurationS != 12 {
		t.Errorf("unexpected summary: %+v", s)
	}
	if s.WorstP99["put"] != 9 || s.WorstP99["get"] != 3 {
		t.Errorf("worst p99 must be the max across clients: %v", s.WorstP99)
	}
	if bad := stressProblems("c0", s, 0); len(bad) != 0 {
		t.Errorf("clean results must have no problems: %v", bad)
	}

	broken := summarize([]stressResult{{
		Puts: 10, Errors: 3, Lost: 1, FinalLost: 2, Corrupt: 1, FinalCorrupt: 4}})
	bad := stressProblems("c0", broken, 0)
	joined := strings.Join(bad, "\n")
	for _, want := range []string{"3 client errors", "DATA LOSS: 1 keys missing during the run, 2 at the final check",
		"CORRUPTION: 1 wrong values during the run, 4 at the final check"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if bad := stressProblems("c0", summarize([]stressResult{{Puts: 10, Errors: 3}}), 5); len(bad) != 0 {
		t.Errorf("errors within the allowance are fine: %v", bad)
	}
	if bad := stressProblems("c0", summarize(nil), 0); len(bad) != 1 || !strings.Contains(bad[0], "no writes") {
		t.Errorf("no writes at all must be reported: %v", bad)
	}
}

func scrape(puts, gets, vputs float64) map[string]map[string]float64 {
	return map[string]map[string]float64{
		"c-0": {"riak_node_puts_total": puts, "riak_node_gets_total": gets, "riak_vnode_puts_total": vputs},
		"c-1": {"riak_node_puts_total": 0, "riak_node_gets_total": 0, "riak_vnode_puts_total": vputs},
		"c-2": {"riak_node_puts_total": 0, "riak_node_gets_total": 0, "riak_vnode_puts_total": vputs},
	}
}

func TestStressMetricsProblems(t *testing.T) {
	s := summarize([]stressResult{{Puts: 100, Gets: 200, Verified: 100}})
	before := scrape(5, 10, 5)
	// 100 writes (node puts +100), 300 reads (gets +300), replicated 3x (+100 on each of 3 nodes).
	good := scrape(105, 310, 105)
	if bad := stressMetricsProblems("c", before, good, s); len(bad) != 0 {
		t.Errorf("metrics that match the clients are fine: %v", bad)
	}
	// The exporter under-counts: fewer puts and gets than the clients completed.
	low := scrape(60, 100, 105)
	if bad := stressMetricsProblems("c", before, low, s); len(bad) != 2 {
		t.Errorf("want the put and get shortfalls reported, got %v", bad)
	}
	// Replication missing: vnode puts did not move.
	if bad := stressMetricsProblems("c", before, scrape(105, 310, 5), s); len(bad) != 1 ||
		!strings.Contains(bad[0], "write quorum") {
		t.Errorf("want the missing replication reported, got %v", bad)
	}
	// More puts than clients completed (beyond errors) means someone else is writing.
	if bad := stressMetricsProblems("c", before, scrape(500, 310, 505), s); len(bad) != 1 ||
		!strings.Contains(bad[0], "riak_node_puts_total") {
		t.Errorf("unexplained extra writes must be reported, got %v", bad)
	}
}

func TestStressJob(t *testing.T) {
	o := opts{namespace: "ns", stressImage: "img", stressThreads: 8, stressDuration: 90 * time.Second,
		stressValueSize: 2048, stressReadRatio: 0.25}
	j := stressJob(o, "scale-c001", 1)
	if j.Name != "scale-c001-stress-1" || j.Namespace != "ns" {
		t.Errorf("unexpected job identity: %s/%s", j.Namespace, j.Name)
	}
	c := j.Spec.Template.Spec.Containers[0]
	if c.Image != "img" || c.Command[0] != "python3" {
		t.Errorf("unexpected container: %+v", c)
	}
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"/app/riak_stress.py", "--host scale-c001.ns.svc.cluster.local", "--user scale-c001_stress",
		"--bucket-type scale-c001-tstress", "--bucket stress", "--threads 8", "--duration 90",
		"--value-size 2048", "--read-ratio 0.25", "--key-prefix c1-", "--seed 2",
		"--cert /certs/tls.crt", "--cacert /certs/ca.crt",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	vols := j.Spec.Template.Spec.Volumes
	if vols[0].Secret == nil || vols[0].Secret.SecretName != "scale-c001-stress-client-tls" {
		t.Errorf("the client certificate Secret is named after the stress user CR: %+v", vols[0])
	}
	if vols[1].ConfigMap == nil || vols[1].ConfigMap.Name != stressConfigMap {
		t.Errorf("the script ConfigMap must be mounted: %+v", vols[1])
	}
	for _, labels := range []map[string]string{j.Labels, j.Spec.Template.Labels} {
		if _, bad := labels["cluster"]; bad {
			t.Error(`stress pods must not carry the "cluster" label: older clusters' Riak pod anti-affinity selects on it`)
		}
		if labels["riak-stress/target"] != "scale-c001" {
			t.Errorf("the target cluster label is missing: %v", labels)
		}
	}
	if *j.Spec.BackoffLimit != 0 {
		t.Error("a stress client must not be retried: a retry would double the load and hide a failure")
	}
	if done, ok := jobDone(j); done || ok {
		t.Error("a fresh job is not done")
	}
	j.Status.Succeeded = 1
	if done, ok := jobDone(j); !done || !ok {
		t.Error("a succeeded job is done and successful")
	}
	j.Status.Succeeded, j.Status.Failed = 0, 1
	if done, ok := jobDone(j); !done || ok {
		t.Error("a failed job is done but not successful")
	}
}

func TestStressObjects(t *testing.T) {
	if !isStressObject("scale-c000-stress") || isStressObject("scale-c000-u001") {
		t.Error("only <cluster>-stress is a stress object")
	}
	users := withoutStressUsers([]riakv1.RiakUser{
		{ObjectMeta: objMeta("scale-c000-u000")}, {ObjectMeta: objMeta("scale-c000-stress")}})
	if len(users) != 1 || users[0].Name != "scale-c000-u000" {
		t.Errorf("stress user must be filtered out: %v", users)
	}
	buckets := withoutStressBuckets([]riakv1.RiakBucket{
		{ObjectMeta: objMeta("scale-c000-b000")}, {ObjectMeta: objMeta("scale-c000-stress")}})
	if len(buckets) != 1 || buckets[0].Name != "scale-c000-b000" {
		t.Errorf("stress bucket must be filtered out: %v", buckets)
	}
	u := stressUser("ns", "scale-c000")
	if u.Spec.Username != "scale-c000_stress" || len(u.Spec.Grants) != 1 ||
		u.Spec.Grants[0].BucketName != "scale-c000-tstress" || u.Spec.Grants[0].Permission != "admin" {
		t.Errorf("the stress user needs admin on the stress bucket type: %+v", u.Spec)
	}
	b := stressBucket("ns", "scale-c000")
	if b.Spec.BucketType != "scale-c000-tstress" || b.Spec.BucketName != "stress" {
		t.Errorf("unexpected stress bucket: %+v", b.Spec)
	}
}

func TestRiakPodProblems(t *testing.T) {
	ok := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "riak"}}}}
	if bad := riakPodProblems([]corev1.Pod{ok}); len(bad) != 0 {
		t.Errorf("a running pod without restarts is fine: %v", bad)
	}
	crashed := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "riak", RestartCount: 2,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137}}}}}}
	bad := riakPodProblems([]corev1.Pod{crashed})
	if len(bad) != 1 || !strings.Contains(bad[0], "restarted 2 times") || !strings.Contains(bad[0], "OOMKilled") {
		t.Errorf("a restarted Riak pod must be reported with its reason: %v", bad)
	}
	if bad := riakPodProblems([]corev1.Pod{{Status: corev1.PodStatus{Phase: corev1.PodPending}}}); len(bad) != 1 {
		t.Errorf("a pod that is not running must be reported: %v", bad)
	}
}

func objMeta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }
