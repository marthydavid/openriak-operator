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
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/examples/stressapp"
)

// The stress test adds one RiakBucket and one RiakUser per cluster, both named
// "<cluster>-stress". The user holds every KV permission on the bucket's type, and the
// example application (examples/stressapp) runs as that user over mTLS.
const (
	stressSuffix       = "-stress"
	stressBucketName   = "stress"
	stressConfigMap    = "riak-stress-script"
	stressScriptKey    = "riak_stress.py"
	stressDefaultImage = "registry.access.redhat.com/ubi9/python-311"
)

// isStressObject reports whether a CR belongs to the stress test, so the grant/property
// mutation and user deletion stages leave it alone.
func isStressObject(name string) bool { return strings.HasSuffix(name, stressSuffix) }

// withoutStressUsers / withoutStressBuckets drop the stress test's own CRs, which the grant,
// property and deletion stages must not touch (the clients depend on them).
func withoutStressUsers(items []riakv1.RiakUser) []riakv1.RiakUser {
	out := make([]riakv1.RiakUser, 0, len(items))
	for _, u := range items {
		if !isStressObject(u.Name) {
			out = append(out, u)
		}
	}
	return out
}

func withoutStressBuckets(items []riakv1.RiakBucket) []riakv1.RiakBucket {
	out := make([]riakv1.RiakBucket, 0, len(items))
	for _, b := range items {
		if !isStressObject(b.Name) {
			out = append(out, b)
		}
	}
	return out
}

func stressBucketType(cluster string) string { return cluster + "-tstress" }
func stressUsername(cluster string) string   { return cluster + "_stress" }

// stressBucket and stressUser are the CRs createAll adds per cluster in stress mode.
func stressBucket(ns, cluster string) *riakv1.RiakBucket {
	return &riakv1.RiakBucket{
		ObjectMeta: metav1.ObjectMeta{Name: cluster + stressSuffix, Namespace: ns},
		Spec: riakv1.RiakBucketSpec{
			ClusterName: cluster,
			BucketName:  stressBucketName,
			BucketType:  stressBucketType(cluster),
		},
	}
}

func stressUser(ns, cluster string) *riakv1.RiakUser {
	return &riakv1.RiakUser{
		ObjectMeta: metav1.ObjectMeta{Name: cluster + stressSuffix, Namespace: ns},
		Spec: riakv1.RiakUserSpec{
			ClusterName: cluster,
			Username:    stressUsername(cluster),
			CertificateRef: &riakv1.UserCertificateRef{
				IssuerRef: &riakv1.CertIssuerRef{Name: "scale-issuer", Kind: "Issuer"},
			},
			Grants: []riakv1.Grant{{
				Resource: "bucket", BucketName: stressBucketType(cluster), Permission: "admin",
			}},
		},
	}
}

// stressLatency is one operation type's latency percentiles in milliseconds.
type stressLatency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// stressResult is the RESULT line riak_stress.py prints.
type stressResult struct {
	Ops          int64                    `json:"ops"`
	Puts         int64                    `json:"puts"`
	Gets         int64                    `json:"gets"`
	Errors       int64                    `json:"errors"`
	Lost         int64                    `json:"lost"`
	Corrupt      int64                    `json:"corrupt"`
	Siblings     int64                    `json:"siblings"`
	Verified     int64                    `json:"verified"`
	FinalLost    int64                    `json:"final_lost"`
	FinalCorrupt int64                    `json:"final_corrupt"`
	DurationS    float64                  `json:"duration_s"`
	OpsPerS      float64                  `json:"ops_per_s"`
	Late         int64                    `json:"late"`
	Latency      map[string]stressLatency `json:"latency_ms"`
	ErrorKinds   map[string]int64         `json:"error_kinds"`
}

// parseStressResult finds the RESULT line in a client's log.
func parseStressResult(log string) (stressResult, error) {
	var r stressResult
	for _, line := range strings.Split(log, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "RESULT "); ok {
			if err := json.Unmarshal([]byte(rest), &r); err != nil {
				return r, fmt.Errorf("parse RESULT line: %w", err)
			}
			return r, nil
		}
	}
	return r, fmt.Errorf("no RESULT line in the client log")
}

// stressSummary is one cluster's results merged over its clients.
type stressSummary struct {
	Clients  int
	Total    stressResult
	WorstP99 map[string]float64
}

