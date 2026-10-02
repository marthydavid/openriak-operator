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
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// requiredMetrics must be present on every node once the exporter works.
var requiredMetrics = []string{
	"riak_node_gets_total",
	"riak_node_puts_total",
	"riak_vnode_gets_total",
	"riak_ring_num_partitions",
	"riak_memory_system",
	"riak_memory_processes",
	"riak_sys_process_count",
	"riak_node_get_fsm_time_95",
}

// scrapeMetrics fetches the exporter's Riak probe from one pod through the
// apiserver pod proxy, so it works from outside the cluster and needs neither
// a shell in the distroless exporter image nor a Prometheus.
func scrapeMetrics(ns, pod string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := url.Values{"module": {"riak"}, "target": {"http://127.0.0.1:8098/stats"}}
	raw := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:7979/proxy/probe?%s", ns, pod, q.Encode())
	out, err := exec.CommandContext(ctx, "kubectl", "get", "--raw", raw).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("scrape %s: %w: %s", pod, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// parseMetrics reads Prometheus text exposition into name -> value. Labels are
// ignored: the Riak module exports plain gauges.
func parseMetrics(body string) map[string]float64 {
	m := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name := f[0]
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name = name[:i]
		}
		if v, err := strconv.ParseFloat(f[1], 64); err == nil {
			m[name] = v
		}
	}
	return m
}

// checkMetrics returns what is wrong with one node's scrape. ringSize is the
// ring_size the cluster was configured with: the exporter must report it back
// as riak_ring_num_partitions, which ties the exporter to live Riak rather than
// just to the shape of its output.
func checkMetrics(m map[string]float64, ringSize int) []string {
	var bad []string
	for _, name := range requiredMetrics {
		if _, ok := m[name]; !ok {
			bad = append(bad, "missing "+name)
		}
	}
	if v, ok := m["riak_ring_num_partitions"]; ok && int(v) != ringSize {
		bad = append(bad, fmt.Sprintf("riak_ring_num_partitions=%v, want %d", v, ringSize))
	}
	// A running BEAM always has memory and processes; zero or negative values mean
	// the exporter is reading the wrong field, not an idle node.
	for _, name := range []string{"riak_memory_system", "riak_memory_processes", "riak_sys_process_count"} {
		if v, ok := m[name]; ok && v <= 0 {
			bad = append(bad, fmt.Sprintf("%s=%v, want > 0", name, v))
		}
	}
	for name, v := range m {
		if v < 0 {
			bad = append(bad, fmt.Sprintf("%s=%v is negative", name, v))
		}
	}
	sort.Strings(bad)
	return bad
}

