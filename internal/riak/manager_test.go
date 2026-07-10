package riak

import (
	"context"
	"errors"
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

// ---------- CreateUser ----------

func TestCreateUser_noMembers(t *testing.T) {
	m := newManager(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	err := m.CreateUser(context.Background(), emptyCluster(), "alice", "pass")
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestCreateUser_withMember(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security": ""}, nil)
	m := newManager(runner)

	err := m.CreateUser(context.Background(), clusterWithMembers("pod-0"), "alice", "secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var sawAddUser bool
	for _, c := range *calls {
		if strings.Contains(strings.Join(c.args, " "), "add-user alice") {
			sawAddUser = true
		}
	}
	if !sawAddUser {
		t.Error("expected add-user alice to be called")
	}
}

// ---------- GrantUserPermission ----------

func TestGrantUserPermission_noMembers(t *testing.T) {
	m := newManager(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	err := m.GrantUserPermission(context.Background(), emptyCluster(), "alice", "any", "read", "")
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestGrantUserPermission_withMember(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	m := newManager(runner)

	err := m.GrantUserPermission(context.Background(), clusterWithMembers("pod-0"), "alice", "any", "read", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "security grant riak_kv.get on any to alice") {
		t.Errorf("unexpected call args: %s", joined)
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
	err := m.AddSecuritySource(context.Background(), emptyCluster(), "certuser", "certificate")
	if err == nil || !strings.Contains(err.Error(), "no cluster members") {
		t.Fatalf("expected 'no cluster members' error, got: %v", err)
	}
}

func TestManagerAddSecuritySource_withMember(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security add-source": ""}, nil)
	m := newManager(runner)

	err := m.AddSecuritySource(context.Background(), clusterWithMembers("pod-0"), "certuser", "certificate")
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

// ---------- InitializeCluster ----------

func TestInitializeCluster_noPods(t *testing.T) {
	runner, _ := mockRunner(nil, nil)
	exec := newTestExecutor(runner)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := NewManager(exec, client, logr.Discard())

	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "riak", Namespace: "default"},
	}
	err := m.InitializeCluster(context.Background(), cluster)
	if err == nil || !strings.Contains(err.Error(), "no cluster pods") {
		t.Fatalf("expected 'no cluster pods' error, got: %v", err)
	}
}

func TestInitializeCluster_singlePod(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"cluster": ""}, nil)
	exec := newTestExecutor(runner)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "riak-0",
			Namespace: "default",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&pod).Build()
	m := NewManager(exec, client, logr.Discard())

	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "riak", Namespace: "default"},
	}
	// Single pod: no joins needed, but plan+commit should be called.
	err := m.InitializeCluster(context.Background(), cluster)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var sawPlan, sawCommit bool
	for _, c := range *calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "cluster plan") {
			sawPlan = true
		}
		if strings.Contains(joined, "cluster commit") {
			sawCommit = true
		}
	}
	if !sawPlan {
		t.Error("expected cluster plan call")
	}
	if !sawCommit {
		t.Error("expected cluster commit call")
	}
}

func TestInitializeCluster_multiplePods_joinsNonSeed(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"cluster": ""}, nil)
	exec := newTestExecutor(runner)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "riak-0", Namespace: "default"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "riak-1", Namespace: "default"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "riak-2", Namespace: "default"}},
	).Build()
	m := NewManager(exec, client, logr.Discard())

	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "riak", Namespace: "default"},
	}
	if err := m.InitializeCluster(context.Background(), cluster); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joinCount := 0
	for _, c := range *calls {
		if strings.Contains(strings.Join(c.args, " "), "cluster join") {
			joinCount++
		}
	}
	if joinCount != 2 {
		t.Errorf("expected 2 join calls (for riak-1 and riak-2), got %d", joinCount)
	}
}

func TestInitializeCluster_commitError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "cluster commit") {
			return "", errors.New("commit failed")
		}
		return "", nil
	}
	exec := newTestExecutor(runner)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "riak-0", Namespace: "default"}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	m := NewManager(exec, client, logr.Discard())

	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "riak", Namespace: "default"},
	}
	err := m.InitializeCluster(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected error from commit, got nil")
	}
}

func TestInitializeCluster_planError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "cluster plan") {
			return "", errors.New("plan failed")
		}
		return "", nil
	}
	exec := newTestExecutor(runner)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "riak-0", Namespace: "default"}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	m := NewManager(exec, client, logr.Discard())

	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "riak", Namespace: "default"},
	}
	err := m.InitializeCluster(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected error from plan, got nil")
	}
}
