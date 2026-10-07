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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// What a long run leaves behind (-soak-artifacts DIR), so it can be analysed after the cluster is
// gone:
//
//	samples.jsonl   one row per sample: the cluster state and the load every client achieved
//	metrics.jsonl   every riak_* series of every node, once a minute
//	nodes.jsonl     `kubectl top nodes`, once a minute
//	logs/           Riak, exporter and operator logs, every client's log (its WINDOW lines are the
//	                per-interval load), events, pod descriptions and the custom resources
//	summary.json    the verdict, the timeline and the final numbers

// artifactRow is one line of samples.jsonl.
type artifactRow struct {
	Time       time.Time            `json:"time"`
	ElapsedS   float64              `json:"elapsed_s"`
	Sample     soakSample           `json:"sample"`
	Nodes      int32                `json:"nodes"`
	MemLimit   string               `json:"memory_limit"`
	PodMemByte map[string]int64     `json:"pod_memory_bytes,omitempty"`
	PodDiskGiB map[string]float64   `json:"pod_disk_used_gib,omitempty"`
	Clients    map[string]windowRec `json:"clients,omitempty"`
}

type artifacts struct {
	dir     string
	samples *os.File
	metrics *os.File
	nodes   *os.File
}

// newArtifacts creates the directory and opens the time-series files. An empty dir disables it.
func newArtifacts(dir string) (*artifacts, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o750); err != nil {
		return nil, err
	}
	open := func(name string) (*os.File, error) {
		return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	}
	a := &artifacts{dir: dir}
	var err error
	if a.samples, err = open("samples.jsonl"); err != nil {
		return nil, err
	}
	if a.metrics, err = open("metrics.jsonl"); err != nil {
		return nil, err
	}
	if a.nodes, err = open("nodes.jsonl"); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *artifacts) close() {
	if a == nil {
		return
	}
	for _, f := range []*os.File{a.samples, a.metrics, a.nodes} {
		if f != nil {
			_ = f.Close()
		}
	}
}

func writeJSONLine(f *os.File, v interface{}) {
	if f == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

func (a *artifacts) writeSample(r artifactRow) {
	if a != nil {
		writeJSONLine(a.samples, r)
	}
}

// writeMetrics stores every series one pod's exporter returned.
func (a *artifacts) writeMetrics(at time.Time, pod string, m map[string]float64) {
	if a != nil {
		writeJSONLine(a.metrics, map[string]interface{}{"time": at, "pod": pod, "metrics": m})
	}
}

func (a *artifacts) writeNodes(at time.Time, top string) {
	if a != nil && strings.TrimSpace(top) != "" {
		lines := strings.Split(strings.TrimSpace(top), "\n")
		writeJSONLine(a.nodes, map[string]interface{}{"time": at, "top": lines})
	}
}

// logCommand is one file of logs/ and the kubectl arguments that produce it.
type logCommand struct {
	File string
	Args []string
}

// logCommands lists everything worth keeping from the namespace: per-pod Riak, exporter and Riak
// error/crash logs (plus the previous container's when it restarted), every client's log, events,
// descriptions and the custom resources, and the operator's log when its namespace is known.
func logCommands(o opts, pods []corev1.Pod) []logCommand {
	ns := o.namespace
	cmds := []logCommand{
		{"events.yaml", []string{"get", "events", "-n", ns, "-o", "yaml"}},
		{"pods-describe.txt", []string{"describe", "pods", "-n", ns}},
		{"pods-wide.txt", []string{"get", "pods", "-o", "wide", "-n", ns}},
		{"pvc.txt", []string{"get", "pvc", "-n", ns, "-o", "wide"}},
		{"top-pods.txt", []string{"top", "pods", "-n", ns, "--containers"}},
		{"top-nodes.txt", []string{"top", "nodes"}},
		{"custom-resources.yaml", []string{"get", "riakclusters,riakbuckets,riakusers", "-n", ns, "-o", "yaml"}},
	}
	for _, p := range pods {
		cmds = append(cmds,
			logCommand{"riak-" + p.Name + ".log", []string{"logs", "-n", ns, p.Name, "-c", "riak", "--tail=50000"}},
			logCommand{"exporter-" + p.Name + ".log",
				[]string{"logs", "-n", ns, p.Name, "-c", "metrics-exporter", "--tail=2000"}},
			logCommand{"riak-error-" + p.Name + ".log", []string{"exec", "-n", ns, p.Name, "-c", "riak", "--",
				"sh", "-c", "tail -n 20000 /var/log/riak/error.log /var/log/riak/crash.log 2>&1"}},
		)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				cmds = append(cmds, logCommand{"previous-" + cs.Name + "-" + p.Name + ".log",
					[]string{"logs", "-n", ns, p.Name, "-c", cs.Name, "--previous", "--tail=20000"}})
			}
		}
	}
	for i := 0; i < o.soak.users; i++ {
		cmds = append(cmds, logCommand{"client-" + fmt.Sprintf("%02d", i) + ".log",
			[]string{"logs", "-n", ns, "job/" + soakJobName(i)}})
	}
	if o.operatorNamespace != "" {
		cmds = append(cmds, logCommand{"operator.log", []string{"logs", "-n", o.operatorNamespace,
			"-l", "control-plane=controller-manager", "--tail=20000", "--all-containers"}})
	}
	return cmds
}