// verifyMetrics checks the monitoring surface of every cluster: the status
// reports every exporter ready, and every pod serves the required riak_* series.
func verifyMetrics(ctx context.Context, c client.Client, o opts) ([]string, error) {
	clusters := &riakv1.RiakClusterList{}
	if err := c.List(ctx, clusters, client.InNamespace(o.namespace)); err != nil {
		return nil, err
	}
	var (
		mu       sync.Mutex
		problems []string
		wg       sync.WaitGroup
		sem      = make(chan struct{}, o.verifyWorkers)
	)
	fail := func(format string, a ...interface{}) {
		mu.Lock()
		problems = append(problems, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	for _, cl := range clusters.Items {
		if !cl.Status.MonitoringStatus.Enabled || !cl.Status.MonitoringStatus.ExporterReady {
			fail("%s: monitoringStatus not ready (%+v)", cl.Name, cl.Status.MonitoringStatus)
		}
		if p := scrapeObjectProblem(ctx, c, cl); p != "" {
			fail("%s: %s", cl.Name, p)
		}
		for i := int32(0); i < cl.Spec.Size; i++ {
			pod := fmt.Sprintf("%s-%d", cl.Name, i)
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				body, err := scrapeMetrics(o.namespace, pod)
				if err != nil {
					fail("%v", err)
					return
				}
				for _, b := range checkMetrics(parseMetrics(body), ringSizeOf(cl)) {
					fail("%s: %s", pod, b)
				}
			}()
		}
	}
	wg.Wait()
	sort.Strings(problems)
	return problems, nil
}

// scrapeObjectProblem checks what Prometheus would do for the cluster's scrape
// object (spec.monitoring.scrapeKind, PodMonitor by default): its selector must
// match something that exposes the metrics port. "" means fine; a skipped
// object (no Prometheus Operator CRDs) or scrapeKind None is not a problem.
func scrapeObjectProblem(ctx context.Context, c client.Client, cl riakv1.RiakCluster) string {
	kind := cl.Status.MonitoringStatus.ScrapeKind
	if kind == "" {
		kind = riakv1.ScrapeKindPodMonitor
	}
	if kind == riakv1.ScrapeKindNone {
		return ""
	}
	mon := &unstructured.Unstructured{}
	mon.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: kind})
	if err := c.Get(ctx, client.ObjectKey{Namespace: cl.Namespace, Name: cl.Name + "-metrics"}, mon); err != nil {
		if meta.IsNoMatchError(err) {
			return ""
		}
		return fmt.Sprintf("get %s: %v", kind, err)
	}
	match, _, _ := unstructured.NestedStringMap(mon.Object, "spec", "selector", "matchLabels")
	sel := labels.SelectorFromSet(match)

	if kind == riakv1.ScrapeKindServiceMonitor {
		svcs := &corev1.ServiceList{}
		if err := c.List(ctx, svcs, client.InNamespace(cl.Namespace)); err != nil {
			return fmt.Sprintf("list Services: %v", err)
		}
		for _, s := range svcs.Items {
			if !sel.Matches(labels.Set(s.Labels)) {
				continue
			}
			for _, p := range s.Spec.Ports {
				if p.Name == "metrics" {
					return ""
				}
			}
		}
		return fmt.Sprintf("ServiceMonitor selector %v matches no Service with a metrics port: no targets", match)
	}

	pods := &corev1.PodList{}
	listOpts := []client.ListOption{client.InNamespace(cl.Namespace), client.MatchingLabelsSelector{Selector: sel}}
	if err := c.List(ctx, pods, listOpts...); err != nil {
		return fmt.Sprintf("list Pods: %v", err)
	}
	targets := 0
	for _, p := range pods.Items {
		for _, ct := range p.Spec.Containers {
			for _, port := range ct.Ports {
				if port.Name == "metrics" {
					targets++
				}
			}
		}
	}
	if int32(targets) != cl.Spec.Size {
		return fmt.Sprintf("PodMonitor selector %v matches %d pods with a metrics port, want %d",
			match, targets, cl.Spec.Size)
	}
	return ""
}

// verifyMetricsEventually retries verifyMetrics: exporters start after Riak
// and the first /stats can be empty while the node finishes booting.
func verifyMetricsEventually(ctx context.Context, c client.Client, o opts) error {
	fmt.Println("\n── verifying Riak metrics ──")
	deadline := time.Now().Add(o.verifyTimeout)
	for {
		problems, err := verifyMetrics(ctx, c, o)
		if err != nil {
			return err
		}
		if len(problems) == 0 {
			fmt.Println("METRICS OK: every node serves the riak_* series; exporters report ready")
			return nil
		}
		if time.Now().After(deadline) {
			for i, p := range problems {
				if i == 25 {
					fmt.Printf("  ... and %d more\n", len(problems)-25)
					break
				}
				fmt.Println("  METRICS:", p)
			}
			return fmt.Errorf("%d metrics problems", len(problems))
		}
		fmt.Printf("  %d metrics problems, retrying...\n", len(problems))
		time.Sleep(10 * time.Second)
	}
}

// stat reads one series from a scrape, treating an absent series as 0.
func stat(m map[string]float64, name string) float64 { return m[name] }

// exerciseDeltas compares scrapes taken before and after one `riak-admin test`
// cycle (one write plus reads through a single node) and returns what the
// metrics got wrong. before/after are per-pod scrapes; coord is the pod the
// cycle ran on.
//
// The write must be visible on the coordinating node (node_puts +1), the reads
// must register (node_gets >= +1), and, because the object is replicated n_val
// times, the vnode_puts summed over the whole cluster must rise by at least a
// write quorum (2). That last check ties the exporter to replication across the
// real ring: a node that only counted its own writes would fail it.
func exerciseDeltas(before, after map[string]map[string]float64, coord string) []string {
	var bad []string
	if d := stat(after[coord], "riak_node_puts_total") - stat(before[coord], "riak_node_puts_total"); d != 1 {
		bad = append(bad, fmt.Sprintf("%s: riak_node_puts_total rose by %v after one write, want 1", coord, d))
	}
	if d := stat(after[coord], "riak_node_gets_total") - stat(before[coord], "riak_node_gets_total"); d < 1 {
		bad = append(bad, fmt.Sprintf("%s: riak_node_gets_total rose by %v after the reads, want >= 1", coord, d))
	}
	var vputs float64
	for pod := range after {
		vputs += stat(after[pod], "riak_vnode_puts_total") - stat(before[pod], "riak_vnode_puts_total")
	}
	if vputs < 2 {
		bad = append(bad, fmt.Sprintf(
			"riak_vnode_puts_total summed over the cluster rose by %v, want >= 2 (a write quorum)", vputs))
	}
	return bad
}

