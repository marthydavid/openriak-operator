package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func q(s string) resource.Quantity { return resource.MustParse(s) }

func testPolicy() soakPolicy {
	return soakPolicy{
		TargetRate: 200, MaxMemory: q("16Gi"), MaxSize: 5, Cooldown: 15 * time.Minute,
		P99Limit: 500, MinRateRatio: 0.9, MaxErrRate: 0.05,
	}
}

// samples builds n healthy samples one minute apart starting one minute after t0, edited by mod.
func samples(n int, mod func(i int, s *soakSample)) []soakSample {
	out := make([]soakSample, n)
	for i := range out {
		out[i] = soakSample{At: t0.Add(time.Duration(i+1) * time.Minute), ClusterReady: true, AllNodesUp: true,
			Rate: 200, P99: 40, MemPct: 30}
		if mod != nil {
			mod(i, &out[i])
		}
	}
	return out
}

func TestNextMemory(t *testing.T) {
	got, ok := nextMemory(q("4Gi"), q("16Gi"))
	if !ok || got.Cmp(q("6Gi")) != 0 {
		t.Fatalf("4Gi -> %v %v, want 6Gi", got.String(), ok)
	}
	got, ok = nextMemory(q("10Gi"), q("12Gi"))
	if !ok || got.Cmp(q("12Gi")) != 0 {
		t.Fatalf("growth must be capped at the maximum, got %v", got.String())
	}
	if _, ok = nextMemory(q("16Gi"), q("16Gi")); ok {
		t.Fatal("no growth possible at the cap")
	}
	// Rounded up to whole 256Mi steps: 1Gi*1.5 = 1536Mi, already a multiple.
	if got, _ = nextMemory(q("1Gi"), q("16Gi")); got.Value() != int64(1536<<20) {
		t.Fatalf("got %v", got.String())
	}
	if got, _ = nextMemory(q("1000Mi"), q("16Gi")); got.Value()%int64(256<<20) != 0 {
		t.Fatalf("not a multiple of 256Mi: %v", got.String())
	}
}

