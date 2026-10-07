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

// The soak test holds a constant load on one multi-node cluster for hours: soakUsers cert-auth
// users, each running one client Job against all soakBuckets buckets, together at a fixed
// operations-per-second rate. While it runs the harness watches for OOM kills, restarts, memory,
// disk and the clients' own throughput, and applies soakScaler's decisions (more memory per node,
// more nodes) to the RiakCluster. At the end it verifies the data and reports.
const (
	soakCluster   = "soak"
	soakConfigMap = "riak-soak-script"
	soakApp       = "riak-soak"
)

// soakOpts are the soak test's flags.
type soakOpts struct {
	enabled       bool
	duration      time.Duration
	rate          int // operations per second, all clients together
	users         int
	buckets       int
	threads       int // per client
	keyspace      int // keys per thread and bucket
	readRatio     float64
	valueSize     int
	nVal          int
	pr, pw        int
	storage       string
	memory        string
	maxMemory     string
	cpu           string
	replicas      int
	maxReplicas   int
	check         time.Duration
	window        time.Duration
	cooldown      time.Duration
	p99Limit      float64
	maxErrRate    float64
	minRateRatio  float64
	noScale       bool
	actionTimeout time.Duration
	memPressure   float64
	// podAntiAffinity is spec.podAntiAffinity of the cluster; "" keeps the operator's default (Required).
	podAntiAffinity string
	clientCPU       string  // CPU request of each load client (no CPU limit)
	maxDisk         float64 // stop the clients when a data volume is fuller than this percent (0 = never)
}

func (s soakOpts) perClientRate() float64 { return float64(s.rate) / float64(s.users) }

func soakBucketType(i int) string { return fmt.Sprintf("soak-t%02d", i) }
func soakBucketName(i int) string { return fmt.Sprintf("soak-b%02d", i) }
func soakUserCR(i int) string     { return fmt.Sprintf("soak-u%02d", i) }
func soakUsername(i int) string   { return fmt.Sprintf("soak_u%02d", i) }
func soakJobName(i int) string    { return fmt.Sprintf("soak-client-%02d", i) }

func validateSoak(s soakOpts) error {
	switch {
	case s.rate < 1 || s.users < 1 || s.buckets < 1 || s.threads < 1:
		return fmt.Errorf("-soak-rate, -soak-users, -soak-buckets and -soak-threads must be at least 1")
	case s.perClientRate() < float64(s.threads)*0.1:
		return fmt.Errorf("-soak-rate %d over %d users is too low for %d threads each", s.rate, s.users, s.threads)
	case s.replicas < 3 || s.maxReplicas < s.replicas:
		return fmt.Errorf("-soak-replicas must be at least 3 (n_val 3, pw 2) and not above -soak-max-replicas")
	case s.nVal < 1 || s.pr > s.nVal || s.pw > s.nVal:
		return fmt.Errorf("-soak-nval %d: pr/pw (%d/%d) cannot exceed it", s.nVal, s.pr, s.pw)
	case s.podAntiAffinity != "" && s.podAntiAffinity != riakv1.PodAntiAffinityRequired &&
		s.podAntiAffinity != riakv1.PodAntiAffinityPreferred && s.podAntiAffinity != riakv1.PodAntiAffinityNone:
		return fmt.Errorf("-soak-pod-anti-affinity %q: must be Required, Preferred or None", s.podAntiAffinity)
	case s.memPressure <= 0 || s.memPressure > 1:
		return fmt.Errorf("-soak-mem-pressure %v: must be in (0, 1]", s.memPressure)
	case s.duration < 2*time.Minute:
		return fmt.Errorf("-soak-duration must be at least 2m")
	}
	parsed := map[string]resource.Quantity{}
	for _, q := range []string{s.storage, s.memory, s.maxMemory, s.cpu, s.clientCPU} {
		v, err := resource.ParseQuantity(q)
		if err != nil {
			return fmt.Errorf("invalid quantity %q: %w", q, err)
		}
		parsed[q] = v
	}
	mem, maxMem := parsed[s.memory], parsed[s.maxMemory]
	if maxMem.Cmp(mem) < 0 {
		return fmt.Errorf("-soak-max-memory must not be below -soak-memory")
	}
	return nil
}

