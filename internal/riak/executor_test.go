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
		for key, e := range errs {
			if strings.Contains(joined, key) {
				return "", e
			}
		}
		return "", nil
	}
	return fn, calls
}

func newTestExecutor(runner func(context.Context, string, ...string) (string, error)) *Executor {
	return NewExecutorWithRunner(logr.Discard(), runner)
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
	wantPrefix := []string{"exec", "-n", "mynamespace", "mypod", "-c", "riak", "--", "sh", "-c"}
	for i, a := range wantPrefix {
		if i >= len(c.args) || c.args[i] != a {
			t.Fatalf("arg[%d]: want %q, got %v", i, a, c.args)
		}
	}
	// The script must point riak-admin at the generated vm.args, and the real
	// riak-admin arguments follow it as positional parameters ($0 = riak-admin).
	script := c.args[len(wantPrefix)]
	if !strings.Contains(script, "VMARGS_PATH=") || !strings.Contains(script, "exec riak-admin") {
		t.Errorf("unexpected wrapper script: %q", script)
	}
	if got := c.args[len(wantPrefix)+1:]; len(got) != 2 || got[0] != "riak-admin" || got[1] != "status" {
		t.Errorf("want trailing args [riak-admin status], got %v", got)
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

// Real riak-admin output, captured from Riak 3.2.6. riak-admin exits 0 on
// failure, so these replies arrive as successful command output.
const (
	outCreateAlreadyActive = "Error creating bucket type t1:\nalready_active\nerror"
	outCreateDefaultType   = "Error creating bucket type default:\ndefault_type\nerror"
	outUpdated             = "t1 updated\nok"
	cmdCreate              = "create"
	cmdUpdate              = "update"
)

// bucketTypeRunner answers each bucket-type subcommand with a fixed output and
// records the subcommands that ran.
func bucketTypeRunner(outputs map[string]string, ran *[]string) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		for i, a := range args {
			if a == "bucket-type" && i+1 < len(args) {
				*ran = append(*ran, args[i+1])
				return outputs[args[i+1]], nil
			}
		}
		return "", nil
	}
}

func TestCreateBucket_updatesAnActiveType(t *testing.T) {
	var ran []string
	e := newTestExecutor(bucketTypeRunner(map[string]string{
		cmdCreate: outCreateAlreadyActive, cmdUpdate: outUpdated}, &ran))

	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "t1", "", map[string]string{"n_val": "2"}); err != nil {
		t.Fatalf("expected the existing type to be updated, got: %v", err)
	}
	if strings.Join(ran, ",") != cmdCreate+",update" {
		t.Errorf("expected create then update, got %v", ran)
	}
}

func TestCreateBucket_activeTypeWithoutPropsIsDone(t *testing.T) {
	var ran []string
	e := newTestExecutor(bucketTypeRunner(map[string]string{cmdCreate: outCreateAlreadyActive}, &ran))

	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "t1", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(ran, ",") != cmdCreate {
		t.Errorf("expected only create, got %v", ran)
	}
}

func TestCreateBucket_defaultTypeIsUpdated(t *testing.T) {
	var ran []string
	e := newTestExecutor(bucketTypeRunner(map[string]string{
		cmdCreate: outCreateDefaultType, cmdUpdate: outUpdated}, &ran))

	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "default", "", map[string]string{"allow_mult": "true"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(ran, ",") != cmdCreate+",update" {
		t.Errorf("expected create then update, got %v", ran)
	}
}

func TestCreateBucket_returnsUpdateError(t *testing.T) {
	var ran []string
	e := newTestExecutor(bucketTypeRunner(map[string]string{
		cmdCreate: outCreateAlreadyActive,
		cmdUpdate: "Error updating bucket type t1:\nWrite once buckets must not be consistent=true\nerror"}, &ran))

	err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "t1", "", map[string]string{"consistent": "true"})
	if err == nil || !strings.Contains(err.Error(), "consistent=true") {
		t.Fatalf("expected the update error with Riak's reason, got: %v", err)
	}
}

func TestCreateBucket_returnsRejectedCreate(t *testing.T) {
	var ran []string
	e := newTestExecutor(bucketTypeRunner(map[string]string{
		cmdCreate: "Cannot create bucket type t1: invalid json\nerror"}, &ran))

	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "t1", "", nil); err == nil {
		t.Fatal("expected a create rejected by Riak to be an error")
	}
	if strings.Join(ran, ",") != cmdCreate {
		t.Errorf("activate must not run after a failed create, got %v", ran)
	}
}

