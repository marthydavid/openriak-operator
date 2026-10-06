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
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Pure logic of the soak test: what the monitor observes, when the cluster is scaled and how
// the output of kubectl is parsed. Nothing here talks to a cluster, so all of it is unit
// tested; soak.go wires it to the real thing.

// soakSample is one observation of the cluster and its clients.
type soakSample struct {
	At           time.Time
	ClusterReady bool    // phase Ready and every desired node ready
	AllNodesUp   bool    // every node that is scheduled is ready (a Pending, unschedulable pod does not count)
	OOMKills     int     // cumulative OOMKilled terminations of Riak containers
	Restarts     int     // cumulative restarts of Riak containers
	MemPct       float64 // highest Riak working set / its memory limit; 0 when unknown
	Rate         float64 // ops/s all clients achieved in their latest window; 0 when unknown
	ErrRate      float64 // errors / (ops+errors) in that window
	P99          float64 // worst put/get p99 (ms) of that window
	DiskPct      float64 // fullest data volume, percent
}

// soakPolicy decides when to give the cluster more memory or more nodes.
type soakPolicy struct {
	TargetRate   float64           // ops/s all clients together should reach
	MaxMemory    resource.Quantity // never raise a node's memory limit above this
	MaxSize      int32             // never grow past this many nodes
	Cooldown     time.Duration     // minimum time between two actions
	P99Limit     float64           // ms; sustained above this means too slow
	MinRateRatio float64           // sustained below this share of TargetRate means too slow
	MemPressure  float64           // working set / limit above this is pressure (0.85)
	MemSamples   int               // consecutive samples of memory pressure to act on
	SlowSamples  int               // consecutive samples of slowness to act on
	MaxErrRate   float64           // slowness is not scaled for while errors exceed this (a restart)
}

func (p soakPolicy) withDefaults() soakPolicy {
	if p.MemPressure == 0 {
		p.MemPressure = 0.85
	}
	if p.MemSamples == 0 {
		p.MemSamples = 3
	}
	if p.SlowSamples == 0 {
		p.SlowSamples = 5
	}
	if p.MinRateRatio == 0 {
		p.MinRateRatio = 0.9
	}
	return p
}

const (
	actionNone     = "none"
	actionMemory   = "memory"
	actionScaleOut = "scale-out"
)

// soakAction is what the scaler wants done to the RiakCluster.
type soakAction struct {
	Kind   string
	Memory resource.Quantity // for actionMemory: the new per-node memory limit
	Size   int32             // for actionScaleOut: the new number of nodes
	Reason string
}

// soakScaler remembers the last action so each decision looks only at what happened after it.
type soakScaler struct {
	policy      soakPolicy
	lastAction  time.Time
	oomAtAction int
}

// nextMemory raises a limit by half, rounded up to 256Mi, and never above max. It reports
// whether the limit actually grew.
func nextMemory(cur, max resource.Quantity) (resource.Quantity, bool) {
	const step = int64(256 << 20)
	grown := cur.Value() * 3 / 2
	grown = (grown + step - 1) / step * step
	if grown > max.Value() {
		grown = max.Value()
	}
	if grown <= cur.Value() {
		return cur, false
	}
	return *resource.NewQuantity(grown, resource.BinarySI), true
}

// recent returns the samples taken after the last action, which are the only ones that say
// anything about the cluster as it is now.
func (s *soakScaler) recent(hist []soakSample) []soakSample {
	for i, h := range hist {
		if h.At.After(s.lastAction) {
			return hist[i:]
		}
	}
	return nil
}

// lastN reports whether the last n samples all satisfy pred.
func lastN(hist []soakSample, n int, pred func(soakSample) bool) bool {
	if len(hist) < n {
		return false
	}
	for _, h := range hist[len(hist)-n:] {
		if !pred(h) {
			return false
		}
	}
	return true
}