// collectLogs runs logCommands and writes each result under logs/. A failing command is
// recorded in the file, not fatal: a pod that is already gone still leaves its other files.
func (a *artifacts) collectLogs(o opts, pods []corev1.Pod) {
	if a == nil {
		return
	}
	for _, c := range logCommands(o, pods) {
		out, err := kubectlOut(3*time.Minute, c.Args...)
		if err != nil {
			out += fmt.Sprintf("\n# kubectl failed: %v\n", err)
		}
		_ = os.WriteFile(filepath.Join(a.dir, "logs", c.File), []byte(out), 0o640)
	}
}

// writeSummary stores the final numbers, the timeline and the verdict as JSON.
func (a *artifacts) writeSummary(v interface{}) {
	if a == nil {
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(a.dir, "summary.json"), append(b, '\n'), 0o640)
}

// soakSummary is summary.json.
type soakSummary struct {
	Started     time.Time              `json:"started"`
	Ended       time.Time              `json:"ended"`
	Passed      bool                   `json:"passed"`
	Config      map[string]interface{} `json:"config"`
	Results     stressSummary          `json:"results"`
	Problems    []string               `json:"problems,omitempty"`
	Timeline    []string               `json:"timeline,omitempty"`
	Events      map[string]*soakEvent  `json:"warning_events,omitempty"`
	OOMKills    int                    `json:"oom_kills"`
	Restarts    int                    `json:"container_restarts"`
	PeakMemPct  float64                `json:"peak_memory_pct"`
	PeakDiskPct float64                `json:"peak_disk_pct"`
	PeakP99     float64                `json:"peak_p99_ms"`
}

// soakConfig is the run's settings in a form encoding/json can write (soakOpts has no exported fields).
func soakConfig(s soakOpts) map[string]interface{} {
	return map[string]interface{}{
		"duration": s.duration.String(), "rate_ops_per_s": s.rate, "users": s.users, "buckets": s.buckets,
		"threads_per_client": s.threads, "keyspace_per_thread_and_bucket": s.keyspace,
		"read_ratio": s.readRatio, "value_size_bytes": s.valueSize, "n_val": s.nVal, "pr": s.pr, "pw": s.pw,
		"storage": s.storage, "memory": s.memory, "max_memory": s.maxMemory, "cpu_request": s.cpu,
		"client_cpu_request": s.clientCPU, "replicas": s.replicas, "max_replicas": s.maxReplicas,
		"pod_anti_affinity": s.podAntiAffinity, "mem_pressure": s.memPressure, "p99_limit_ms": s.p99Limit,
		"max_error_rate": s.maxErrRate, "min_rate_ratio": s.minRateRatio, "max_disk_pct": s.maxDisk,
		"cooldown": s.cooldown.String(), "check": s.check.String(), "window": s.window.String(),
		"scaling_disabled": s.noScale,
	}
}

// metricsEveryTicks is how often the exporters are scraped and the nodes sampled: with the
// default 30s sample interval, once a minute.
func metricsEveryTicks(check time.Duration) int {
	if check <= 0 {
		return 1
	}
	n := int(time.Minute / check)
	return max(n, 1)
}
