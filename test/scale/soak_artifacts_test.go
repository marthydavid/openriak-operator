package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewArtifacts_disabledWhenEmpty(t *testing.T) {
	a, err := newArtifacts("")
	if err != nil || a != nil {
		t.Fatalf("an empty directory disables the capture: %v %v", a, err)
	}
	// Every method is a no-op on nil, so callers need no checks.
	a.writeSample(artifactRow{})
	a.writeMetrics(time.Now(), "p", nil)
	a.writeNodes(time.Now(), "x")
	a.collectLogs(soakTestOpts(), nil)
	a.writeSummary(1)
	a.close()
}

func readLines(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []map[string]interface{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("%s: not JSON: %v: %q", path, err, sc.Text())
		}
		out = append(out, m)
	}
	return out
}

func TestArtifacts_timeSeriesAreJSONLines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	a, err := newArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.writeSample(artifactRow{Time: now, ElapsedS: 30, Sample: soakSample{Rate: 998, P99: 41, MemPct: 12},
		Nodes: 3, MemLimit: "8Gi", PodMemByte: map[string]int64{"soak-0": 1 << 30},
		PodDiskGiB: map[string]float64{"soak-0": 12.5},
		Clients:    map[string]windowRec{"soak-client-00-x": {Pod: "soak-client-00-x", OpsPerS: 100, Ops: 3000}}})
	a.writeSample(artifactRow{Time: now.Add(30 * time.Second), ElapsedS: 60})
	a.writeMetrics(now, "soak-0", map[string]float64{"riak_node_gets_total": 42, "riak_memory_system": 1e9})
	a.writeNodes(now, "sm1 900m 8% 20Gi 15%\nsm2 700m 6% 18Gi 14%\n")
	a.writeNodes(now, "  \n") // nothing to record
	a.close()

	rows := readLines(t, filepath.Join(dir, "samples.jsonl"))
	if len(rows) != 2 || rows[0]["elapsed_s"].(float64) != 30 {
		t.Fatalf("samples: %v", rows)
	}
	clients := rows[0]["clients"].(map[string]interface{})
	if clients["soak-client-00-x"].(map[string]interface{})["OpsPerS"].(float64) != 100 {
		t.Fatalf("the per-client load must be saved: %v", clients)
	}
	m := readLines(t, filepath.Join(dir, "metrics.jsonl"))
	if len(m) != 1 || m[0]["pod"] != "soak-0" {
		t.Fatalf("metrics: %v", m)
	}
	if gets := m[0]["metrics"].(map[string]interface{})["riak_node_gets_total"].(float64); gets != 42 {
		t.Fatalf("every series must be saved, got %v", m[0]["metrics"])
	}
	n := readLines(t, filepath.Join(dir, "nodes.jsonl"))
	if len(n) != 1 || len(n[0]["top"].([]interface{})) != 2 {
		t.Fatalf("nodes: %v", n)
	}
	if st, err := os.Stat(filepath.Join(dir, "logs")); err != nil || !st.IsDir() {
		t.Fatal("logs/ must exist")
	}
}

func podWithRestarts(name string, riakRestarts int32) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{Name: "riak", RestartCount: riakRestarts},
			{Name: "metrics-exporter"}}}}
}

func TestLogCommands(t *testing.T) {
	o := soakTestOpts()
	o.soak.users = 3
	o.operatorNamespace = "openriak-operator"
	files := map[string][]string{}
	for _, c := range logCommands(o, []corev1.Pod{podWithRestarts("soak-0", 0), podWithRestarts("soak-1", 2)}) {
		files[c.File] = c.Args
	}
	for _, want := range []string{"events.yaml", "pods-describe.txt", "custom-resources.yaml", "top-nodes.txt",
		"riak-soak-0.log", "riak-soak-1.log", "exporter-soak-0.log", "riak-error-soak-1.log",
		"client-00.log", "client-01.log", "client-02.log", "operator.log", "previous-riak-soak-1.log"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s (have %d files)", want, len(files))
		}
	}
	if _, ok := files["client-03.log"]; ok {
		t.Error("only the configured clients")
	}
	if _, ok := files["previous-riak-soak-0.log"]; ok {
		t.Error("a pod that never restarted has no previous log")
	}
	if _, ok := files["previous-metrics-exporter-soak-1.log"]; ok {
		t.Error("the exporter did not restart")
	}
	if !strings.Contains(strings.Join(files["previous-riak-soak-1.log"], " "), "--previous") {
		t.Errorf("args: %v", files["previous-riak-soak-1.log"])
	}
	for name, args := range files {
		if args[0] == "logs" && !contains(args, o.namespace) && name != "operator.log" {
			t.Errorf("%s reads logs outside the test namespace: %v", name, args)
		}
	}
	o.operatorNamespace = ""
	for _, c := range logCommands(o, nil) {
		if c.File == "operator.log" {
			t.Error("without an operator namespace there is no operator log to read")
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestSummaryAndConfigAreWrittenAsJSON(t *testing.T) {
	dir := t.TempDir()
	a, err := newArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	a.writeSummary(soakSummary{Passed: true, Config: soakConfig(soakTestOpts().soak), OOMKills: 2,
		Timeline: []string{"[1m] raised memory"}, Events: map[string]*soakEvent{"BackOff": {Count: 3, Example: "x"}}})
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	cfg := got["config"].(map[string]interface{})
	if cfg["rate_ops_per_s"].(float64) != 200 || cfg["value_size_bytes"].(float64) != 16384 || cfg["pr"].(float64) != 2 {
		t.Fatalf("the settings must be readable in the summary, got %v", cfg)
	}
	if got["passed"] != true || got["oom_kills"].(float64) != 2 {
		t.Fatalf("summary: %v", got)
	}
}

func TestMetricsEveryTicks(t *testing.T) {
	for _, tc := range []struct {
		check time.Duration
		want  int
	}{{30 * time.Second, 2}, {20 * time.Second, 3}, {time.Minute, 1}, {5 * time.Minute, 1}, {0, 1}} {
		if got := metricsEveryTicks(tc.check); got != tc.want {
			t.Errorf("metricsEveryTicks(%s) = %d, want %d", tc.check, got, tc.want)
		}
	}
}
