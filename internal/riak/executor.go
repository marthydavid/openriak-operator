package riak

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// Executor handles shell command execution to Riak nodes.
type Executor struct {
	log      logr.Logger
	runnerFn func(ctx context.Context, name string, args ...string) (string, error)
}

// NewExecutor creates a new Riak command executor.
func NewExecutor(log logr.Logger) *Executor {
	e := &Executor{log: log}
	e.runnerFn = runShellCommand
	return e
}

// NewExecutorWithRunner creates an Executor using a custom command runner.
// Useful for integration testing and environments with a non-standard kubectl.
func NewExecutorWithRunner(log logr.Logger, runner func(context.Context, string, ...string) (string, error)) *Executor {
	return &Executor{log: log, runnerFn: runner}
}

// runShellCommand is the default runner that invokes the real binary.
func runShellCommand(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ExecuteRiakAdmin executes a riak-admin command inside a pod via kubectl exec.
func (e *Executor) ExecuteRiakAdmin(ctx context.Context, namespace, podName, containerName string, args ...string) (string, error) {
	e.log.V(2).Info("executing riak-admin command", "pod", podName, "args", args)

	cmdArgs := []string{
		"exec",
		"-n", namespace,
		podName,
		"-c", containerName,
		"--",
		// riak-admin calls the release's `riak` script directly, which falls back to
		// the default vm.args (-sname riak) and so addresses the node as
		// riak@<short hostname>. Point it at the generated vm.args the node was
		// started with so it finds the node under its real (FQDN) name.
		"sh", "-c",
		"VMARGS_PATH=$(ls -1 /var/lib/riak/generated.conf/vm.*.args 2>/dev/null | tail -1) exec riak-admin \"$@\"",
		"riak-admin",
	}
	cmdArgs = append(cmdArgs, args...)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := e.runnerFn(ctx, "kubectl", cmdArgs...)
	if err != nil {
		e.log.Error(err, "riak-admin command failed", "pod", podName, "args", args)
		return "", fmt.Errorf("riak-admin failed: %w", err)
	}
	return out, nil
}

// GetClusterMembers retrieves the list of cluster members from a node.
func (e *Executor) GetClusterMembers(ctx context.Context, namespace, podName, containerName string) ([]string, error) {
	output, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "member-status")
	if err != nil {
		return nil, err
	}

	var members []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "---") || strings.Contains(line, "Status") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) > 0 {
			members = append(members, parts[0])
		}
	}
	return members, nil
}

// ClusterMember is one row of `riak-admin member-status`.
type ClusterMember struct {
	Status string // valid, joining, leaving, exiting, down
	Node   string // Erlang node name, e.g. riak@pod-0.svc.ns.svc.cluster.local
}

// MemberStatus returns the members of the ring as seen from podName. A node
// that has not joined a cluster yet reports a ring containing only itself.
func (e *Executor) MemberStatus(ctx context.Context, namespace, podName, containerName string) ([]ClusterMember, error) {
	out, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "member-status")
	if err != nil {
		return nil, err
	}
	return parseMemberStatus(out), nil
}

// parseMemberStatus extracts the rows of the member-status table:
//
//	valid     100.0%      --      riak@node-0
func parseMemberStatus(output string) []ClusterMember {
	var members []ClusterMember
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || !strings.HasPrefix(f[3], "riak@") {
			continue
		}
		switch f[0] {
		case "valid", "joining", "leaving", "exiting", "down":
			members = append(members, ClusterMember{Status: f[0], Node: f[3]})
		}
	}
	return members
}

// JoinCluster stages a join of podName's node into the ring that contains
// targetNode. riak-admin must run on the joining node.
func (e *Executor) JoinCluster(ctx context.Context, namespace, podName, containerName, targetNode string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "cluster", "join", targetNode)
	return err
}

// PlanCluster stages a cluster membership change.
func (e *Executor) PlanCluster(ctx context.Context, namespace, podName, containerName, action string) (string, error) {
	return e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "cluster", action)
}

// CommitCluster applies staged cluster changes.
func (e *Executor) CommitCluster(ctx context.Context, namespace, podName, containerName string) (string, error) {
	return e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "cluster", "commit")
}

// GetStatus gets the status of a node.
func (e *Executor) GetStatus(ctx context.Context, namespace, podName, containerName string) (string, error) {
	return e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "status")
}

