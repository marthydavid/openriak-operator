package main

import "testing"

func TestParseAndCheckMetrics(t *testing.T) {
	body := `# HELP riak_node_gets_total x
# TYPE riak_node_gets_total untyped
riak_node_gets_total 12
riak_node_puts_total 3
riak_vnode_gets_total{a="b"} 7
riak_ring_num_partitions 128
riak_memory_system 1.5e+08
riak_memory_processes 2e+07
riak_sys_process_count 400
riak_node_get_fsm_time_95 0
`
	m := parseMetrics(body)
	if m["riak_vnode_gets_total"] != 7 || m["riak_memory_system"] != 1.5e8 {
		t.Fatalf("parse: %v", m)
	}
	if bad := checkMetrics(m, 128); len(bad) != 0 {
		t.Fatalf("unexpected problems: %v", bad)
	}
	delete(m, "riak_node_gets_total")
	m["riak_ring_num_partitions"] = 64
	if bad := checkMetrics(m, 128); len(bad) != 2 {
		t.Fatalf("want 2 problems, got %v", bad)
	}
}

func TestCheckMetrics_ringSizeFollowsTheCluster(t *testing.T) {
	m := map[string]float64{
		"riak_node_gets_total": 0, "riak_node_puts_total": 0, "riak_vnode_gets_total": 0,
		"riak_ring_num_partitions": 256, "riak_memory_system": 1, "riak_memory_processes": 1,
		"riak_sys_process_count": 1, "riak_node_get_fsm_time_95": 0,
	}
	if bad := checkMetrics(m, 256); len(bad) != 0 {
		t.Fatalf("a 256-partition ring on a ring_size 256 cluster is fine: %v", bad)
	}
	if bad := checkMetrics(m, 128); len(bad) != 1 {
		t.Fatalf("want exactly the ring-size mismatch, got %v", bad)
	}
}

func TestCheckMetrics_rejectsImplausibleValues(t *testing.T) {
	m := map[string]float64{
		"riak_node_gets_total": -1, "riak_node_puts_total": 0, "riak_vnode_gets_total": 0,
		"riak_ring_num_partitions": 128, "riak_memory_system": 0, "riak_memory_processes": 1,
		"riak_sys_process_count": 1, "riak_node_get_fsm_time_95": 0,
	}
	if bad := checkMetrics(m, 128); len(bad) != 2 {
		t.Fatalf("want zero memory and negative counter reported, got %v", bad)
	}
}

func TestExerciseDeltas(t *testing.T) {
	before := map[string]map[string]float64{
		"c-0": {"riak_node_puts_total": 1, "riak_node_gets_total": 2, "riak_vnode_puts_total": 1},
		"c-1": {"riak_vnode_puts_total": 1},
		"c-2": {"riak_vnode_puts_total": 1},
	}
	good := map[string]map[string]float64{
		"c-0": {"riak_node_puts_total": 2, "riak_node_gets_total": 4, "riak_vnode_puts_total": 2},
		"c-1": {"riak_vnode_puts_total": 2},
		"c-2": {"riak_vnode_puts_total": 2},
	}
	if bad := exerciseDeltas(before, good, "c-0"); len(bad) != 0 {
		t.Fatalf("a write replicated to 3 vnodes is healthy: %v", bad)
	}
	// Only the coordinator counted: the replicas' vnode puts did not move.
	lonely := map[string]map[string]float64{
		"c-0": {"riak_node_puts_total": 2, "riak_node_gets_total": 4, "riak_vnode_puts_total": 2},
		"c-1": {"riak_vnode_puts_total": 1},
		"c-2": {"riak_vnode_puts_total": 1},
	}
	if bad := exerciseDeltas(before, lonely, "c-0"); len(bad) != 1 {
		t.Fatalf("want the missing replication reported, got %v", bad)
	}
	// Nothing moved at all.
	if bad := exerciseDeltas(before, before, "c-0"); len(bad) != 3 {
		t.Fatalf("want puts, gets and replication reported, got %v", bad)
	}
}