// soakCRs builds the cluster, its buckets and its users.
func soakCRs(o opts) (*riakv1.RiakCluster, []*riakv1.RiakBucket, []*riakv1.RiakUser) {
	s := o.soak
	storage := resource.MustParse(s.storage)
	mem := resource.MustParse(s.memory)
	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: soakCluster, Namespace: o.namespace},
		Spec: riakv1.RiakClusterSpec{
			Size:             int32(s.replicas),
			Image:            o.image,
			RiakConfig:       map[string]string{"ring_size": strconv.Itoa(o.ringSize)},
			StorageClassName: o.storage,
			StorageSize:      &storage,
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(s.cpu), corev1.ResourceMemory: mem},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: mem},
			},
			PodAntiAffinity: s.podAntiAffinity,
			Monitoring:      &riakv1.MonitoringConfig{Enabled: true, ScrapeKind: o.scrapeKind},
			TLS: &riakv1.TLSConfig{
				Enabled:     true,
				CertManager: &riakv1.CertManagerConfig{IssuerName: "scale-issuer", IssuerKind: "Issuer"},
			},
		},
	}
	var buckets []*riakv1.RiakBucket
	for b := 0; b < s.buckets; b++ {
		buckets = append(buckets, &riakv1.RiakBucket{
			ObjectMeta: metav1.ObjectMeta{Name: soakBucketName(b), Namespace: o.namespace},
			Spec: riakv1.RiakBucketSpec{
				ClusterName: soakCluster, BucketName: soakBucketName(b), BucketType: soakBucketType(b),
				NVal: int32(s.nVal),
				// pr/pw are also sent with every request by the clients; here they are the
				// bucket's defaults, so any other client gets the same guarantees.
				Properties: map[string]string{"pr": strconv.Itoa(s.pr), "pw": strconv.Itoa(s.pw)},
			},
		})
	}
	var users []*riakv1.RiakUser
	for u := 0; u < s.users; u++ {
		var grants []riakv1.Grant
		for b := 0; b < s.buckets; b++ {
			grants = append(grants, riakv1.Grant{Resource: "bucket", BucketName: soakBucketType(b), Permission: "admin"})
		}
		users = append(users, &riakv1.RiakUser{
			ObjectMeta: metav1.ObjectMeta{Name: soakUserCR(u), Namespace: o.namespace},
			Spec: riakv1.RiakUserSpec{
				ClusterName: soakCluster, Username: soakUsername(u), Grants: grants,
				CertificateRef: &riakv1.UserCertificateRef{
					IssuerRef: &riakv1.CertIssuerRef{Name: "scale-issuer", Kind: "Issuer"}},
			},
		})
	}
	return cluster, buckets, users
}

// soakClientArgs is the command line of user i's client: all buckets, a share of the rate.
func soakClientArgs(o opts, i int) []string {
	s := o.soak
	var types, names []string
	for b := 0; b < s.buckets; b++ {
		types = append(types, soakBucketType(b))
		names = append(names, soakBucketName(b))
	}
	return []string{
		"/app/" + stressScriptKey,
		"--host", fmt.Sprintf("%s.%s.svc.cluster.local", soakCluster, o.namespace),
		"--user", soakUsername(i),
		"--cert", "/certs/tls.crt", "--key", "/certs/tls.key", "--cacert", "/certs/ca.crt",
		"--bucket-type", strings.Join(types, ","), "--bucket", strings.Join(names, ","),
		"--threads", strconv.Itoa(s.threads),
		"--duration", strconv.Itoa(int(s.duration.Seconds())),
		"--value-size", strconv.Itoa(s.valueSize),
		"--read-ratio", strconv.FormatFloat(s.readRatio, 'f', -1, 64),
		"--rate", strconv.FormatFloat(s.perClientRate(), 'f', -1, 64),
		"--pr", strconv.Itoa(s.pr), "--pw", strconv.Itoa(s.pw),
		"--keyspace", strconv.Itoa(s.keyspace),
		"--window", strconv.Itoa(int(s.window.Seconds())),
		"--key-prefix", fmt.Sprintf("u%02d-", i),
		"--seed", strconv.Itoa(i + 1),
	}
}

func soakLabels() map[string]string { return map[string]string{"app": soakApp} }