func TestDecide(t *testing.T) {
	now := t0.Add(2 * time.Hour)
	for _, tc := range []struct {
		name   string
		hist   []soakSample
		mem    string
		size   int32
		setup  func(*soakScaler)
		policy func(*soakPolicy)
		want   string
		check  func(*testing.T, soakAction)
	}{
		{name: "healthy cluster is left alone", hist: samples(10, nil), mem: "4Gi", size: 3, want: actionNone},
		{name: "no samples", hist: nil, mem: "4Gi", size: 3, want: actionNone},
		{
			name: "an OOM kill raises memory first", mem: "4Gi", size: 3, want: actionMemory,
			hist: samples(5, func(i int, s *soakSample) { s.OOMKills = 1 }),
			check: func(t *testing.T, a soakAction) {
				if a.Memory.Cmp(q("6Gi")) != 0 {
					t.Fatalf("memory %v, want 6Gi", a.Memory.String())
				}
			},
		},
		{
			name: "an OOM kill with memory at its cap adds a node", mem: "16Gi", size: 3, want: actionScaleOut,
			hist: samples(5, func(i int, s *soakSample) { s.OOMKills = 2 }),
			check: func(t *testing.T, a soakAction) {
				if a.Size != 4 {
					t.Fatalf("size %d, want 4", a.Size)
				}
			},
		},
		{
			name: "an OOM kill with every cap reached does nothing", mem: "16Gi", size: 5, want: actionNone,
			hist: samples(5, func(i int, s *soakSample) { s.OOMKills = 2 }),
		},
		{
			name: "OOM kills before the last action are not counted again", mem: "6Gi", size: 3, want: actionNone,
			hist:  samples(5, func(i int, s *soakSample) { s.OOMKills = 1 }),
			setup: func(s *soakScaler) { s.oomAtAction = 1 },
		},
		{
			name: "memory pressure for three samples raises memory", mem: "4Gi", size: 3, want: actionMemory,
			hist: samples(6, func(i int, s *soakSample) { s.MemPct = 90 }),
		},
		{
			name: "two samples of memory pressure are not enough", mem: "4Gi", size: 3, want: actionNone,
			hist: samples(6, func(i int, s *soakSample) {
				if i >= 4 {
					s.MemPct = 95
				}
			}),
		},
		{
			name: "unknown memory (0) is never pressure", mem: "4Gi", size: 3, want: actionNone,
			hist: samples(8, func(i int, s *soakSample) { s.MemPct = 0 }),
		},
		{
			name: "sustained high p99 adds a node", mem: "4Gi", size: 3, want: actionScaleOut,
			hist: samples(8, func(i int, s *soakSample) { s.P99 = 900 }),
		},
		{
			name: "sustained low throughput adds a node", mem: "4Gi", size: 3, want: actionScaleOut,
			hist: samples(8, func(i int, s *soakSample) { s.Rate = 120 }),
		},
		{
			name: "a short latency spike is ignored", mem: "4Gi", size: 3, want: actionNone,
			hist: samples(8, func(i int, s *soakSample) {
				if i == 5 {
					s.P99 = 5000
				}
			}),
		},
		{
			name: "slowness together with many errors is a restart, not load", mem: "4Gi", size: 3, want: actionNone,
			hist: samples(8, func(i int, s *soakSample) { s.P99 = 900; s.ErrRate = 0.3 }),
		},
		{
			name: "too slow at the maximum size does nothing", mem: "4Gi", size: 5, want: actionNone,
			hist: samples(8, func(i int, s *soakSample) { s.P99 = 900 }),
		},
		{
			name: "nothing happens while a node is down or restarting", mem: "4Gi", size: 3, want: actionNone,
			hist: samples(8, func(i int, s *soakSample) {
				s.P99 = 900
				s.OOMKills = 1
				s.AllNodesUp = i != 7
			}),
		},
		{
			// The smoke test's failure: a scale-out that cannot be scheduled leaves the cluster "not ready"
			// for good. Memory pressure must still be handled; adding yet another node must not be tried.
			name: "memory is raised even when a node cannot be scheduled", mem: "4Gi", size: 4, want: actionMemory,
			hist: samples(6, func(i int, s *soakSample) { s.MemPct = 90; s.ClusterReady = false }),
		},
		{
			name: "an OOM kill with a stuck node and memory at its cap does not add another", mem: "16Gi", size: 4,
			want: actionNone,
			hist: samples(6, func(i int, s *soakSample) { s.OOMKills = 1; s.ClusterReady = false }),
		},
		{
			name: "slowness does not scale out while the cluster is not fully ready", mem: "4Gi", size: 4,
			want: actionNone,
			hist: samples(8, func(i int, s *soakSample) { s.P99 = 900; s.ClusterReady = false }),
		},
		{
			name: "cooldown holds back a second action", mem: "4Gi", size: 3, want: actionNone,
			hist:  samples(8, func(i int, s *soakSample) { s.OOMKills = 3 }),
			setup: func(s *soakScaler) { s.lastAction = now.Add(-5 * time.Minute) },
		},
		{
			name: "samples from before the last action do not count", mem: "6Gi", size: 3, want: actionNone,
			hist: samples(8, func(i int, s *soakSample) { s.P99 = 900 }),
			setup: func(s *soakScaler) {
				s.lastAction = t0.Add(7*time.Minute + 30*time.Second) // only the last sample is newer
			},
			policy: func(p *soakPolicy) { p.Cooldown = time.Minute },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &soakScaler{policy: testPolicy()}
			if tc.setup != nil {
				tc.setup(s)
			}
			if tc.policy != nil {
				tc.policy(&s.policy)
			}
			a := s.decide(now, tc.hist, q(tc.mem), tc.size)
			if a.Kind != tc.want {
				t.Fatalf("got %s (%s), want %s", a.Kind, a.Reason, tc.want)
			}
			if a.Reason == "" {
				t.Fatal("every decision must say why")
			}
			if tc.check != nil {
				tc.check(t, a)
			}
		})
	}
}

func TestDecide_appliedResetsTheBaseline(t *testing.T) {
	s := &soakScaler{policy: testPolicy()}
	hist := samples(5, func(i int, s *soakSample) { s.OOMKills = 1 })
	if a := s.decide(t0.Add(time.Hour), hist, q("4Gi"), 3); a.Kind != actionMemory {
		t.Fatalf("first decision: %+v", a)
	}
	s.applied(t0.Add(time.Hour), 1)
	if a := s.decide(t0.Add(3*time.Hour), hist, q("6Gi"), 3); a.Kind != actionNone {
		t.Fatalf("the same OOM kill must not trigger twice: %+v", a)
	}
	hist[len(hist)-1].OOMKills = 2
	if a := s.decide(t0.Add(3*time.Hour), hist, q("6Gi"), 3); a.Kind != actionMemory {
		t.Fatalf("a new OOM kill must: %+v", a)
	}
}

func TestParseTopMemory(t *testing.T) {
	out := `soak-0      riak       850m         1536Mi
soak-0      metrics-exporter   3m   20Mi
soak-1      riak       910m         2Gi
junk line
`
	got := parseTopMemory(out, "riak")
	if len(got) != 2 || got["soak-0"] != int64(1536<<20) || got["soak-1"] != int64(2<<30) {
		t.Fatalf("got %v", got)
	}
}

