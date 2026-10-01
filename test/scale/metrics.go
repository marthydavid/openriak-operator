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

// scaleRingSize is the ring_size createAll sets on every cluster; the exporter
// must report it back as riak_ring_num_partitions.
const scaleRingSize = 8

// requiredMetrics must be present on every node once the exporter works.
var requiredMetrics = []string{
	"riak_node_gets_total",
	"riak_node_puts_total",
	"riak_vnode_gets_total",
	"riak_ring_num_partitions",
	"riak_memory_system",
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

// checkMetrics returns what is wrong with one node's scrape.
func checkMetrics(m map[string]float64) []string {
	var bad []string
	for _, name := range requiredMetrics {
		if _, ok := m[name]; !ok {
			bad = append(bad, "missing "+name)
		}
	}
	if v, ok := m["riak_ring_num_partitions"]; ok && int(v) != scaleRingSize {
		bad = append(bad, fmt.Sprintf("riak_ring_num_partitions=%v, want %d", v, scaleRingSize))
	}
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
				for _, b := range checkMetrics(parseMetrics(body)) {
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
	if err := c.List(ctx, pods, client.InNamespace(cl.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
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
		return fmt.Sprintf("PodMonitor selector %v matches %d pods with a metrics port, want %d", match, targets, cl.Spec.Size)
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