// soakJob is user i's client. Its deadline leaves room for connecting, the final verification
// pass over every key and a slow start.
func soakJob(o opts, i int) *batchv1.Job {
	backoff := int32(0)
	deadline := int64((o.soak.duration + 60*time.Minute).Seconds())
	mode := int32(0o555)
	noPriv := false
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: soakJobName(i), Namespace: o.namespace, Labels: soakLabels()},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: soakLabels()},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name: "soak", Image: o.stressImage, Command: []string{"python3"}, Args: soakClientArgs(o, i),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(o.soak.clientCPU),
								corev1.ResourceMemory: resource.MustParse("256Mi")},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
						},
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
							Secret: &corev1.SecretVolumeSource{SecretName: soakUserCR(i) + "-client-tls"}}},
						{Name: "app", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: soakConfigMap}, DefaultMode: &mode}}},
					},
				},
			},
		},
	}
}

// kubectlOut runs kubectl with a timeout and returns its stdout.
func kubectlOut(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// soakState is everything the monitor accumulates.
type soakState struct {
	scaler    soakScaler
	hist      []soakSample
	seenOOM   map[string]bool
	oomKills  int
	restarts  int
	timeline  []string
	events    map[string]*soakEvent
	seenEvent map[string]bool
	peak      struct{ mem, disk, p99 float64 }
	rateSum   float64
	rateN     int
	started   time.Time
	diskPods  map[string]float64
	tick      int

	actionAt   time.Time // when the last action was applied
	actionKind string
	stalled    bool
	aborted    string // set when the load was stopped early
	stalls     []string
}

type soakEvent struct {
	Count   int
	Example string
}

func (st *soakState) note(format string, a ...interface{}) {
	line := fmt.Sprintf("[%s] %s", time.Since(st.started).Round(time.Second), fmt.Sprintf(format, a...))
	st.timeline = append(st.timeline, line)
	fmt.Println("  >>", line)
}

// observePods counts OOM kills and restarts of the Riak containers and finds the highest
// memory use relative to the limit.
func (st *soakState) observePods(pods []corev1.Pod, limit resource.Quantity, topOut string) float64 {
	use := parseTopMemory(topOut, "riak")
	var pct float64
	restarts := 0
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != "riak" {
				continue
			}
			restarts += int(cs.RestartCount)
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
				key := fmt.Sprintf("%s/%d", p.Name, cs.RestartCount)
				if !st.seenOOM[key] {
					st.seenOOM[key] = true
					st.oomKills++
					st.note("OOM kill: pod %s container riak was OOMKilled (restart #%d)", p.Name, cs.RestartCount)
				}
			}
		}
		if u, ok := use[p.Name]; ok && limit.Value() > 0 {
			if v := float64(u) / float64(limit.Value()) * 100; v > pct {
				pct = v
			}
		}
	}
	st.restarts = restarts
	return pct
}

