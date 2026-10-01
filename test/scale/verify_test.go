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
	"fmt"
	"strings"
	"testing"
)

func memberTable(pcts ...string) string {
	var b strings.Builder
	b.WriteString("Status     Ring    Pending    Node\n-----\n")
	for i, p := range pcts {
		fmt.Fprintf(&b, "valid      %s      --      riak@n-%d.svc\n", p, i)
	}
	return b.String()
}

func TestRingImbalance(t *testing.T) {
	cases := []struct {
		name     string
		table    string
		nodes    int
		ringSize int
		wantBad  bool
	}{
		{"8 over 3 as riak splits it (4/2/2) is unbalanced", memberTable("50.0%", "25.0%", "25.0%"), 3, 8, true},
		{"8 over 3 evenly (3/3/2) is balanced", memberTable("37.5%", "37.5%", "25.0%"), 3, 8, false},
		{"128 over 3 (43/43/42)", memberTable("33.6%", "33.6%", "32.8%"), 3, 128, false},
		{"128 over 3 skewed (64/32/32)", memberTable("50.0%", "25.0%", "25.0%"), 3, 128, true},
		{"128 over 4 exact", memberTable("25.0%", "25.0%", "25.0%", "25.0%"), 4, 128, false},
		{"single node owns everything", memberTable("100.0%"), 1, 128, false},
	}
	for _, c := range cases {
		got := ringImbalance(c.table, c.nodes, c.ringSize)
		if (got != "") != c.wantBad {
			t.Errorf("%s: ringImbalance = %q, wantBad=%v", c.name, got, c.wantBad)
		}
	}
	if got := ringImbalance(memberTable("50.0%", "50.0%"), 3, 128); !strings.Contains(got, "lists 2 nodes") {
		t.Errorf("a missing node must be reported, got %q", got)
	}
}

func TestParseGrants_wrappedAndAny(t *testing.T) {
	out := `Dedicated permissions (user/u)

+------+------+------+
| type |bucket|grants|
+------+------+------+
|  *   |  *   |riak_kv.get|
|t1    |b1    |riak_kv.put,|
|      |      |riak_kv.get|
+------+------+------+

Cumulative permissions (user/u)
`
	got := parseGrants(out)
	if !got["* *"]["riak_kv.get"] || !got["t1 b1"]["riak_kv.put"] || !got["t1 b1"]["riak_kv.get"] {
		t.Errorf("unexpected parse: %+v", got)
	}
}

// realMemberStatus is verbatim `riak-admin member-status` output from a 3-node,
// ring_size 128 cluster.
const realMemberStatus = `================================= Membership ==================================
Status     Ring    Pending    Node
-------------------------------------------------------------------------------
valid      33.6%      --      riak@scale-c000-0.scale-c000-headless.scale-test.svc.cluster.local
valid      32.8%      --      riak@scale-c000-1.scale-c000-headless.scale-test.svc.cluster.local
valid      33.6%      --      riak@scale-c000-2.scale-c000-headless.scale-test.svc.cluster.local
-------------------------------------------------------------------------------
Valid:3 / Leaving:0 / Exiting:0 / Joining:0 / Down:0
`

func TestRingImbalance_realOutput(t *testing.T) {
	if got := ringImbalance(realMemberStatus, 3, 128); got != "" {
		t.Errorf("a 43/42/43 ring over 3 nodes is balanced, got %q", got)
	}
}
