package riak

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

// capturedCall records a single invocation of the mock runner.
type capturedCall struct {
	name string
	args []string
}

// mockRunner returns a runner that records calls and returns canned responses.
// responses maps a substring of the full args string → (output, error).
// If no key matches, the runner returns ("", nil) by default.
func mockRunner(responses map[string]string, errs map[string]error) (func(context.Context, string, ...string) (string, error), *[]capturedCall) {
	calls := &[]capturedCall{}
	fn := func(_ context.Context, name string, args ...string) (string, error) {
		*calls = append(*calls, capturedCall{name: name, args: args})
		joined := strings.Join(args, " ")
		for key, out := range responses {
			if strings.Contains(joined, key) {
				if errs != nil {
					if e, ok := errs[key]; ok {
						return "", e
					}
				}
				return out, nil
			}
		}
		if errs != nil {
			for key, e := range errs {
				if strings.Contains(joined, key) {
					return "", e
				}
			}
		}
		return "", nil
	}
	return fn, calls
}

func newTestExecutor(runner func(context.Context, string, ...string) (string, error)) *Executor {
	return &Executor{log: logr.Discard(), runnerFn: runner}
}

// ---------- NewExecutor / runShellCommand ----------

func TestNewExecutor_setsRunner(t *testing.T) {
	e := NewExecutor(logr.Discard())
	if e == nil {
		t.Fatal("expected non-nil executor")
	}
	if e.runnerFn == nil {
		t.Fatal("expected runnerFn to be set")
	}
}

func TestRunShellCommand_success(t *testing.T) {
	out, err := runShellCommand(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello" {
		t.Errorf("expected 'hello', got %q", out)
	}
}

func TestRunShellCommand_failure(t *testing.T) {
	_, err := runShellCommand(context.Background(), "false")
	if err == nil {
		t.Fatal("expected non-zero exit error, got nil")
	}
}

// ---------- ExecuteRiakAdmin ----------

func TestExecuteRiakAdmin_buildsCorrectArgs(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"status": "running"}, nil)
	e := newTestExecutor(runner)

	out, err := e.ExecuteRiakAdmin(context.Background(), "mynamespace", "mypod", "riak", "status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "running" {
		t.Errorf("want output 'running', got %q", out)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	c := (*calls)[0]
	if c.name != "kubectl" {
		t.Errorf("expected kubectl, got %q", c.name)
	}
	wantArgs := []string{"exec", "-n", "mynamespace", "mypod", "-c", "riak", "--", "riak-admin", "status"}
	for i, a := range wantArgs {
		if i >= len(c.args) || c.args[i] != a {
			t.Errorf("arg[%d]: want %q, got %q", i, a, c.args[i])
		}
	}
}

func TestExecuteRiakAdmin_propagatesError(t *testing.T) {
	runner, _ := mockRunner(nil, map[string]error{"status": fmt.Errorf("exit status 1")})
	e := newTestExecutor(runner)

	_, err := e.ExecuteRiakAdmin(context.Background(), "ns", "pod", "riak", "status")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "riak-admin failed") {
		t.Errorf("expected 'riak-admin failed' in error, got: %v", err)
	}
}

// ---------- GetClusterMembers ----------

func TestGetClusterMembers_parsesOutput(t *testing.T) {
	memberOutput := `
Status     Ring    Pending    Node
--------------------------------------
valid      20.3%   --         riak@node1.cluster.svc
valid      20.3%   --         riak@node2.cluster.svc
valid      20.3%   --         riak@node3.cluster.svc
`
	runner, _ := mockRunner(map[string]string{"member-status": memberOutput}, nil)
	e := newTestExecutor(runner)

	members, err := e.GetClusterMembers(context.Background(), "ns", "pod", "riak")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 3 {
		t.Errorf("expected 3 members, got %d: %v", len(members), members)
	}
}

func TestGetClusterMembers_emptyOutput(t *testing.T) {
	runner, _ := mockRunner(map[string]string{"member-status": ""}, nil)
	e := newTestExecutor(runner)

	members, err := e.GetClusterMembers(context.Background(), "ns", "pod", "riak")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("expected 0 members, got %d", len(members))
	}
}

func TestGetClusterMembers_propagatesError(t *testing.T) {
	runner, _ := mockRunner(nil, map[string]error{"member-status": errors.New("connection refused")})
	e := newTestExecutor(runner)

	_, err := e.GetClusterMembers(context.Background(), "ns", "pod", "riak")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------- PlanCluster / CommitCluster / GetStatus ----------

func TestPlanCluster(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"cluster plan": "Success"}, nil)
	e := newTestExecutor(runner)

	out, err := e.PlanCluster(context.Background(), "ns", "pod", "riak", "plan")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Success" {
		t.Errorf("want 'Success', got %q", out)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "cluster plan") {
		t.Errorf("expected 'cluster plan' in args: %s", joined)
	}
}