// summarize adds up the clients of one cluster. Percentiles cannot be merged exactly, so
// the summary keeps the worst client's p99 per operation, an honest upper bound.
func summarize(results []stressResult) stressSummary {
	s := stressSummary{Clients: len(results), WorstP99: map[string]float64{}}
	for _, r := range results {
		s.Total.Ops += r.Ops
		s.Total.Puts += r.Puts
		s.Total.Gets += r.Gets
		s.Total.Errors += r.Errors
		s.Total.Lost += r.Lost
		s.Total.Corrupt += r.Corrupt
		s.Total.Siblings += r.Siblings
		s.Total.Verified += r.Verified
		s.Total.FinalLost += r.FinalLost
		s.Total.FinalCorrupt += r.FinalCorrupt
		s.Total.OpsPerS += r.OpsPerS
		s.Total.Late += r.Late
		for kind, n := range r.ErrorKinds {
			if s.Total.ErrorKinds == nil {
				s.Total.ErrorKinds = map[string]int64{}
			}
			s.Total.ErrorKinds[kind] += n
		}
		if r.DurationS > s.Total.DurationS {
			s.Total.DurationS = r.DurationS
		}
		for op, l := range r.Latency {
			if l.P99 > s.WorstP99[op] {
				s.WorstP99[op] = l.P99
			}
		}
	}
	return s
}

// stressProblems returns what the clients' own results say went wrong: any error, lost or
// corrupt value, or no writes at all.
func stressProblems(cluster string, s stressSummary, maxErrors int64) []string {
	var bad []string
	t := s.Total
	if t.Puts == 0 {
		bad = append(bad, cluster+": no writes were performed")
	}
	if t.Errors > maxErrors {
		bad = append(bad, fmt.Sprintf("%s: %d client errors (allowed %d)", cluster, t.Errors, maxErrors))
	}
	if t.Lost > 0 || t.FinalLost > 0 {
		bad = append(bad, fmt.Sprintf("%s: DATA LOSS: %d keys missing during the run, %d at the final check",
			cluster, t.Lost, t.FinalLost))
	}
	if t.Corrupt > 0 || t.FinalCorrupt > 0 {
		bad = append(bad, fmt.Sprintf("%s: CORRUPTION: %d wrong values during the run, %d at the final check",
			cluster, t.Corrupt, t.FinalCorrupt))
	}
	return bad
}

// stressMetricsProblems cross-checks what the clients did against what Riak's own
// metrics counted, from scrapes taken before and after the load. They are independent
// observers of the same traffic: every client write is exactly one coordinated put on some
// node, every replica stores it (vnode puts), and every client read, including the final
// verification pass, is a coordinated get.
func stressMetricsProblems(cluster string, before, after map[string]map[string]float64, s stressSummary) []string {
	delta := func(name string) float64 {
		var d float64
		for pod := range after {
			d += after[pod][name] - before[pod][name]
		}
		return d
	}
	var bad []string
	puts, gets := float64(s.Total.Puts), float64(s.Total.Gets+s.Total.Verified)
	if d := delta("riak_node_puts_total"); d < puts || d > puts+float64(s.Total.Errors) {
		bad = append(bad, fmt.Sprintf(
			"%s: riak_node_puts_total rose by %.0f but the clients completed %.0f writes (+%d errors)",
			cluster, d, puts, s.Total.Errors))
	}
	if d := delta("riak_node_gets_total"); d < gets {
		bad = append(bad, fmt.Sprintf("%s: riak_node_gets_total rose by %.0f but the clients completed %.0f reads",
			cluster, d, gets))
	}
	if d := delta("riak_vnode_puts_total"); d < 2*puts {
		bad = append(bad, fmt.Sprintf("%s: riak_vnode_puts_total rose by %.0f, less than a write quorum (2x) of %.0f writes",
			cluster, d, puts))
	}
	return bad
}

// riakPodProblems flags Riak pods that restarted or are not running: a stress test that
// leaves nodes crashing has failed even if the clients saw no errors.
func riakPodProblems(pods []corev1.Pod) []string {
	var bad []string
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				reason := "unknown"
				if t := cs.LastTerminationState.Terminated; t != nil {
					reason = fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
				}
				bad = append(bad, fmt.Sprintf("Riak pod %s container %s restarted %d times, last: %s",
					p.Name, cs.Name, cs.RestartCount, reason))
			}
		}
		if p.Status.Phase != corev1.PodRunning {
			bad = append(bad, fmt.Sprintf("Riak pod %s is %s, not Running", p.Name, p.Status.Phase))
		}
	}
	return bad
}