// recordEvents folds new Warning events about the soak's pods into the summary.
func (st *soakState) recordEvents(ns string) {
	out, err := kubectlOut(30*time.Second, "get", "events", "-n", ns, "--field-selector", "type=Warning", "-o", "json")
	if err != nil {
		return
	}
	var list struct {
		Items []struct {
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			InvolvedObject struct {
				Name string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return
	}
	for _, e := range list.Items {
		if st.seenEvent[e.Metadata.UID] || !strings.HasPrefix(e.InvolvedObject.Name, "soak") {
			continue
		}
		// A client Job ends "failed" when any operation failed: that is in its RESULT, not news.
		if strings.HasPrefix(e.InvolvedObject.Name, "soak-client") && e.Reason == "BackoffLimitExceeded" {
			continue
		}
		st.seenEvent[e.Metadata.UID] = true
		ev := st.events[e.Reason]
		if ev == nil {
			ev = &soakEvent{Example: fmt.Sprintf("%s: %s", e.InvolvedObject.Name, e.Message)}
			st.events[e.Reason] = ev
		}
		ev.Count++
	}
}

// diskUsage reads how full each node's data volume is.
func (st *soakState) diskUsage(ns string, pods []corev1.Pod) float64 {
	var worst float64
	for _, p := range pods {
		out, err := kubectlOut(60*time.Second, "exec", "-n", ns, p.Name, "-c", "riak", "--",
			"df", "-P", "-B1", "/var/lib/riak")
		if err != nil {
			continue
		}
		used, total, err := parseDf(out)
		if err != nil {
			continue
		}
		pct := float64(used) / float64(total) * 100
		st.diskPods[p.Name] = float64(used) / (1 << 30)
		if pct > worst {
			worst = pct
		}
	}
	return worst
}

func memLimit(cl *riakv1.RiakCluster) resource.Quantity {
	if cl.Spec.Resources != nil {
		if m, ok := cl.Spec.Resources.Limits[corev1.ResourceMemory]; ok {
			return m
		}
	}
	return resource.Quantity{}
}

func memString(cl *riakv1.RiakCluster) string {
	m := memLimit(cl)
	return m.String()
}

func soakReady(cl *riakv1.RiakCluster) bool {
	return cl.Status.Phase == riakv1.PhaseReady && cl.Status.ReadyNodes == cl.Spec.Size
}

// sampleOnce observes the cluster and the clients once.
func (st *soakState) sampleOnce(ctx context.Context, c client.Client, o opts) (soakSample, *riakv1.RiakCluster, error) {
	cl := &riakv1.RiakCluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: soakCluster}, cl); err != nil {
		return soakSample{}, nil, err
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(o.namespace),
		client.MatchingLabels{"app": "riak", "cluster": soakCluster}); err != nil {
		return soakSample{}, nil, err
	}
	top, _ := kubectlOut(30*time.Second, "top", "pod", "-n", o.namespace, "-l", "app=riak,cluster="+soakCluster,
		"--containers", "--no-headers")
	s := soakSample{At: time.Now(), ClusterReady: soakReady(cl), AllNodesUp: nodesUp(pods.Items)}
	s.MemPct = st.observePods(pods.Items, memLimit(cl), top)
	s.OOMKills, s.Restarts = st.oomKills, st.restarts

	logs, _ := kubectlOut(60*time.Second, "logs", "-n", o.namespace, "-l", "app="+soakApp, "--tail=2", "--prefix",
		"--max-log-requests="+strconv.Itoa(o.soak.users+5))
	rate, errRate, p99, n := aggregateWindows(parseLatestWindows(logs), s.At, 3*o.soak.window)
	if n > 0 {
		s.Rate, s.ErrRate, s.P99 = rate, errRate, p99
	}
	if st.tick%2 == 0 { // every minute: big objects can fill a volume within minutes
		s.DiskPct = st.diskUsage(o.namespace, pods.Items)
	} else if len(st.hist) > 0 {
		s.DiskPct = st.hist[len(st.hist)-1].DiskPct
	}
	return s, cl, nil
}

// applyAction changes the RiakCluster the way the scaler asked.
func applyAction(ctx context.Context, c client.Client, o opts, a soakAction) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		cl := &riakv1.RiakCluster{}
		if err = c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: soakCluster}, cl); err != nil {
			return err
		}
		switch a.Kind {
		case actionMemory:
			if cl.Spec.Resources == nil {
				cl.Spec.Resources = &corev1.ResourceRequirements{}
			}
			if cl.Spec.Resources.Requests == nil {
				cl.Spec.Resources.Requests = corev1.ResourceList{}
			}
			if cl.Spec.Resources.Limits == nil {
				cl.Spec.Resources.Limits = corev1.ResourceList{}
			}
			cl.Spec.Resources.Requests[corev1.ResourceMemory] = a.Memory
			cl.Spec.Resources.Limits[corev1.ResourceMemory] = a.Memory
		case actionScaleOut:
			cl.Spec.Size = a.Size
		default:
			return fmt.Errorf("unknown action %q", a.Kind)
		}
		if err = c.Update(ctx, cl); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func (st *soakState) tickLine(o opts, s soakSample, cl *riakv1.RiakCluster) string {
	mem := "mem ?"
	if s.MemPct > 0 {
		mem = fmt.Sprintf("mem %.0f%%", s.MemPct)
	}
	rate := "no client data"
	if s.Rate > 0 {
		rate = fmt.Sprintf("%.0f ops/s (%.0f%% of %d) err %.2f%% p99 %.0f ms",
			s.Rate, s.Rate/float64(o.soak.rate)*100, o.soak.rate, s.ErrRate*100, s.P99)
	}
	return fmt.Sprintf("  [%8s] nodes %d/%d ready=%v | %s | %s | disk %.1f%% | oom %d restarts %d | limit %s",
		time.Since(st.started).Round(time.Second), cl.Status.ReadyNodes, cl.Spec.Size, s.ClusterReady, rate, mem,
		s.DiskPct, s.OOMKills, s.Restarts, memString(cl))
}