func TestCreateBucket_activateAlreadyActiveIsOK(t *testing.T) {
	var ran []string
	e := newTestExecutor(bucketTypeRunner(map[string]string{
		cmdCreate: "t1 created\nok", "activate": "t1 has been activated\nok"}, &ran))

	if err := e.CreateBucket(context.Background(), "ns", "pod", "riak", "t1", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(ran, ",") != cmdCreate+",activate" {
		t.Errorf("expected create then activate, got %v", ran)
	}
}

// ---------- riak-admin failure detection ----------

func TestRiakAdminFailed(t *testing.T) {
	cases := []struct {
		out  string
		fail bool
	}{
		{"t1 created\nok", false},
		{"", false},
		{"Successfully granted\nok\n", false},
		{outCreateAlreadyActive, true},
		{"Name(s) not recognized: bob\n{error,{unknown_roles,[<<\"bob\">>]}}", true},
		// add-user of an existing user: the error term, then a table and "ok".
		{"This name is already in use\n{error,role_exists}\n\n| username |\nok", true},
		{"nosuch is not an existing bucket type\n{error,undefined}", true},
		{"an error occurred earlier\nok", false},
	}
	for _, c := range cases {
		if got := riakAdminFailed(c.out); got != c.fail {
			t.Errorf("riakAdminFailed(%q) = %v, want %v", c.out, got, c.fail)
		}
	}
}

func TestExecuteRiakAdmin_reportsFailureInOutput(t *testing.T) {
	e := newTestExecutor(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "User(s) not recognized: bob\n{error,{unknown_users,[<<\"bob\">>]}}", nil
	})
	_, err := e.ExecuteRiakAdmin(context.Background(), "ns", "pod", "riak", "security", "add-source", "bob", "0.0.0.0/0", "certificate")
	if err == nil {
		t.Fatal("expected an error for a riak-admin reply carrying {error,...}")
	}
	if !strings.Contains(err.Error(), "security add-source") || !strings.Contains(err.Error(), "unknown_users") {
		t.Errorf("error should name the subcommand and Riak's reason: %v", err)
	}
	if strings.Contains(err.Error(), "0.0.0.0/0") {
		t.Errorf("error should not echo the command arguments: %v", err)
	}
}

func TestCompactOutput_caps(t *testing.T) {
	if got := compactOutput(strings.Repeat("x ", 400)); len(got) != 303 || !strings.HasSuffix(got, "...") {
		t.Errorf("expected a 300-char capped message, got %d chars", len(got))
	}
}

func TestCreateUserForCert_existingUserIsOK(t *testing.T) {
	e := newTestExecutor(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "This name is already in use\n{error,role_exists}\nok", nil
	})
	if err := e.CreateUserForCert(context.Background(), "ns", "pod", "riak", "alice"); err != nil {
		t.Fatalf("an existing user must not be an error: %v", err)
	}
}

func TestDeleteUser_unknownUserIsOK(t *testing.T) {
	e := newTestExecutor(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "User not recognized: alice\n{error,{unknown_user,<<\"alice\">>}}", nil
	})
	if err := e.DeleteUser(context.Background(), "ns", "pod", "riak", "alice"); err != nil {
		t.Fatalf("deleting an unknown user must succeed: %v", err)
	}
}

func TestDeleteUser_returnsOtherErrors(t *testing.T) {
	e := newTestExecutor(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", errors.New("exec failed")
	})
	if err := e.DeleteUser(context.Background(), "ns", "pod", "riak", "alice"); err == nil {
		t.Fatal("expected the exec error")
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

// ---------- CreateUserForCert: add-user error ----------

func TestCreateUserForCert_returnsAddUserError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "add-user") {
			return "", errors.New("user creation failed")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	err := e.CreateUserForCert(context.Background(), "ns", "pod", "riak", "alice")
	if err == nil {
		t.Fatal("expected error from add-user, got nil")
	}
}

// ---------- GrantPermissions (batched) ----------