func TestParseDf(t *testing.T) {
	out := `Filesystem        1-blocks         Used     Available Capacity Mounted on
/dev/topolvm/abc 322122547200 32212254720 289910292480      10% /var/lib/riak
`
	used, total, err := parseDf(out)
	if err != nil || used != 32212254720 || total != 322122547200 {
		t.Fatalf("%d %d %v", used, total, err)
	}
	for _, bad := range []string{"", "Filesystem\n", "h\nx y\n", "h\nfs 0 0 0 0% /\n"} {
		if _, _, err := parseDf(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestParseAndAggregateWindows(t *testing.T) {
	win := func(pod string, t, ops, puts, gets int, rate float64, errs, late, putP99, getP99 int) string {
		return fmt.Sprintf(`[pod/%s/soak] WINDOW {"t": %d, "ops": %d, "puts": %d, "gets": %d, "ops_per_s": %v, `+
			`"errors": %d, "late": %d, "put": {"p50": 5, "p95": 20, "p99": %d, "max": 90}, `+
			`"get": {"p50": 3, "p95": 15, "p99": %d, "max": 70}}`, pod, t, ops, puts, gets, rate, errs, late, putP99, getP99)
	}
	logs := strings.Join([]string{
		win("soak-client-00-a", 1000, 1200, 600, 600, 20.0, 0, 0, 40, 30),
		win("soak-client-00-a", 1060, 1190, 600, 590, 19.8, 10, 2, 60, 30),
		win("soak-client-01-b", 1055, 1200, 600, 600, 20.0, 0, 0, 45, 80),
		"[pod/soak-client-02-c/soak] PROGRESS 10s ops=5",
		"[pod/soak-client-03-d/soak] WINDOW {not json",
	}, "\n") + "\n"
	got := parseLatestWindows(logs)
	if len(got) != 2 || got["soak-client-00-a"].T != 1060 || got["soak-client-01-b"].T != 1055 {
		t.Fatalf("latest per pod wrong: %+v", got)
	}
	rate, errRate, p99, n := aggregateWindows(got, time.Unix(1100, 0), 2*time.Minute)
	if n != 2 || rate != 39.8 || p99 != 80 {
		t.Fatalf("rate=%v p99=%v n=%d", rate, p99, n)
	}
	if want := 10.0 / (1190 + 1200 + 10); errRate < want-1e-9 || errRate > want+1e-9 {
		t.Fatalf("errRate %v want %v", errRate, want)
	}
	// A client that stopped logging is ignored.
	if _, _, _, n := aggregateWindows(got, time.Unix(1000+3600, 0), 2*time.Minute); n != 0 {
		t.Fatalf("stale windows must be ignored, n=%d", n)
	}
}

func node(name string, ready, unschedulable bool) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:   corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}}}
}

func TestSchedulableNodes(t *testing.T) {
	nodes := []corev1.Node{node("a", true, false), node("b", true, false), node("c", true, false),
		node("cordoned", true, true), node("down", false, false), {ObjectMeta: metav1.ObjectMeta{Name: "nostatus"}}}
	if got := schedulableNodes(nodes); got != 3 {
		t.Fatalf("got %d, want 3 (cordoned, NotReady and status-less nodes cannot run a Riak pod)", got)
	}
}

func pod(name, node string, ready bool, deleting bool) corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}}}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestNodesUp(t *testing.T) {
	up := []corev1.Pod{pod("a", "n1", true, false), pod("b", "n2", true, false)}
	if !nodesUp(up) {
		t.Fatal("two ready pods are up")
	}
	// The pod of a scale-out that cannot be placed: Pending, never ran. It is not "down".
	if !nodesUp(append(up, pod("c", "", false, false))) {
		t.Fatal("an unscheduled pod must be ignored")
	}
	if nodesUp(append(up, pod("c", "n3", false, false))) {
		t.Fatal("a scheduled pod that is not ready is down")
	}
	if nodesUp(append(up, pod("c", "n3", true, true))) {
		t.Fatal("a terminating pod is going down")
	}
	if !nodesUp(nil) {
		t.Fatal("no pods: nothing is down")
	}
}

func TestCheckStall(t *testing.T) {
	st := &soakState{started: time.Now()}
	notReady := soakSample{ClusterReady: false}
	st.checkStall(notReady, time.Minute) // no action yet
	if st.stalled {
		t.Fatal("nothing to stall without an action")
	}
	st.actionAt, st.actionKind = time.Now().Add(-30*time.Second), actionScaleOut
	st.checkStall(notReady, time.Minute)
	if st.stalled {
		t.Fatal("not yet past the timeout")
	}
	st.actionAt = time.Now().Add(-2 * time.Minute)
	st.checkStall(soakSample{ClusterReady: true}, time.Minute)
	if st.stalled {
		t.Fatal("a ready cluster is not stalled")
	}
	st.checkStall(notReady, time.Minute)
	st.checkStall(notReady, time.Minute) // reported once
	if !st.stalled || len(st.stalls) != 1 || len(st.timeline) != 1 {
		t.Fatalf("stalled=%v stalls=%v timeline=%v", st.stalled, st.stalls, st.timeline)
	}
}