// jobsDone reports how many client Jobs have finished.
func jobsDone(ctx context.Context, c client.Client, o opts) (done, total int, err error) {
	jobs := &batchv1.JobList{}
	if err = c.List(ctx, jobs, client.InNamespace(o.namespace), client.MatchingLabels(soakLabels())); err != nil {
		return 0, 0, err
	}
	for i := range jobs.Items {
		if d, _ := jobDone(&jobs.Items[i]); d {
			done++
		}
	}
	return done, len(jobs.Items), nil
}

// monitorSoak runs until the clients are done, sampling, scaling and printing.
func monitorSoak(ctx context.Context, c client.Client, o opts, st *soakState) error {
	s := o.soak
	limit := st.started.Add(s.duration + 70*time.Minute)
	mem := resource.MustParse(s.maxMemory)
	maxSize := st.capSize(ctx, c, s.maxReplicas, s.podAntiAffinity)
	st.scaler = soakScaler{policy: soakPolicy{
		TargetRate: float64(s.rate), MaxMemory: mem, MaxSize: maxSize, Cooldown: s.cooldown,
		P99Limit: s.p99Limit, MinRateRatio: s.minRateRatio, MaxErrRate: s.maxErrRate,
		MemPressure: s.memPressure,
	}}
	for {
		sample, cl, err := st.sampleOnce(ctx, c, o)
		if err != nil {
			fmt.Printf("  sample failed: %v\n", err)
		} else {
			st.hist = append(st.hist, sample)
			st.peak.mem = max(st.peak.mem, sample.MemPct)
			st.peak.disk = max(st.peak.disk, sample.DiskPct)
			st.peak.p99 = max(st.peak.p99, sample.P99)
			if sample.Rate > 0 && time.Since(st.started) < s.duration {
				st.rateSum += sample.Rate
				st.rateN++
			}
			fmt.Println(st.tickLine(o, sample, cl))
			st.checkStall(sample, s.actionTimeout)
			if diskTooFull(sample.DiskPct, s.maxDisk) {
				return st.abortForDisk(ctx, c, o, sample.DiskPct)
			}
			if !s.noScale && time.Since(st.started) < s.duration {
				a := st.scaler.decide(time.Now(), st.hist, memLimit(cl), cl.Spec.Size)
				if a.Kind != actionNone {
					st.note("scaling: %s: %s", a.Kind, a.Reason)
					if err := applyAction(ctx, c, o, a); err != nil {
						st.note("applying the %s action failed: %v", a.Kind, err)
					} else {
						st.scaler.applied(time.Now(), sample.OOMKills)
						st.actionAt, st.actionKind, st.stalled = time.Now(), a.Kind, false
						if a.Kind == actionMemory {
							st.note("raised the memory limit of every node to %s", a.Memory.String())
						} else {
							st.note("scaled the cluster out to %d nodes", a.Size)
						}
					}
				}
			}
		}
		st.tick++
		if st.tick%4 == 0 {
			st.recordEvents(o.namespace)
		}
		if done, total, err := jobsDone(ctx, c, o); err == nil && total > 0 && done == total {
			return nil
		}
		if time.Now().After(limit) {
			return fmt.Errorf("the clients did not finish within %s", s.duration+70*time.Minute)
		}
		time.Sleep(s.check)
	}
}