func TestCommitCluster(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"cluster commit": "Committed"}, nil)
	e := newTestExecutor(runner)

	out, err := e.CommitCluster(context.Background(), "ns", "pod", "riak")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Committed" {
		t.Errorf("want 'Committed', got %q", out)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "cluster commit") {
		t.Errorf("expected 'cluster commit' in args: %s", joined)
	}
}

func TestGetStatus(t *testing.T) {
	runner, _ := mockRunner(map[string]string{"riak-admin status": "riak is running"}, nil)
	e := newTestExecutor(runner)

	out, err := e.GetStatus(context.Background(), "ns", "pod", "riak")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = out
}

// ---------- CreateBucket ----------

func TestCreateBucket_sendsJSONProps(t *testing.T) {
	var createdWith string
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "bucket-type create") {
			// capture the JSON argument (last element after "create <type>")
			for i, a := range args {
				if a == "create" && i+2 < len(args) {
					createdWith = args[i+2]
				}
			}
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	props := map[string]string{"n_val": "3", "allow_mult": "false"}
	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "mybucket", "", props); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(createdWith, `"props"`) {
		t.Errorf("expected JSON with 'props' key, got: %s", createdWith)
	}
	// n_val should be numeric 3, not string "3"
	if strings.Contains(createdWith, `"n_val":"3"`) {
		t.Errorf("n_val should be numeric, not a string: %s", createdWith)
	}
	if !strings.Contains(createdWith, `"n_val":3`) {
		t.Errorf("expected n_val:3 in JSON, got: %s", createdWith)
	}
}

func TestCreateBucket_emptyProps(t *testing.T) {
	runner, _ := mockRunner(map[string]string{"bucket-type": ""}, nil)
	e := newTestExecutor(runner)

	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "mytype", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateBucket_ignoresAlreadyExistsError(t *testing.T) {
	runner, _ := mockRunner(nil, map[string]error{"bucket-type": errors.New("Error: bucket type already exists")})
	e := newTestExecutor(runner)

	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "existing", "", nil); err != nil {
		t.Fatalf("expected no error for already-exists, got: %v", err)
	}
}

func TestCreateBucket_returnsCreateError(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		callCount++
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "bucket-type create") {
			return "", errors.New("network error")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "mytype", "", nil)
	if err == nil {
		t.Fatal("expected error from create, got nil")
	}
}

func TestCreateBucket_returnsActivateError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "bucket-type activate") {
			return "", errors.New("activation failed")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "mytype", "", nil)
	if err == nil {
		t.Fatal("expected error from activate, got nil")
	}
}

// ---------- CreateUser ----------

func TestCreateUser_enablesSecurityAndAddsUser(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security": ""}, nil)
	e := newTestExecutor(runner)

	if err := e.CreateUser(context.Background(), "ns", "pod", "riak", "alice", "secret"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var hasEnable, hasAddUser bool
	for _, c := range *calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "security enable") {
			hasEnable = true
		}
		if strings.Contains(joined, "security add-user alice") {
			hasAddUser = true
		}
	}
	if !hasEnable {
		t.Error("expected 'security enable' call")
	}
	if !hasAddUser {
		t.Error("expected 'security add-user alice' call")
	}
}

func TestCreateUser_failsOnNonAlreadyEnableError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "security enable") {
			return "", errors.New("network timeout")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	err := e.CreateUser(context.Background(), "ns", "pod", "riak", "alice", "pwd")
	if err == nil {
		t.Fatal("expected error from non-already enable failure, got nil")
	}
}

func TestCreateUser_ignoresAlreadyEnabledError(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		callCount++
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "security enable") {
			return "", errors.New("security already enabled")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	if err := e.CreateUser(context.Background(), "ns", "pod", "riak", "alice", "pwd"); err != nil {
		t.Fatalf("unexpected error for already-enabled: %v", err)
	}
}

func TestCreateUser_returnsAddUserError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "add-user") {
			return "", errors.New("user creation failed")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	err := e.CreateUser(context.Background(), "ns", "pod", "riak", "alice", "pwd")
	if err == nil {
		t.Fatal("expected error from add-user, got nil")
	}
}

// ---------- GrantPermission ----------

func TestGrantPermission_nosBucket(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	e := newTestExecutor(runner)

	if err := e.GrantPermission(context.Background(), "ns", "pod", "riak", "alice", "any", "read", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "security grant read on any to alice") {
		t.Errorf("unexpected args: %s", joined)
	}
}

func TestGrantPermission_withBucket(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	e := newTestExecutor(runner)

	if err := e.GrantPermission(context.Background(), "ns", "pod", "riak", "alice", "bucket", "write", "mybucket"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "mybucket") {
		t.Errorf("expected bucket name in args: %s", joined)
	}
}

func TestGrantPermission_propagatesError(t *testing.T) {
	runner, _ := mockRunner(nil, map[string]error{"security grant": errors.New("grant failed")})
	e := newTestExecutor(runner)

	err := e.GrantPermission(context.Background(), "ns", "pod", "riak", "alice", "any", "read", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