// CreateBucket creates a bucket type with the given properties.
// Riak requires JSON: riak-admin bucket-type create <type> '{"props":{"n_val":3}}'
// String values that parse as a JSON literal (number, bool) are sent as their native type.
func (e *Executor) CreateBucket(ctx context.Context, namespace, podName, containerName, bucketType, _ string, properties map[string]string) error {
	props := make(map[string]any, len(properties))
	for k, v := range properties {
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			props[k] = parsed
		} else {
			props[k] = v
		}
	}

	propsJSON, err := json.Marshal(map[string]any{"props": props})
	if err != nil {
		return fmt.Errorf("failed to marshal bucket properties: %w", err)
	}

	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "bucket-type", "create", bucketType, string(propsJSON))
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}

	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "bucket-type", "activate", bucketType)
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}

	return nil
}

// CreateUserForCert creates a Riak user without a password for certificate-based authentication.
// The user is still created in the security system; a separate AddSecuritySource call configures
// the certificate source so Riak accepts client certs with CN == username.
func (e *Executor) CreateUserForCert(ctx context.Context, namespace, podName, containerName, username string) error {
	// Security must already be enabled on the cluster (see EnableSecurity); it is
	// enabled once per cluster rather than here, per user.
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "add-user", username)
	return err
}

// EnableSecurity turns on Riak's security subsystem. "already enabled" is treated
// as success, but this must be run sparingly: repeatedly toggling security on a
// live node bounces its client listeners and destabilises the node under load, so
// callers enable it once per cluster (guarded by RiakCluster.Status.SecurityEnabled),
// not once per user.
func (e *Executor) EnableSecurity(ctx context.Context, namespace, podName, containerName string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "enable")
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}
	return nil
}

// AddSecuritySource registers the certificate security source for a user:
// mTLS client certificates (CN == username) are the only authentication mode,
// so the source is not caller-selectable.
func (e *Executor) AddSecuritySource(ctx context.Context, namespace, podName, containerName, username string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName,
		"security", "add-source", username, "0.0.0.0/0", "certificate")
	return err
}

// riakKVPermissions maps the CRD's friendly permission names to the Riak KV
// application permissions that `riak-admin security grant` expects. Riak does
// not understand the short forms (read/write/…); it requires fully qualified
// names such as riak_kv.get. Unknown values pass through unchanged.
func riakKVPermissions(permission string) string {
	switch permission {
	case "read":
		return "riak_kv.get"
	case "write":
		return "riak_kv.put"
	case "delete":
		return "riak_kv.delete"
	case "list":
		return "riak_kv.list_keys,riak_kv.list_buckets"
	case "admin":
		return "riak_kv.get,riak_kv.put,riak_kv.delete,riak_kv.list_keys,riak_kv.list_buckets"
	default:
		return permission
	}
}

// GrantPermission grants a permission to a user on a resource.
//
// Riak's grant syntax is:
//
//	security grant <perms> on any to <user>              # default bucket type
//	security grant <perms> on <type> [bucket] to <user>  # a bucket type / bucket
//
// The CRD models resource as "any" or "bucket"; for "bucket" the bucket field
// carries the grant target — either "<type>" or "<type> <bucket>".
func (e *Executor) GrantPermission(ctx context.Context, namespace, podName, containerName, username, resource, permission, bucket string) error {
	return e.GrantPermissions(ctx, namespace, podName, containerName, username, resource, bucket, []string{permission})
}

// GrantPermissions grants several CRD permissions on a single target in one
// riak-admin call, e.g. "security grant riak_kv.get,riak_kv.put on any to bob".
// Each riak-admin invocation spawns a temporary Erlang VM on the node, so
// collapsing a user's grants on the same target into one call materially cuts
// the exec/BEAM load when provisioning many users. The mapped permission tokens
// are de-duplicated with a stable order so the pod template / command is
// deterministic.
func (e *Executor) GrantPermissions(ctx context.Context, namespace, podName, containerName, username, resource, bucket string, permissions []string) error {
	// Resolve the grant target. A "bucket" resource with an empty bucket must
	// NOT fall through to "on any" — that would silently grant cluster-wide
	// access. Reject it, and reject unknown resources, instead.
	var target []string
	switch resource {
	case "any":
		target = []string{"any"}
	case "bucket":
		// strings.Fields also collapses a whitespace-only bucket to an empty
		// target, so validate the parsed result rather than the raw string.
		target = strings.Fields(bucket)
		if len(target) == 0 {
			return fmt.Errorf("bucket grant requires a bucket target")
		}
	default:
		return fmt.Errorf("unknown grant resource %q", resource)
	}

	seen := map[string]bool{}
	var tokens []string
	for _, p := range permissions {
		for _, t := range strings.Split(riakKVPermissions(p), ",") {
			if !seen[t] {
				seen[t] = true
				tokens = append(tokens, t)
			}
		}
	}

	args := []string{"security", "grant", strings.Join(tokens, ","), "on"}
	args = append(args, target...)
	args = append(args, "to", username)
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, args...)
	return err
}