// decide returns what to do given the history, the cluster's current memory limit and size.
// Memory is raised first (an OOM kill or sustained memory pressure), and a node is added when
// memory is already at its cap or the cluster is simply too slow. It never scales in.
func (s *soakScaler) decide(now time.Time, hist []soakSample, memory resource.Quantity, size int32) soakAction {
	p := s.policy.withDefaults()
	none := func(reason string) soakAction { return soakAction{Kind: actionNone, Reason: reason} }
	if len(hist) == 0 {
		return none("no samples yet")
	}
	cur := hist[len(hist)-1]
	if !cur.AllNodesUp {
		return none("a node is down or restarting")
	}
	if !s.lastAction.IsZero() && now.Sub(s.lastAction) < p.Cooldown {
		return none("cooling down after the last action")
	}

	grow := func(reason string) soakAction {
		if mem, ok := nextMemory(memory, p.MaxMemory); ok {
			return soakAction{Kind: actionMemory, Memory: mem, Reason: reason}
		}
		if size < p.MaxSize && cur.ClusterReady {
			return soakAction{Kind: actionScaleOut, Size: size + 1,
				Reason: reason + "; memory is already at its cap"}
		}
		return none(reason + "; memory is at its cap and no node can be added (limit reached or cluster not ready)")
	}

	if cur.OOMKills > s.oomAtAction {
		return grow(fmt.Sprintf("%d new OOM kill(s) of Riak", cur.OOMKills-s.oomAtAction))
	}
	recent := s.recent(hist)
	if lastN(recent, p.MemSamples, func(h soakSample) bool { return h.MemPct >= p.MemPressure*100 }) {
		return grow(fmt.Sprintf("memory pressure: working set at %.0f%% of the limit for %d samples",
			cur.MemPct, p.MemSamples))
	}
	slow := func(h soakSample) bool {
		if h.ErrRate > p.MaxErrRate && p.MaxErrRate > 0 {
			return false // errors point at a restart or an outage, not at load
		}
		tooSlow := p.P99Limit > 0 && h.P99 > p.P99Limit
		tooLittle := p.TargetRate > 0 && h.Rate > 0 && h.Rate < p.MinRateRatio*p.TargetRate
		return tooSlow || tooLittle
	}
	if lastN(recent, p.SlowSamples, slow) {
		if !cur.ClusterReady {
			return none("too slow, but the cluster is not fully ready, so no node is added")
		}
		if size < p.MaxSize {
			return soakAction{Kind: actionScaleOut, Size: size + 1, Reason: fmt.Sprintf(
				"too slow for %d samples: %.0f ops/s of %.0f, worst p99 %.0f ms (limit %.0f)",
				p.SlowSamples, cur.Rate, p.TargetRate, cur.P99, p.P99Limit)}
		}
		return none("too slow, but the cluster is at its maximum size (limited by -soak-max-replicas or by the " +
			"number of schedulable Kubernetes nodes)")
	}
	return none("healthy")
}

// steadyErrThreshold is the share of failed operations above which a healthy-cluster sample is "noisy".
const steadyErrThreshold = 0.01

// steadyErrors looks at the samples taken while the cluster was healthy: every desired node ready,
// no node down, and the sample before it healthy too (the clients need a moment to reconnect after
// a restart). It returns how many such samples there are and how many of them saw more than
// threshold failed operations. A restart makes quorums fail for a while, which is expected; errors
// in steady state are not.
func steadyErrors(hist []soakSample) (steady, bad int) {
	for i := 1; i < len(hist); i++ {
		healthy := func(s soakSample) bool { return s.ClusterReady && s.AllNodesUp }
		if !healthy(hist[i]) || !healthy(hist[i-1]) || hist[i].Rate == 0 {
			continue
		}
		steady++
		if hist[i].ErrRate > steadyErrThreshold {
			bad++
		}
	}
	return steady, bad
}

// applied records that an action was taken, so later decisions only consider newer samples.
func (s *soakScaler) applied(now time.Time, oomKills int) {
	s.lastAction = now
	s.oomAtAction = oomKills
}

// ── parsing what kubectl prints ────────────────────────────────────────────────────────

// parseTopMemory reads `kubectl top pod --containers --no-headers` and returns the memory
// working set in bytes of the named container, per pod.
func parseTopMemory(out, container string) map[string]int64 {
	res := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != container {
			continue
		}
		q, err := resource.ParseQuantity(f[3])
		if err != nil {
			continue
		}
		res[f[0]] = q.Value()
	}
	return res
}