// capSize limits how far the cluster may grow: no further than -soak-max-replicas, and no further
// than there are schedulable Kubernetes nodes, because Riak pods need one node each.
func (st *soakState) capSize(ctx context.Context, c client.Client, wanted int, antiAffinity string) int32 {
	if antiAffinity == riakv1.PodAntiAffinityPreferred || antiAffinity == riakv1.PodAntiAffinityNone {
		return int32(wanted) // Riak pods may share a Kubernetes node: the node count is no limit
	}
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		fmt.Printf("  could not list the Kubernetes nodes (%v); not limiting the cluster size by them\n", err)
		return int32(wanted)
	}
	n := schedulableNodes(nodes.Items)
	size := min(wanted, n)
	if size < wanted {
		fmt.Printf("  scale-out is limited to %d nodes: only %d schedulable Kubernetes nodes, "+
			"and a Riak node needs one each\n", size, n)
	}
	return int32(size)
}

// diskTooFull reports whether the fullest data volume is past the limit (0 disables the check).
func diskTooFull(pct, limit float64) bool { return limit > 0 && pct >= limit }

// abortForDisk stops the load: a full data volume would damage the cluster, not test it.
func (st *soakState) abortForDisk(ctx context.Context, c client.Client, o opts, pct float64) error {
	st.note("ABORT: a data volume is %.1f%% full (limit %.0f%%); stopping the clients", pct, o.soak.maxDisk)
	prop := metav1.DeletePropagationBackground
	for i := 0; i < o.soak.users; i++ {
		_ = c.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: soakJobName(i), Namespace: o.namespace}},
			&client.DeleteOptions{PropagationPolicy: &prop})
	}
	st.aborted = fmt.Sprintf("stopped early: a data volume reached %.1f%% (limit %.0f%%)", pct, o.soak.maxDisk)
	return fmt.Errorf("%s", st.aborted)
}

// checkStall notes an action that left the cluster not ready for too long, once.
func (st *soakState) checkStall(s soakSample, timeout time.Duration) {
	if st.actionAt.IsZero() || s.ClusterReady || st.stalled || time.Since(st.actionAt) < timeout {
		return
	}
	st.stalled = true
	st.note("STALLED: the cluster has not become ready %s after the %s action",
		time.Since(st.actionAt).Round(time.Second), st.actionKind)
	st.stalls = append(st.stalls, fmt.Sprintf("the %s action left the cluster not ready for more than %s",
		st.actionKind, timeout))
}

// soakSetup creates the cluster, buckets and users and waits until clients can connect.
func soakSetup(ctx context.Context, c client.Client, o opts) error {
	cluster, buckets, users := soakCRs(o)
	if err := c.Create(ctx, cluster); err != nil && !apiAlreadyExists(err) {
		return fmt.Errorf("create the cluster: %w", err)
	}
	for _, b := range buckets {
		if err := c.Create(ctx, b); err != nil && !apiAlreadyExists(err) {
			return fmt.Errorf("create bucket %s: %w", b.Name, err)
		}
	}
	for _, u := range users {
		if err := c.Create(ctx, u); err != nil && !apiAlreadyExists(err) {
			return fmt.Errorf("create user %s: %w", u.Name, err)
		}
	}
	deadline := time.Now().Add(o.timeout)
	for {
		cl := &riakv1.RiakCluster{}
		_ = c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: soakCluster}, cl)
		nb, fb := countPhase(ctx, c, o.namespace, "RiakBucketList")
		ul := &riakv1.RiakUserList{}
		_ = c.List(ctx, ul, client.InNamespace(o.namespace))
		ready, certs := 0, 0
		for _, u := range ul.Items {
			if u.Status.Phase == riakv1.UserPhaseReady {
				ready++
			}
			if u.Status.CertificateReady {
				certs++
			}
		}
		fmt.Printf("  setup: cluster %s %d/%d | buckets %d/%d (fail %d) | users %d/%d certs %d/%d\n",
			cl.Status.Phase, cl.Status.ReadyNodes, cl.Spec.Size, nb, o.soak.buckets, fb, ready, o.soak.users,
			certs, o.soak.users)
		if soakReady(cl) && nb >= o.soak.buckets && ready >= o.soak.users && certs >= o.soak.users {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the soak cluster did not become ready within %s", o.timeout)
		}
		time.Sleep(o.poll)
	}
}