// verifyRiakPods fails when any Riak node restarted during the run.
func verifyRiakPods(ctx context.Context, c client.Client, o opts) error {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(o.namespace), client.MatchingLabels{"app": "riak"}); err != nil {
		return fmt.Errorf("list Riak pods: %w", err)
	}
	if bad := riakPodProblems(pods.Items); len(bad) > 0 {
		for _, b := range bad {
			fmt.Println("  RIAK PODS:", b)
		}
		return fmt.Errorf("%d Riak pod problems", len(bad))
	}
	fmt.Printf("RIAK PODS OK: %d pod(s) running with 0 restarts\n", len(pods.Items))
	return nil
}

// stressArgs builds the command line for one client of one cluster.
func stressArgs(o opts, cluster string, client int) []string {
	return []string{
		"/app/" + stressScriptKey,
		"--host", fmt.Sprintf("%s.%s.svc.cluster.local", cluster, o.namespace),
		"--user", stressUsername(cluster),
		"--cert", "/certs/tls.crt", "--key", "/certs/tls.key", "--cacert", "/certs/ca.crt",
		"--bucket-type", stressBucketType(cluster), "--bucket", stressBucketName,
		"--threads", strconv.Itoa(o.stressThreads),
		"--duration", strconv.Itoa(int(o.stressDuration.Seconds())),
		"--value-size", strconv.Itoa(o.stressValueSize),
		"--read-ratio", strconv.FormatFloat(o.stressReadRatio, 'f', -1, 64),
		"--key-prefix", fmt.Sprintf("c%d-", client),
		"--seed", strconv.Itoa(client + 1),
	}
}

// stressLabels labels a stress client's Job and pod. It deliberately avoids the key "cluster":
// StatefulSets created by operators before 0.0.11 have a required pod anti-affinity that selects
// pods by cluster=<name> alone, so a client labelled that way could never be scheduled on a node
// that runs a node of that cluster (newer ones also require app=riak). Staying clear of it keeps
// the harness working against clusters that have not rolled yet.
func stressLabels(cluster string) map[string]string {
	return map[string]string{"app": "riak-stress", "riak-stress/target": cluster}
}

func stressJobName(cluster string, client int) string {
	return fmt.Sprintf("%s-stress-%d", cluster, client)
}

// stressJob builds the Job that runs one client against one cluster, with the stress
// user's client certificate and the application script mounted.
func stressJob(o opts, cluster string, client int) *batchv1.Job {
	backoff := int32(0)
	mode := int32(0o555)
	noPriv := false
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: stressJobName(cluster, client), Namespace: o.namespace,
			Labels: stressLabels(cluster),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: stressLabels(cluster)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "stress",
						Image:   o.stressImage,
						Command: []string{"python3"},
						Args:    stressArgs(o, cluster, client),
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noPriv,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "certs", MountPath: "/certs", ReadOnly: true},
							{Name: "app", MountPath: "/app", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "certs", VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: cluster + stressSuffix + "-client-tls"}}},
						{Name: "app", VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: stressConfigMap},
								DefaultMode:          &mode,
							}}},
					},
				},
			},
		},
	}
}

// jobDone reports whether the Job has finished, and whether it succeeded.
func jobDone(j *batchv1.Job) (done, succeeded bool) {
	if j.Status.Succeeded > 0 {
		return true, true
	}
	return j.Status.Failed > 0, false
}

func kubectlLogs(ns, job string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "logs", "-n", ns, "job/"+job).CombinedOutput()
	return string(out), err
}