// parseDf reads `df -P -B1 <path>` and returns used and total bytes.
func parseDf(out string) (used, total int64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("unexpected df output: %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, 0, fmt.Errorf("unexpected df line: %q", lines[len(lines)-1])
	}
	total, err1 := strconv.ParseInt(f[1], 10, 64)
	used, err2 := strconv.ParseInt(f[2], 10, 64)
	if err1 != nil || err2 != nil || total <= 0 {
		return 0, 0, fmt.Errorf("unexpected df numbers: %q", lines[len(lines)-1])
	}
	return used, total, nil
}

// windowRec is a client's WINDOW line: its achieved rate and latency over one interval.
type windowRec struct {
	Pod     string
	T       int64
	Ops     int64
	Puts    int64
	Gets    int64
	OpsPerS float64
	Errors  int64
	Late    int64
	Put     stressLatency
	Get     stressLatency
}

type windowJSON struct {
	T       int64         `json:"t"`
	Ops     int64         `json:"ops"`
	Puts    int64         `json:"puts"`
	Gets    int64         `json:"gets"`
	OpsPerS float64       `json:"ops_per_s"`
	Errors  int64         `json:"errors"`
	Late    int64         `json:"late"`
	Put     stressLatency `json:"put"`
	Get     stressLatency `json:"get"`
}

// parseLatestWindows reads `kubectl logs --prefix` output and returns the newest WINDOW line
// of every pod. Lines look like `[pod/soak-client-00-abc/soak] WINDOW {...}`.
func parseLatestWindows(logs string) map[string]windowRec {
	latest := map[string]windowRec{}
	for _, line := range strings.Split(logs, "\n") {
		i := strings.Index(line, "WINDOW {")
		if i < 0 {
			continue
		}
		pod := ""
		if rest, ok := strings.CutPrefix(line, "[pod/"); ok {
			pod = strings.SplitN(rest, "/", 2)[0]
		}
		var w windowJSON
		if err := json.Unmarshal([]byte(line[i+len("WINDOW "):]), &w); err != nil {
			continue
		}
		if cur, ok := latest[pod]; ok && cur.T >= w.T {
			continue
		}
		latest[pod] = windowRec{Pod: pod, T: w.T, Ops: w.Ops, Puts: w.Puts, Gets: w.Gets, OpsPerS: w.OpsPerS,
			Errors: w.Errors, Late: w.Late, Put: w.Put, Get: w.Get}
	}
	return latest
}

// aggregateWindows combines the clients' latest windows: the rate they achieved together, the
// share of failed operations and the worst p99. Windows older than maxAge are stale (a client
// that has stopped logging) and are ignored.
func aggregateWindows(
	ws map[string]windowRec, now time.Time, maxAge time.Duration,
) (rate, errRate, p99 float64, n int) {
	var ops, errs int64
	for _, w := range ws {
		if now.Sub(time.Unix(w.T, 0)) > maxAge {
			continue
		}
		n++
		rate += w.OpsPerS
		ops += w.Ops
		errs += w.Errors
		for _, l := range []stressLatency{w.Put, w.Get} {
			if l.P99 > p99 {
				p99 = l.P99
			}
		}
	}
	if ops+errs > 0 {
		errRate = float64(errs) / float64(ops+errs)
	}
	return rate, errRate, p99, n
}

// schedulableNodes counts the Kubernetes nodes a Riak pod could run on. Riak pods carry a required
// pod anti-affinity (one per node), so the cluster cannot have more nodes than this.
func schedulableNodes(nodes []corev1.Node) int {
	n := 0
	for _, node := range nodes {
		if node.Spec.Unschedulable {
			continue
		}
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				n++
				break
			}
		}
	}
	return n
}

// nodesUp reports whether every Riak pod that has been scheduled is ready. A pod that is Pending
// without a node (a scale-out that cannot be placed) is ignored: it is not "down", it never ran.
func nodesUp(pods []corev1.Pod) bool {
	for _, p := range pods {
		if p.Spec.NodeName == "" {
			continue
		}
		if p.DeletionTimestamp != nil || !podReady(p) {
			return false
		}
	}
	return true
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