// runSoak is the whole soak test.
func runSoak(ctx context.Context, c client.Client, o opts) error {
	s := o.soak
	fmt.Printf("Soak test: %d users x %d buckets, %d ops/s total (%.1f per client), %.0f%% reads, %d-byte values, "+
		"n_val %d pr %d pw %d, %d nodes with %s each, for %s\n", s.users, s.buckets, s.rate, s.perClientRate(),
		s.readRatio*100, s.valueSize, s.nVal, s.pr, s.pw, s.replicas, s.storage, s.duration)
	if err := ensureNamespace(ctx, c, o.namespace); err != nil {
		return err
	}
	if !o.keep {
		defer teardown(c, o)
	}
	if err := ensureIssuer(ctx, c, o.namespace, true); err != nil {
		return err
	}
	if err := soakSetup(ctx, c, o); err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: soakConfigMap, Namespace: o.namespace},
		Data:       map[string]string{stressScriptKey: stressapp.Script},
	}
	if err := c.Create(ctx, cm); err != nil && !apiAlreadyExists(err) {
		return fmt.Errorf("create the script ConfigMap: %w", err)
	}
	st := &soakState{seenOOM: map[string]bool{}, events: map[string]*soakEvent{}, seenEvent: map[string]bool{},
		diskPods: map[string]float64{}, started: time.Now()}
	for i := 0; i < s.users; i++ {
		if err := c.Create(ctx, soakJob(o, i)); err != nil && !apiAlreadyExists(err) {
			return fmt.Errorf("create client %d: %w", i, err)
		}
	}
	fmt.Printf("\n── soak: %d clients started; monitoring every %s ──\n", s.users, s.check)
	monErr := monitorSoak(ctx, c, o, st)
	st.recordEvents(o.namespace)
	return finishSoak(ctx, c, o, st, monErr)
}

// finishSoak collects the clients' results, verifies the cluster and prints the report.
func finishSoak(ctx context.Context, c client.Client, o opts, st *soakState, monErr error) error {
	s := o.soak
	var results []stressResult
	var problems []string
	if monErr != nil {
		problems = append(problems, monErr.Error())
	}
	for i := 0; i < s.users; i++ {
		log, err := kubectlLogs(o.namespace, soakJobName(i))
		if err != nil && log == "" {
			problems = append(problems, fmt.Sprintf("logs of %s: %v", soakJobName(i), err))
			continue
		}
		r, err := parseStressResult(log)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", soakJobName(i), err))
			continue
		}
		results = append(results, r)
	}
	sum := summarize(results)
	cl := &riakv1.RiakCluster{}
	_ = c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: soakCluster}, cl)
	pods := &corev1.PodList{}
	_ = c.List(ctx, pods, client.InNamespace(o.namespace), client.MatchingLabels{"app": "riak", "cluster": soakCluster})

	printSoakReport(o, st, sum, results, cl)
	problems = append(problems, soakVerdict(o, st, sum, len(results), cl, pods.Items)...)

	if err := verifyEventually(ctx, c, o, "after the soak test"); err != nil {
		problems = append(problems, err.Error())
	}
	if err := verifyOperatorHealthy(ctx, c, o); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("  SOAK:", p)
		}
		return fmt.Errorf("%d soak test problems", len(problems))
	}
	fmt.Println("SOAK OK: constant load held, no lost or corrupt data, no unhandled failures")
	return nil
}

