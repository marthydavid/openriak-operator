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
	"math/rand"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// riakAdminScript points riak-admin at the generated vm.args, the way the
// operator does; without it the tool cannot find a node with a FQDN name.
const riakAdminScript = `VMARGS_PATH=$(ls -1 /var/lib/riak/generated.conf/vm.*.args 2>/dev/null | tail -1) exec riak-admin "$@"`

// riakAdmin runs riak-admin inside a Riak pod and returns its output.
func riakAdmin(ns, pod string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmdArgs := append([]string{"exec", "-n", ns, pod, "-c", "riak", "--", "sh", "-c", riakAdminScript, "riak-admin"}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", cmdArgs...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl exec %s riak-admin %s: %w", pod, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// expectedTokens is the harness's own mapping from a CRD permission to Riak
// permission tokens. It is deliberately independent of the operator's mapping:
// a verification that reuses the code under test cannot catch its mistakes.
var expectedTokens = map[string][]string{
	"read":   {"riak_kv.get"},
	"write":  {"riak_kv.put"},
	"delete": {"riak_kv.delete"},
	"list":   {"riak_kv.list_keys", "riak_kv.list_buckets"},
	"admin":  {"riak_kv.get", "riak_kv.put", "riak_kv.delete", "riak_kv.list_keys", "riak_kv.list_buckets"},
}

// grantKey normalises a spec grant to how Riak prints it: "type bucket", with
// "*" for `any` and for a whole bucket type.
func grantKey(g riakv1.Grant) string {
	if g.Resource == "any" {
		return "* *"
	}
	f := strings.Fields(g.BucketName)
	if len(f) == 1 {
		return f[0] + " *"
	}
	return f[0] + " " + f[1]
}

// expectedGrants is the exact set of grants a user must hold: key -> tokens.
func expectedGrants(grants []riakv1.Grant) map[string]map[string]bool {
	want := map[string]map[string]bool{}
	for _, g := range grants {
		k := grantKey(g)
		if want[k] == nil {
			want[k] = map[string]bool{}
		}
		for _, t := range expectedTokens[g.Permission] {
			want[k][t] = true
		}
	}
	return want
}

// tableRows splits a riak-admin ASCII table into trimmed cell rows, skipping
// separators.
func tableRows(section string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		var cells []string
		for _, c := range strings.Split(strings.Trim(line, "|"), "|") {
			cells = append(cells, strings.TrimSpace(c))
		}
		rows = append(rows, cells)
	}
	return rows
}

// parseGrants reads the "Dedicated permissions" table of print-grants into
// key -> tokens. Long token lists wrap onto continuation rows.
func parseGrants(out string) map[string]map[string]bool {
	got := map[string]map[string]bool{}
	i := strings.Index(out, "Dedicated permissions")
	if i < 0 {
		return got
	}
	section := out[i:]
	if j := strings.Index(section, "Cumulative permissions"); j >= 0 {
		section = section[:j]
	}
	last := ""
	for _, cells := range tableRows(section) {
		if len(cells) != 3 || cells[0] == "type" {
			continue
		}
		if cells[0] != "" {
			last = cells[0] + " " + cells[1]
		}
		if last == "" {
			continue
		}
		if got[last] == nil {
			got[last] = map[string]bool{}
		}
		for _, t := range strings.Split(cells[2], ",") {
			if t = strings.TrimSpace(t); t != "" {
				got[last][t] = true
			}
		}
	}
	return got
}

// parseUsers returns the usernames of a print-users table.
func parseUsers(out string) map[string]bool {
	users := map[string]bool{}
	for _, cells := range tableRows(out) {
		if len(cells) >= 1 && cells[0] != "" && cells[0] != "username" {
			users[cells[0]] = true
		}
	}
	return users
}

// parseCertSources returns the users that have a `certificate` source.
func parseCertSources(out string) map[string]bool {
	users := map[string]bool{}
	source := ""
	for _, cells := range tableRows(out) {
		if len(cells) != 4 || cells[0] == "users" {
			continue
		}
		if cells[2] != "" {
			source = cells[2]
		}
		if source != "certificate" {
			continue
		}
		for _, u := range strings.Split(cells[0], ",") {
			if u = strings.TrimSpace(u); u != "" {
				users[u] = true
			}
		}
	}
	return users
}

// activeBucketTypes returns the bucket types reported "(active)" by bucket-type list.
func activeBucketTypes(out string) map[string]bool {
	types := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == "(active)" {
			types[f[0]] = true
		}
	}
	return types
}

// expectedBucketProps is the harness's own reading of the RiakBucket contract:
// spec.properties is the base, nVal (else replicationFactor) sets n_val, and
// allowMulti=true sets allow_mult. Only these keys are checked; everything else
// is Riak's default.
func expectedBucketProps(spec riakv1.RiakBucketSpec) map[string]string {
	want := map[string]string{}
	for k, v := range spec.Properties {
		want[k] = v
	}
	n := spec.NVal
	if n == 0 {
		n = spec.ReplicationFactor
	}
	if n > 0 {
		want["n_val"] = strconv.Itoa(int(n))
	}
	if spec.AllowMulti {
		want["allow_mult"] = "true"
	}
	return want
}

