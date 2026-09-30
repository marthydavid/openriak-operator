package riak

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------- helpers ----------

func clusterWithMembers(members ...string) *riakv1.RiakCluster {
	c := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "riak", Namespace: "default"},
		Status:     riakv1.RiakClusterStatus{Phase: riakv1.PhaseReady},
	}
	for _, m := range members {
		c.Status.Members = append(c.Status.Members, riakv1.RiakNodeMember{Pod: m, Name: m})
	}
	return c
}

func emptyCluster() *riakv1.RiakCluster {
	return clusterWithMembers()
}

func newManager(runner func(context.Context, string, ...string) (string, error)) *Manager {
	exec := newTestExecutor(runner)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewManager(exec, client, logr.Discard())
}

// ---------- GetClusterStatus ----------

func TestGetClusterStatus_noMembers(t *testing.T) {
	m := newManager(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	_, err := m.GetClusterStatus(context.Background(), emptyCluster())
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestGetClusterStatus_withMember(t *testing.T) {
	runner, _ := mockRunner(map[string]string{"status": "riak is running"}, nil)
	m := newManager(runner)

	out, err := m.GetClusterStatus(context.Background(), clusterWithMembers("pod-0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "riak is running") {
		t.Errorf("unexpected output: %q", out)
	}
}

// ---------- CreateBucketType ----------

func TestCreateBucketType_noMembers(t *testing.T) {
	m := newManager(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	err := m.CreateBucketType(context.Background(), emptyCluster(), "mytype", nil)
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestCreateBucketType_withMember(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"bucket-type": ""}, nil)
	m := newManager(runner)

	err := m.CreateBucketType(context.Background(), clusterWithMembers("pod-0"), "mytype", map[string]string{"n_val": "3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var sawCreate bool
	for _, c := range *calls {
		if strings.Contains(strings.Join(c.args, " "), "bucket-type create") {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Error("expected bucket-type create to be called")
	}
}

// ---------- GrantUserPermissions ----------

func TestGrantUserPermissions_noMembers(t *testing.T) {
	m := newManager(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	grants := []riakv1.Grant{{Resource: "any", Permission: "read"}}
	err := m.GrantUserPermissions(context.Background(), emptyCluster(), "alice", grants)
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestGrantUserPermissions_empty(t *testing.T) {
	runner, calls := mockRunner(nil, nil)
	m := newManager(runner)
	// No grants: nothing to do, and no cluster-member requirement.
	if err := m.GrantUserPermissions(context.Background(), emptyCluster(), "alice", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no calls, got %d", len(*calls))
	}
}

func TestGrantUserPermissions_batchesByTarget(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	m := newManager(runner)

	// Two grants on "any" collapse into one call; a bucket grant is a second.
	grants := []riakv1.Grant{
		{Resource: "any", Permission: "read"},
		{Resource: "any", Permission: "write"},
		{Resource: "bucket", Permission: "read", BucketName: "mytype"},
	}
	if err := m.GrantUserPermissions(context.Background(), clusterWithMembers("pod-0"), "alice", grants); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 batched calls, got %d", len(*calls))
	}
	anyCall := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(anyCall, "security grant riak_kv.get,riak_kv.put on any to alice") {
		t.Errorf("unexpected 'any' call: %s", anyCall)
	}
	bucketCall := strings.Join((*calls)[1].args, " ")
	if !strings.Contains(bucketCall, "security grant riak_kv.get on mytype to alice") {
		t.Errorf("unexpected bucket call: %s", bucketCall)
	}
}

// ---------- CreateUserForCert ----------

func TestManagerCreateUserForCert_noMembers(t *testing.T) {
	m := newManager(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	err := m.CreateUserForCert(context.Background(), emptyCluster(), "certuser")
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestManagerCreateUserForCert_withMember(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security": ""}, nil)
	m := newManager(runner)

	err := m.CreateUserForCert(context.Background(), clusterWithMembers("pod-0"), "certuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var sawAddUser bool
	for _, c := range *calls {
		if strings.Contains(strings.Join(c.args, " "), "add-user certuser") {
			sawAddUser = true
		}
	}
	if !sawAddUser {
		t.Error("expected add-user certuser to be called")
	}
}

// ---------- AddSecuritySource ----------

func TestManagerAddSecuritySource_noMembers(t *testing.T) {
	m := newManager(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	err := m.AddSecuritySource(context.Background(), emptyCluster(), "certuser")
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestManagerAddSecuritySource_withMember(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security add-source": ""}, nil)
	m := newManager(runner)

	err := m.AddSecuritySource(context.Background(), clusterWithMembers("pod-0"), "certuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "security add-source certuser 0.0.0.0/0 certificate") {
		t.Errorf("unexpected call args: %s", joined)
	}
}

// ---------- ConfigureNode ----------

func TestConfigureNode_logsErrorAndContinues(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		return "", errors.New("config set failed")
	}
	m := newManager(runner)

	// Should return nil even when all config set calls fail (errors are logged, not returned).
	err := m.ConfigureNode(context.Background(), "ns", "pod-0", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("ConfigureNode should not return an error on config set failure, got: %v", err)
	}
}

func TestConfigureNode_setsEachKey(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"config set": ""}, nil)
	m := newManager(runner)

	cfg := map[string]string{"ring_size": "64", "storage_backend": "bitcask"}
	if err := m.ConfigureNode(context.Background(), "ns", "pod-0", cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should have one call per config key
	if len(*calls) != 2 {
		t.Errorf("expected 2 config set calls, got %d", len(*calls))
	}
}

// ---------- ReconcileMembership ----------

const (
	seedNode = "riak@riak-0.riak-headless.default.svc.cluster.local"
	soloFmt  = "================================= Membership ==================================\n" +
		"Status     Ring    Pending    Node\n" +
		"-------------------------------------------------------------------------------\n" +
		"valid     100.0%%      --      %s\n" +
		"-------------------------------------------------------------------------------\n" +
		"Valid:1 / Leaving:0 / Exiting:0 / Joining:0 / Down:0\n"
)

func memberTable(nodes ...string) string {
	out := "Status     Ring    Pending    Node\n---------------\n"
	for _, n := range nodes {
		out += "valid      33.3%      --      " + n + "\n"
	}
	return out
}

func newMembershipManager(runner func(context.Context, string, ...string) (string, error)) *Manager {
	return NewManager(newTestExecutor(runner), nil, logr.Discard())
}

func membershipCluster() *riakv1.RiakCluster {
	return &riakv1.RiakCluster{ObjectMeta: metav1.ObjectMeta{Name: "riak", Namespace: "default"}}
}

func nodeOf(pod string) string {
	return "riak@" + pod + ".riak-headless.default.svc.cluster.local"
}

func TestReconcileMembership_joinsStandaloneNodes(t *testing.T) {
	var cmds []string
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		cmds = append(cmds, joined)
		if strings.HasSuffix(joined, "member-status") {
			// every pod is its own ring
			for _, p := range []string{"riak-0", "riak-1", "riak-2"} {
				if strings.Contains(joined, " "+p+" ") {
					return memberTable(nodeOf(p)), nil
				}
			}
		}
		return "", nil
	}
	formed, err := newMembershipManager(runner).ReconcileMembership(
		context.Background(), membershipCluster(), []string{"riak-0", "riak-1", "riak-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if formed {
		t.Error("cluster must not be reported formed in the same pass that staged joins")
	}
	var joins, plan, commit int
	for _, c := range cmds {
		switch {
		case strings.Contains(c, "cluster join "+seedNode):
			joins++
			if !strings.Contains(c, " riak-1 ") && !strings.Contains(c, " riak-2 ") {
				t.Errorf("join must run on the joining node, ran: %s", c)
			}
		case strings.HasSuffix(c, "cluster plan"):
			plan++
		case strings.HasSuffix(c, "cluster commit"):
			commit++
		}
	}
	if joins != 2 || plan != 1 || commit != 1 {
		t.Errorf("want 2 joins, 1 plan, 1 commit; got %d/%d/%d\n%s", joins, plan, commit, strings.Join(cmds, "\n"))
	}
}

func TestReconcileMembership_alreadyFormed(t *testing.T) {
	var cmds []string
	all := memberTable(nodeOf("riak-0"), nodeOf("riak-1"), nodeOf("riak-2"))
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		cmds = append(cmds, strings.Join(args, " "))
		return all, nil
	}
	formed, err := newMembershipManager(runner).ReconcileMembership(
		context.Background(), membershipCluster(), []string{"riak-0", "riak-1", "riak-2"})
	if err != nil || !formed {
		t.Fatalf("want formed=true, got %v err=%v", formed, err)
	}
	for _, c := range cmds {
		if strings.Contains(c, "cluster join") || strings.Contains(c, "cluster plan") || strings.Contains(c, "cluster commit") {
			t.Errorf("an already formed cluster must not be touched, ran: %s", c)
		}
	}
}

func TestReconcileMembership_peerAlreadyInSeedRingIsNotRejoined(t *testing.T) {
	// riak-2 reports a ring of itself, but the seed already lists it as a member
	// (join committed, ring still propagating): it must not be joined again.
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, " riak-2 ") {
			return memberTable(nodeOf("riak-2")), nil
		}
		return memberTable(nodeOf("riak-0"), nodeOf("riak-1"), nodeOf("riak-2")), nil
	}
	formed, err := newMembershipManager(runner).ReconcileMembership(
		context.Background(), membershipCluster(), []string{"riak-0", "riak-1", "riak-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !formed {
		t.Error("riak-2 is already in the seed's ring, cluster is formed")
	}
}

func TestReconcileMembership_seedMemberStatusError(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", errors.New("exec failed")
	}
	if _, err := newMembershipManager(runner).ReconcileMembership(
		context.Background(), membershipCluster(), []string{"riak-0", "riak-1"}); err == nil {
		t.Fatal("expected an error when the seed cannot be queried")
	}
}

func TestReconcileMembership_seedNotInMemberStatus(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) (string, error) {
		return memberTable("riak@other"), nil
	}
	if _, err := newMembershipManager(runner).ReconcileMembership(
		context.Background(), membershipCluster(), []string{"riak-0", "riak-1"}); err == nil {
		t.Fatal("expected an error when the seed node is not listed")
	}
}

func TestReconcileMembership_joinError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "cluster join") {
			return "", errors.New("join failed")
		}
		if strings.Contains(joined, " riak-1 ") {
			return memberTable(nodeOf("riak-1")), nil
		}
		return memberTable(nodeOf("riak-0")), nil
	}
	if _, err := newMembershipManager(runner).ReconcileMembership(
		context.Background(), membershipCluster(), []string{"riak-0", "riak-1"}); err == nil {
		t.Fatal("expected join error")
	}
}

func TestReconcileMembership_planAndCommitErrors(t *testing.T) {
	for _, failing := range []string{"cluster plan", "cluster commit"} {
		runner := func(_ context.Context, _ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if strings.HasSuffix(joined, failing) {
				return "", errors.New("boom")
			}
			if strings.Contains(joined, " riak-1 ") && strings.HasSuffix(joined, "member-status") {
				return memberTable(nodeOf("riak-1")), nil
			}
			if strings.HasSuffix(joined, "member-status") {
				return memberTable(nodeOf("riak-0")), nil
			}
			return "", nil
		}
		if _, err := newMembershipManager(runner).ReconcileMembership(
			context.Background(), membershipCluster(), []string{"riak-0", "riak-1"}); err == nil {
			t.Errorf("expected error when %q fails", failing)
		}
	}
}

func TestReconcileMembership_peerMemberStatusError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), " riak-1 ") {
			return "", errors.New("peer down")
		}
		return memberTable(nodeOf("riak-0")), nil
	}
	if _, err := newMembershipManager(runner).ReconcileMembership(
		context.Background(), membershipCluster(), []string{"riak-0", "riak-1"}); err == nil {
		t.Fatal("expected error when a peer cannot be queried")
	}
}

func TestParseMemberStatus(t *testing.T) {
	out := fmt.Sprintf(soloFmt, "riak@a.b.c")
	got := parseMemberStatus(out)
	if len(got) != 1 || got[0].Status != "valid" || got[0].Node != "riak@a.b.c" {
		t.Fatalf("unexpected parse: %+v", got)
	}
	if got := parseMemberStatus("garbage\nnot riak@x a b\n"); len(got) != 0 {
		t.Fatalf("expected no members, got %+v", got)
	}
}