// soakVerdict turns the results into problems. OOM kills are reported, not failed on, as long as
// the scaler responded and the cluster recovered: that is what it is for.
func soakVerdict(
	o opts, st *soakState, sum stressSummary, clients int, cl *riakv1.RiakCluster, pods []corev1.Pod,
) []string {
	s := o.soak
	var bad []string
	if clients != s.users {
		bad = append(bad, fmt.Sprintf("only %d of %d clients reported a result", clients, s.users))
	}
	if lost := sum.Total.Lost + sum.Total.FinalLost; lost > 0 {
		bad = append(bad, fmt.Sprintf("%d values were lost", lost))
	}
	if corrupt := sum.Total.Corrupt + sum.Total.FinalCorrupt; corrupt > 0 {
		bad = append(bad, fmt.Sprintf("%d values were corrupt", corrupt))
	}
	if total := sum.Total.Ops + sum.Total.Errors; total > 0 {
		if rate := float64(sum.Total.Errors) / float64(total); rate > s.maxErrRate {
			bad = append(bad, fmt.Sprintf("%.2f%% of operations failed (limit %.2f%%)", rate*100, s.maxErrRate*100))
		}
	}
	if sum.Total.DurationS > 0 {
		avg := float64(sum.Total.Puts+sum.Total.Gets) / s.duration.Seconds()
		if avg < s.minRateRatio*float64(s.rate) {
			bad = append(bad, fmt.Sprintf("the clients averaged %.0f ops/s, below %.0f%% of the %d ops/s target",
				avg, s.minRateRatio*100, s.rate))
		}
	}
	bad = append(bad, st.stalls...)
	if steady, noisy := steadyErrors(st.hist); steady > 0 && float64(noisy)/float64(steady) > 0.05 {
		bad = append(bad, fmt.Sprintf("%d of %d samples taken while the cluster was healthy saw more than 1%% failed "+
			"operations (restarts are excluded)", noisy, steady))
	}
	if !soakReady(cl) {
		bad = append(bad, fmt.Sprintf("the cluster ended not ready (%s, %d/%d nodes)", cl.Status.Phase,
			cl.Status.ReadyNodes, cl.Spec.Size))
	}
	for _, p := range pods {
		if p.Status.Phase != corev1.PodRunning {
			bad = append(bad, fmt.Sprintf("Riak pod %s is %s", p.Name, p.Status.Phase))
		}
	}
	return bad
}

func printSoakReport(o opts, st *soakState, sum stressSummary, results []stressResult, cl *riakv1.RiakCluster) {
	s := o.soak
	fmt.Println("\n──────── soak test report ────────")
	fmt.Printf("ran for        %s against %d -> %d nodes, memory limit %s -> %s\n",
		time.Since(st.started).Round(time.Second), s.replicas, cl.Spec.Size, s.memory, memString(cl))
	avg := 0.0
	if st.rateN > 0 {
		avg = st.rateSum / float64(st.rateN)
	}
	fmt.Printf("throughput     target %d ops/s, clients' windows averaged %.1f ops/s; %d puts, %d gets in total\n",
		s.rate, avg, sum.Total.Puts, sum.Total.Gets)
	pl, gl := bestLatency(results, "put"), bestLatency(results, "get")
	fmt.Printf("latency (ms)   put p50/p95/p99 %.1f/%.1f/%.1f (worst client p99 %.1f) | get %.1f/%.1f/%.1f (worst %.1f)\n",
		pl.P50, pl.P95, pl.P99, sum.WorstP99["put"], gl.P50, gl.P95, gl.P99, sum.WorstP99["get"])
	fmt.Printf("integrity      errors %d, lost %d, corrupt %d, final-verified %d keys (lost %d, corrupt %d), "+
		"late slots %d\n",
		sum.Total.Errors, sum.Total.Lost, sum.Total.Corrupt, sum.Total.Verified, sum.Total.FinalLost,
		sum.Total.FinalCorrupt, sum.Total.Late)
	kinds := make([]string, 0, len(sum.Total.ErrorKinds))
	for k := range sum.Total.ErrorKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Printf("               error x%d: %s\n", sum.Total.ErrorKinds[k], k)
	}
	steady, noisy := steadyErrors(st.hist)
	fmt.Printf("steady state   %d samples with every node up, %d of them with more than 1%% failed operations "+
		"(restart windows excluded)\n", steady, noisy)
	fmt.Printf("riak           OOM kills %d, container restarts %d, peak memory %.0f%% of the limit, "+
		"peak disk %.1f%% of %s\n",
		st.oomKills, st.restarts, st.peak.mem, st.peak.disk, s.storage)
	names := make([]string, 0, len(st.diskPods))
	for n := range st.diskPods {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("               %s data volume: %.1f GiB used\n", n, st.diskPods[n])
	}
	if len(st.events) > 0 {
		fmt.Println("warning events:")
		reasons := make([]string, 0, len(st.events))
		for r := range st.events {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		for _, r := range reasons {
			fmt.Printf("               %s x%d, e.g. %s\n", r, st.events[r].Count, st.events[r].Example)
		}
	}
	if len(st.timeline) == 0 {
		fmt.Println("timeline       no OOM kills and no scaling actions")
	} else {
		fmt.Println("timeline:")
		for _, l := range st.timeline {
			fmt.Println("              ", l)
		}
	}
	fmt.Println("──────────────────────────────────")
}