// parseBucketTypeStatus reads the "key: value" property lines of
// `riak-admin bucket-type status <type>`.
func parseBucketTypeStatus(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok || strings.ContainsAny(k, " \t") {
			continue
		}
		props[k] = strings.TrimSpace(v)
	}
	return props
}

func diffGrants(want, got map[string]map[string]bool) []string {
	var d []string
	for k, wt := range want {
		for t := range wt {
			if !got[k][t] {
				d = append(d, fmt.Sprintf("missing %s on [%s]", t, k))
			}
		}
	}
	for k, gt := range got {
		for t := range gt {
			if !want[k][t] {
				d = append(d, fmt.Sprintf("unexpected %s on [%s]", t, k))
			}
		}
	}
	sort.Strings(d)
	return d
}

// verifyAll checks, on every node of every cluster, that what Riak holds equals
// what the CRs declare: ring membership, active bucket types and their
// n_val/allow_mult/properties, users with a certificate source, and each user's
// exact grants. It returns the mismatches.
func verifyAll(ctx context.Context, c client.Client, o opts) ([]string, error) {
	clusters := &riakv1.RiakClusterList{}
	buckets := &riakv1.RiakBucketList{}
	users := &riakv1.RiakUserList{}
	for _, l := range []client.ObjectList{clusters, buckets, users} {
		if err := c.List(ctx, l, client.InNamespace(o.namespace)); err != nil {
			return nil, err
		}
	}

	var (
		mu       sync.Mutex
		problems []string
		checks   int
		wg       sync.WaitGroup
		sem      = make(chan struct{}, o.verifyWorkers)
	)
	fail := func(format string, a ...interface{}) {
		mu.Lock()
		problems = append(problems, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	run := func(f func()) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f()
		}()
	}
	count := func() { mu.Lock(); checks++; mu.Unlock() }

	for ci := range clusters.Items {
		cl := clusters.Items[ci]
		var bts []string
		var cbs []riakv1.RiakBucket
		for _, b := range buckets.Items {
			if b.Spec.ClusterName == cl.Name {
				bts = append(bts, b.Spec.BucketType)
				cbs = append(cbs, b)
			}
		}
		var cus []riakv1.RiakUser
		for _, u := range users.Items {
			if u.Spec.ClusterName == cl.Name {
				cus = append(cus, u)
			}
		}
		for i := int32(0); i < cl.Spec.Size; i++ {
			pod := fmt.Sprintf("%s-%d", cl.Name, i)
			run(func() { // ring membership
				out, err := riakAdmin(o.namespace, pod, "member-status")
				if err != nil {
					fail("%s: %v", pod, err)
					return
				}
				count()
				want := fmt.Sprintf("Valid:%d / Leaving:0 / Exiting:0 / Joining:0 / Down:0", cl.Spec.Size)
				if !strings.Contains(out, want) {
					fail("%s: ring is not %q\n%s", pod, want, out)
				}
			})
			run(func() { // bucket types
				out, err := riakAdmin(o.namespace, pod, "bucket-type", "list")
				if err != nil {
					fail("%s: %v", pod, err)
					return
				}
				active := activeBucketTypes(out)
				for _, bt := range bts {
					count()
					if !active[bt] {
						fail("%s: bucket type %s is not active", pod, bt)
					}
				}
			})
			run(func() { // users and certificate sources
				uout, err := riakAdmin(o.namespace, pod, "security", "print-users")
				if err != nil {
					fail("%s: %v", pod, err)
					return
				}
				sout, err := riakAdmin(o.namespace, pod, "security", "print-sources")
				if err != nil {
					fail("%s: %v", pod, err)
					return
				}
				have, certs := parseUsers(uout), parseCertSources(sout)
				declared := map[string]bool{}
				for _, u := range cus {
					declared[u.Spec.Username] = true
					count()
					if !have[u.Spec.Username] {
						fail("%s: user %s missing in Riak", pod, u.Spec.Username)
					}
					if !certs[u.Spec.Username] {
						fail("%s: user %s has no certificate source", pod, u.Spec.Username)
					}
				}
				for name := range have {
					if !declared[name] {
						fail("%s: Riak has user %s that no RiakUser declares", pod, name)
					}
				}
			})
			for _, b := range cbs {
				bucket := b
				run(func() { // bucket type properties
					out, err := riakAdmin(o.namespace, pod, "bucket-type", "status", bucket.Spec.BucketType)
					if err != nil {
						fail("%s: %v", pod, err)
						return
					}
					got := parseBucketTypeStatus(out)
					for k, v := range expectedBucketProps(bucket.Spec) {
						count()
						if got[k] != v {
							fail("%s: bucket type %s has %s=%q, spec wants %q", pod, bucket.Spec.BucketType, k, got[k], v)
						}
					}
				})
			}
			for _, u := range cus {
				user := u
				run(func() { // exact grants
					out, err := riakAdmin(o.namespace, pod, "security", "print-grants", user.Spec.Username)
					if err != nil {
						fail("%s: %v", pod, err)
						return
					}
					count()
					if d := diffGrants(expectedGrants(user.Spec.Grants), parseGrants(out)); len(d) > 0 {
						fail("%s: grants of %s differ from spec: %s", pod, user.Spec.Username, strings.Join(d, "; "))
					}
				})
			}
		}
	}
	wg.Wait()
	fmt.Printf("verified %d facts against Riak on every node\n", checks)
	sort.Strings(problems)
	return problems, nil
}

