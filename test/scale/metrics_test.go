package main

import "testing"

func TestParseAndCheckMetrics(t *testing.T) {
	body := `# HELP riak_node_gets_total x
# TYPE riak_node_gets_total untyped
riak_node_gets_total 12
riak_node_puts_total 3
riak_vnode_gets_total{a="b"} 7
riak_ring_num_partitions 8
riak_memory_system 1.5e+08
`
	m := parseMetrics(body)
	if m["riak_vnode_gets_total"] != 7 || m["riak_memory_system"] != 1.5e8 {
		t.Fatalf("parse: %v", m)
	}
	if bad := checkMetrics(m); len(bad) != 0 {
		t.Fatalf("unexpected problems: %v", bad)
	}
	delete(m, "riak_node_gets_total")
	m["riak_ring_num_partitions"] = 64
	if bad := checkMetrics(m); len(bad) != 2 {
		t.Fatalf("want 2 problems, got %v", bad)
	}
}