func TestGrantPermissions_dedupesAndJoins(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	e := newTestExecutor(runner)

	// admin expands to a set that overlaps read/write; the union must be
	// de-duplicated into a single comma-joined token list in one call.
	if err := e.GrantPermissions(context.Background(), "ns", "pod", "riak", "alice",
		"any", "", []string{"read", "write", "admin"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "security grant riak_kv.get,riak_kv.put,riak_kv.delete,riak_kv.list_keys,riak_kv.list_buckets on any to alice") {
		t.Errorf("unexpected deduped args: %s", joined)
	}
}

func TestGrantPermissions_rejectsEmptyBucketTarget(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	e := newTestExecutor(runner)

	// A bucket grant with no bucket must error, not silently grant "on any".
	for _, bad := range []string{"", "   "} {
		err := e.GrantPermissions(context.Background(), "ns", "pod", "riak", "alice",
			"bucket", bad, []string{"read"})
		if err == nil || !strings.Contains(err.Error(), "bucket target") {
			t.Fatalf("bucket=%q: expected bucket-target error, got %v", bad, err)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("expected no security grant call, got %d", len(*calls))
	}
}

func TestGrantPermissions_rejectsUnknownResource(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	e := newTestExecutor(runner)

	err := e.GrantPermissions(context.Background(), "ns", "pod", "riak", "alice",
		"everything", "", []string{"read"})
	if err == nil || !strings.Contains(err.Error(), "unknown grant resource") {
		t.Fatalf("expected unknown-resource error, got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no security grant call, got %d", len(*calls))
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
	// "read" is mapped to the Riak KV permission riak_kv.get.
	if !strings.Contains(joined, "security grant riak_kv.get on any to alice") {
		t.Errorf("unexpected args: %s", joined)
	}
}

func TestGrantPermission_withBucket(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	e := newTestExecutor(runner)

	// resource=bucket with a bucket-type target grants on that type.
	if err := e.GrantPermission(context.Background(), "ns", "pod", "riak", "alice", "bucket", "write", "mybuckettype"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join((*calls)[0].args, " ")
	// "write" is mapped to riak_kv.put and the target is the bucket type.
	if !strings.Contains(joined, "security grant riak_kv.put on mybuckettype to alice") {
		t.Errorf("unexpected args: %s", joined)
	}
}

func TestGrantPermission_withBucketTypeAndBucket(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security grant": ""}, nil)
	e := newTestExecutor(runner)

	// A "<type> <bucket>" target scopes the grant to a single bucket in a type.
	if err := e.GrantPermission(context.Background(), "ns", "pod", "riak", "alice", "bucket", "read", "mytype mybucket"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "security grant riak_kv.get on mytype mybucket to alice") {
		t.Errorf("unexpected args: %s", joined)
	}
}

func TestRiakKVPermissions_mapsFriendlyNames(t *testing.T) {
	cases := map[string]string{
		"read":        "riak_kv.get",
		"write":       "riak_kv.put",
		"delete":      "riak_kv.delete",
		"list":        "riak_kv.list_keys,riak_kv.list_buckets",
		"admin":       "riak_kv.get,riak_kv.put,riak_kv.delete,riak_kv.list_keys,riak_kv.list_buckets",
		"riak_kv.get": "riak_kv.get", // unknown/already-qualified passes through
	}
	for in, want := range cases {
		if got := riakKVPermissions(in); got != want {
			t.Errorf("riakKVPermissions(%q) = %q, want %q", in, got, want)
		}
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

// ---------- CreateUserForCert ----------

func TestCreateUserForCert_addsUserWithoutEnableOrPassword(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security": ""}, nil)
	e := newTestExecutor(runner)

	if err := e.CreateUserForCert(context.Background(), "ns", "pod", "riak", "certuser"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var hasEnable, hasAddUser, passwordArg bool
	for _, c := range *calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "security enable") {
			hasEnable = true
		}
		if strings.Contains(joined, "add-user certuser") {
			hasAddUser = true
			if strings.Contains(joined, "password=") {
				passwordArg = true
			}
		}
	}
	if hasEnable {
		t.Error("CreateUserForCert must not enable security (enabled once per cluster)")
	}
	if !hasAddUser {
		t.Error("expected 'add-user certuser' call")
	}
	if passwordArg {
		t.Error("expected no password= arg for cert-auth user")
	}
}

// ---------- EnableSecurity ----------

func TestEnableSecurity_runsSecurityEnable(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security": ""}, nil)
	e := newTestExecutor(runner)

	if err := e.EnableSecurity(context.Background(), "ns", "pod", "riak"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var hasEnable bool
	for _, c := range *calls {
		if strings.Contains(strings.Join(c.args, " "), "security enable") {
			hasEnable = true
		}
	}
	if !hasEnable {
		t.Error("expected 'security enable' call")
	}
}

func TestEnableSecurity_failsOnEnableError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "security enable") {
			return "", errors.New("network timeout")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	if err := e.EnableSecurity(context.Background(), "ns", "pod", "riak"); err == nil {
		t.Fatal("expected error from non-already enable failure, got nil")
	}
}

func TestEnableSecurity_ignoresAlreadyEnabledError(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "security enable") {
			return "", errors.New("security already enabled")
		}
		return "", nil
	}
	e := newTestExecutor(runner)

	if err := e.EnableSecurity(context.Background(), "ns", "pod", "riak"); err != nil {
		t.Fatalf("unexpected error for already-enabled: %v", err)
	}
}

// ---------- AddSecuritySource ----------

func TestAddSecuritySource_sendsCorrectArgs(t *testing.T) {
	runner, calls := mockRunner(map[string]string{"security add-source": ""}, nil)
	e := newTestExecutor(runner)

	if err := e.AddSecuritySource(context.Background(), "ns", "pod", "riak", "alice"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	// certificate is the only security source the executor can emit
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "security add-source alice 0.0.0.0/0 certificate") {
		t.Errorf("unexpected args: %s", joined)
	}
}

func TestAddSecuritySource_propagatesError(t *testing.T) {
	runner, _ := mockRunner(nil, map[string]error{"add-source": errors.New("source failed")})
	e := newTestExecutor(runner)

	err := e.AddSecuritySource(context.Background(), "ns", "pod", "riak", "alice")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