// mutateGrants re-randomises the grants of every nth user, so verification can
// prove that changing (and removing) grants in a RiakUser changes Riak.
func mutateGrants(ctx context.Context, c client.Client, o opts, rng *rand.Rand) (int, error) {
	users := &riakv1.RiakUserList{}
	if err := c.List(ctx, users, client.InNamespace(o.namespace)); err != nil {
		return 0, err
	}
	sort.Slice(users.Items, func(i, j int) bool { return users.Items[i].Name < users.Items[j].Name })
	n := 0
	for i := range users.Items {
		if i%3 != 0 {
			continue
		}
		u := &users.Items[i]
		if i%9 == 0 {
			u.Spec.Grants = nil // drop every grant
		} else {
			u.Spec.Grants = randomGrants(rng, u.Spec.ClusterName, o.buckets)
		}
		if err := c.Update(ctx, u); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// mutateBuckets re-randomises the properties of every 3rd bucket, always moving
// n_val, so verification can prove that editing a RiakBucket changes Riak.
func mutateBuckets(ctx context.Context, c client.Client, o opts, rng *rand.Rand) (int, error) {
	buckets := &riakv1.RiakBucketList{}
	if err := c.List(ctx, buckets, client.InNamespace(o.namespace)); err != nil {
		return 0, err
	}
	sort.Slice(buckets.Items, func(i, j int) bool { return buckets.Items[i].Name < buckets.Items[j].Name })
	n := 0
	for i := range buckets.Items {
		if i%3 != 0 {
			continue
		}
		b := &buckets.Items[i]
		old := expectedBucketProps(b.Spec)["n_val"]
		for expectedBucketProps(b.Spec)["n_val"] == old {
			randomizeBucketProps(rng, &b.Spec)
		}
		if err := c.Update(ctx, b); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// readyFor reports whether a Ready=True condition was computed for generation gen.
func readyFor(conds []metav1.Condition, gen int64) bool {
	for _, cond := range conds {
		if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == gen {
			return true
		}
	}
	return false
}

// waitObserved waits until every RiakUser's and RiakBucket's Ready condition has
// been computed for its current generation, i.e. the operator has acted on the
// latest spec.
func waitObserved(ctx context.Context, c client.Client, o opts, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		users := &riakv1.RiakUserList{}
		buckets := &riakv1.RiakBucketList{}
		for _, l := range []client.ObjectList{users, buckets} {
			if err := c.List(ctx, l, client.InNamespace(o.namespace)); err != nil {
				return err
			}
		}
		pending := 0
		for _, u := range users.Items {
			if !readyFor(u.Status.Conditions, u.Generation) {
				pending++
			}
		}
		for _, b := range buckets.Items {
			if !readyFor(b.Status.Conditions, b.Generation) {
				pending++
			}
		}
		if pending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d RiakUsers/RiakBuckets have not observed their latest spec within %s", pending, timeout)
		}
		time.Sleep(o.poll)
	}
}

// deleteUsers deletes every nth RiakUser and waits until the objects are gone.
func deleteUsers(ctx context.Context, c client.Client, o opts, every int, timeout time.Duration) (int, error) {
	users := &riakv1.RiakUserList{}
	if err := c.List(ctx, users, client.InNamespace(o.namespace)); err != nil {
		return 0, err
	}
	sort.Slice(users.Items, func(i, j int) bool { return users.Items[i].Name < users.Items[j].Name })
	var gone []string
	for i := range users.Items {
		if i%every != 0 {
			continue
		}
		if err := c.Delete(ctx, &users.Items[i]); err != nil {
			return len(gone), err
		}
		gone = append(gone, users.Items[i].Name)
	}
	deadline := time.Now().Add(timeout)
	for _, name := range gone {
		for {
			err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: name}, &riakv1.RiakUser{})
			if err != nil {
				break
			}
			if time.Now().After(deadline) {
				return len(gone), fmt.Errorf("RiakUser %s still exists after %s", name, timeout)
			}
			time.Sleep(o.poll)
		}
	}
	return len(gone), nil
}