// scrapeCluster scrapes every pod of a cluster.
func scrapeCluster(ns string, cl riakv1.RiakCluster) (map[string]map[string]float64, error) {
	out := map[string]map[string]float64{}
	for i := int32(0); i < cl.Spec.Size; i++ {
		pod := fmt.Sprintf("%s-%d", cl.Name, i)
		body, err := scrapeMetrics(ns, pod)
		if err != nil {
			return nil, err
		}
		out[pod] = parseMetrics(body)
	}
	return out, nil
}

// exerciseMetrics drives one real write/read cycle through each cluster and
// verifies that the Riak metrics move accordingly, then prints a summary of the
// key series for every node. Nothing else in the scale test sends data, so
// without this the traffic counters would only ever be checked at zero.
func exerciseMetrics(ctx context.Context, c client.Client, o opts) error {
	fmt.Println("\n── exercising Riak and verifying the metrics move ──")
	clusters := &riakv1.RiakClusterList{}
	if err := c.List(ctx, clusters, client.InNamespace(o.namespace)); err != nil {
		return err
	}
	sort.Slice(clusters.Items, func(i, j int) bool { return clusters.Items[i].Name < clusters.Items[j].Name })
	var failures []string
	for _, cl := range clusters.Items {
		before, err := scrapeCluster(o.namespace, cl)
		if err != nil {
			return err
		}
		coord := cl.Name + "-0"
		if out, err := riakAdmin(o.namespace, coord, "test"); err != nil || !strings.Contains(out, "Successfully completed") {
			failures = append(failures, fmt.Sprintf("%s: riak-admin test failed: %v %s", cl.Name, err, strings.TrimSpace(out)))
			continue
		}
		var after map[string]map[string]float64
		var bad []string
		for attempt := 0; attempt < 12; attempt++ { // replication to the other vnodes is asynchronous
			after, err = scrapeCluster(o.namespace, cl)
			if err != nil {
				return err
			}
			if bad = exerciseDeltas(before, after, coord); len(bad) == 0 {
				break
			}
			time.Sleep(5 * time.Second)
		}
		failures = append(failures, bad...)
		printMetricsSummary(cl.Name, before, after)
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Println("  METRICS:", f)
		}
		return fmt.Errorf("%d metrics problems after exercising Riak", len(failures))
	}
	fmt.Println("METRICS OK: the counters moved as expected on the coordinating node and across the replicas")
	return nil
}

// printMetricsSummary prints the key series per node, with the change caused by
// the exercised write for the traffic counters.
func printMetricsSummary(cluster string, before, after map[string]map[string]float64) {
	pods := make([]string, 0, len(after))
	for p := range after {
		pods = append(pods, p)
	}
	sort.Strings(pods)
	fmt.Printf("  %s\n", cluster)
	for _, p := range pods {
		a, b := after[p], before[p]
		fmt.Printf("    %-13s partitions=%-4.0f vnode_puts=%.0f(+%.0f) vnode_gets=%.0f(+%.0f) "+
			"node_puts=%.0f(+%.0f) node_gets=%.0f(+%.0f) mem=%.0fMiB procs=%.0f pbc_active=%.0f\n",
			p, a["riak_ring_num_partitions"],
			a["riak_vnode_puts_total"], a["riak_vnode_puts_total"]-b["riak_vnode_puts_total"],
			a["riak_vnode_gets_total"], a["riak_vnode_gets_total"]-b["riak_vnode_gets_total"],
			a["riak_node_puts_total"], a["riak_node_puts_total"]-b["riak_node_puts_total"],
			a["riak_node_gets_total"], a["riak_node_gets_total"]-b["riak_node_gets_total"],
			a["riak_memory_system"]/(1<<20), a["riak_sys_process_count"], a["riak_pbc_active"])
	}
}