// runStress runs the example stress application against every cluster and checks both
// the clients' own results and Riak's metrics. It returns an error on any problem.
func runStress(ctx context.Context, c client.Client, o opts) error {
	fmt.Printf("\n── stress test: %d client(s) x %d threads per cluster for %s, %d-byte values, %.0f%% reads ──\n",
		o.stressClients, o.stressThreads, o.stressDuration, o.stressValueSize, o.stressReadRatio*100)

	clusters := &riakv1.RiakClusterList{}
	if err := c.List(ctx, clusters, client.InNamespace(o.namespace)); err != nil {
		return err
	}
	sort.Slice(clusters.Items, func(i, j int) bool { return clusters.Items[i].Name < clusters.Items[j].Name })

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: stressConfigMap, Namespace: o.namespace},
		Data:       map[string]string{stressScriptKey: stressapp.Script},
	}
	if err := c.Create(ctx, cm); err != nil && !apiAlreadyExists(err) {
		return fmt.Errorf("create script ConfigMap: %w", err)
	}

	before := map[string]map[string]map[string]float64{}
	for _, cl := range clusters.Items {
		if o.monitoring {
			m, err := scrapeCluster(o.namespace, cl)
			if err != nil {
				return fmt.Errorf("metrics before the load: %w", err)
			}
			before[cl.Name] = m
		}
		for i := 0; i < o.stressClients; i++ {
			if err := c.Create(ctx, stressJob(o, cl.Name, i)); err != nil && !apiAlreadyExists(err) {
				return fmt.Errorf("create stress job: %w", err)
			}
		}
	}

	// Pulling the image and connecting take a while on top of the timed phase, and the
	// final verification pass re-reads every key.
	deadline := time.Now().Add(o.stressDuration + 8*time.Minute)
	results := map[string][]stressResult{}
	var problems []string
	for _, cl := range clusters.Items {
		for i := 0; i < o.stressClients; i++ {
			name := stressJobName(cl.Name, i)
			for {
				j := &batchv1.Job{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: name}, j); err != nil {
					return err
				}
				if done, _ := jobDone(j); done {
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("stress job %s did not finish in time", name)
				}
				time.Sleep(o.poll)
			}
			log, err := kubectlLogs(o.namespace, name)
			if err != nil && log == "" {
				return fmt.Errorf("logs of %s: %w", name, err)
			}
			r, err := parseStressResult(log)
			if err != nil {
				tail := log
				if len(tail) > 600 {
					tail = tail[len(tail)-600:]
				}
				problems = append(problems, fmt.Sprintf("%s: %v; log tail: %s", name, err, strings.TrimSpace(tail)))
				continue
			}
			results[cl.Name] = append(results[cl.Name], r)
		}
	}

	fmt.Printf("\n  %-11s %7s %9s %8s %8s | put p50/p95/p99 ms | get p50/p95/p99 ms | errors lost corrupt\n",
		"cluster", "clients", "ops/s", "puts", "gets")
	for _, cl := range clusters.Items {
		s := summarize(results[cl.Name])
		pl, gl := bestLatency(results[cl.Name], "put"), bestLatency(results[cl.Name], "get")
		fmt.Printf("  %-11s %7d %9.0f %8d %8d | %6.1f/%5.1f/%5.1f | %6.1f/%5.1f/%5.1f | %6d %4d %7d\n",
			cl.Name, s.Clients, s.Total.OpsPerS, s.Total.Puts, s.Total.Gets,
			pl.P50, pl.P95, s.WorstP99["put"], gl.P50, gl.P95, s.WorstP99["get"],
			s.Total.Errors, s.Total.Lost+s.Total.FinalLost, s.Total.Corrupt+s.Total.FinalCorrupt)
		problems = append(problems, stressProblems(cl.Name, s, o.stressMaxErrors)...)
		for kind, n := range s.Total.ErrorKinds {
			fmt.Printf("      error x%d: %s\n", n, kind)
		}

		if o.monitoring {
			after, err := scrapeCluster(o.namespace, cl)
			if err != nil {
				return fmt.Errorf("metrics after the load: %w", err)
			}
			mp := stressMetricsProblems(cl.Name, before[cl.Name], after, s)
			problems = append(problems, mp...)
			fmt.Printf("      Riak metrics vs clients: node_puts +%.0f (clients wrote %d), "+
				"node_gets +%.0f (clients read %d), vnode_puts +%.0f\n",
				metricDelta(before[cl.Name], after, "riak_node_puts_total"), s.Total.Puts,
				metricDelta(before[cl.Name], after, "riak_node_gets_total"), s.Total.Gets+s.Total.Verified,
				metricDelta(before[cl.Name], after, "riak_vnode_puts_total"))
		}
	}

	if !o.keep {
		for _, cl := range clusters.Items {
			for i := 0; i < o.stressClients; i++ {
				prop := metav1.DeletePropagationBackground
				_ = c.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
					Name: stressJobName(cl.Name, i), Namespace: o.namespace}},
					&client.DeleteOptions{PropagationPolicy: &prop})
			}
		}
		_ = c.Delete(ctx, cm)
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("  STRESS:", p)
		}
		return fmt.Errorf("%d stress test problems", len(problems))
	}
	fmt.Println("STRESS OK: no errors, no lost or corrupt data, and Riak's metrics agree with the clients")
	return nil
}

func metricDelta(before, after map[string]map[string]float64, name string) float64 {
	var d float64
	for pod := range after {
		d += after[pod][name] - before[pod][name]
	}
	return d
}

// bestLatency returns the median-ish latency of the first client for display; the
// worst-case p99 across clients is printed separately by the summary.
func bestLatency(results []stressResult, op string) stressLatency {
	if len(results) == 0 {
		return stressLatency{}
	}
	return results[0].Latency[op]
}
