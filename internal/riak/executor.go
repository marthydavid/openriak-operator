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
	if riakAdminFailed(out) {
		err := fmt.Errorf("riak-admin %s failed: %s", strings.Join(subcommand(args), " "), compactOutput(out))
		e.log.Error(err, "riak-admin command reported an error", "pod", podName, "args", args)
		return out, err
	}
	return out, nil
}

// riakAdminFailed reports whether riak-admin output signals a failure.
// riak-admin exits 0 even when the command fails: the node's reply is printed
// instead, either as a bare "error" on the last line (bucket-type commands) or
// as an {error,Reason} term (security commands). Without this check every
// failed command would look like a success.
func riakAdminFailed(out string) bool {
	last := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{error,") {
			return true
		}
		last = line
	}
	return last == "error"
}

// subcommand returns the riak-admin subcommand words (e.g. "bucket-type create")
// for error messages, without arguments that may be long JSON documents.
func subcommand(args []string) []string {
	if len(args) > 2 {
		return args[:2]
	}
	return args
}

// compactOutput folds riak-admin output onto one line, capped, for an error message.
func compactOutput(out string) string {
	s := strings.Join(strings.Fields(out), " ")
	const maxLen = 300
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
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

// memberStatusValid is the member-status state of a node that is a settled ring member.
const memberStatusValid = "valid"

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
		case memberStatusValid, "joining", "leaving", "exiting", "down":
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

// CreateBucket makes a bucket type exist, be active, and carry the given
// properties. A type that is already active cannot be re-created, so its
// properties are applied with `bucket-type update` instead: without that, an
// edited RiakBucket would never reach Riak. Riak's built-in "default" type can
// only be updated, never created or activated.
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
	switch {
	case err == nil:
		// New (or created but never activated, which create overwrites): activate it.
		_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "bucket-type", "activate", bucketType)
		if err != nil && !strings.Contains(err.Error(), "already") {
			return err
		}
		return nil
	case strings.Contains(err.Error(), "already_active"), strings.Contains(err.Error(), "default_type"):
		if len(props) == 0 {
			return nil
		}
		_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "bucket-type", "update", bucketType, string(propsJSON))
		return err
	default:
		return err
	}
}

// CreateUserForCert creates a Riak user without a password for certificate-based authentication.
// The user is still created in the security system; a separate AddSecuritySource call configures
// the certificate source so Riak accepts client certs with CN == username.
func (e *Executor) CreateUserForCert(ctx context.Context, namespace, podName, containerName, username string) error {
	// Security must already be enabled on the cluster (see EnableSecurity); it is
	// enabled once per cluster rather than here, per user.
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "add-user", username)
	if err != nil && strings.Contains(err.Error(), "role_exists") {
		return nil // reconciles re-run this; an existing user is the goal
	}
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

// resourceAny is the CRD grant resource (and riak-admin target) for every bucket.
const resourceAny = "any"

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
	target, err := grantTarget(resource, bucket)
	if err != nil {
		return err
	}
	args := []string{"security", "grant", strings.Join(permissionTokens(permissions), ","), "on"}
	args = append(args, target...)
	args = append(args, "to", username)
	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, args...)
	return err
}

// RevokePermissions removes permissions from a user on a grant target, the
// inverse of GrantPermissions.
func (e *Executor) RevokePermissions(ctx context.Context, namespace, podName, containerName, username, resource, bucket string, tokens []string) error {
	target, err := grantTarget(resource, bucket)
	if err != nil {
		return err
	}
	args := []string{"security", "revoke", strings.Join(tokens, ","), "on"}
	args = append(args, target...)
	args = append(args, "from", username)
	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, args...)
	return err
}

// DeleteUser removes a Riak user together with its grants and sources. A user
// Riak does not know is already deleted.
func (e *Executor) DeleteUser(ctx context.Context, namespace, podName, containerName, username string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "del-user", username)
	if err != nil && strings.Contains(err.Error(), "unknown_user") {
		return nil
	}
	return err
}

// grantTarget resolves the `on ...` target of a grant. A "bucket" resource with an
// empty bucket must NOT fall through to "on any" — that would silently grant
// cluster-wide access. Reject it, and reject unknown resources, instead.
func grantTarget(resource, bucket string) ([]string, error) {
	switch resource {
	case resourceAny:
		return []string{resourceAny}, nil
	case "bucket":
		// strings.Fields also collapses a whitespace-only bucket to an empty
		// target, so validate the parsed result rather than the raw string.
		target := strings.Fields(bucket)
		if len(target) == 0 {
			return nil, fmt.Errorf("bucket grant requires a bucket target")
		}
		return target, nil
	default:
		return nil, fmt.Errorf("unknown grant resource %q", resource)
	}
}

// permissionTokens expands CRD permissions into the distinct riak_kv permission
// tokens they stand for, in first-seen order.
func permissionTokens(permissions []string) []string {
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
	return tokens
}

// GrantEntry is one row of a user's dedicated permissions as Riak reports them.
type GrantEntry struct {
	Type   string // bucket type, or "*" for `on any`
	Bucket string // bucket, or "*" for the whole type
	Tokens []string
}

// GetUserGrants returns the user's dedicated (directly granted) permissions.
func (e *Executor) GetUserGrants(ctx context.Context, namespace, podName, containerName, username string) ([]GrantEntry, error) {
	out, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "print-grants", username)
	if err != nil {
		return nil, err
	}
	return parseDedicatedGrants(out), nil
}

// parseDedicatedGrants reads the "Dedicated permissions" table of print-grants.
// Long permission lists wrap onto continuation rows with an empty type column.
func parseDedicatedGrants(output string) []GrantEntry {
	lines := strings.Split(output, "\n")
	entries := make([]GrantEntry, 0, len(lines))
	inSection := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "Dedicated permissions"):
			inSection = true
			continue
		case strings.HasPrefix(line, "Cumulative permissions"):
			return entries
		case !inSection || !strings.HasPrefix(line, "|"):
			continue
		}
		cols := strings.Split(strings.Trim(line, "|"), "|")
		if len(cols) != 3 {
			continue
		}
		typ, bucket := strings.TrimSpace(cols[0]), strings.TrimSpace(cols[1])
		if typ == "type" { // header row
			continue
		}
		var tokens []string
		for _, t := range strings.Split(cols[2], ",") {
			if t = strings.TrimSpace(t); t != "" {
				tokens = append(tokens, t)
			}
		}
		if typ == "" && len(entries) > 0 { // continuation of the previous row
			last := &entries[len(entries)-1]
			last.Tokens = append(last.Tokens, tokens...)
			continue
		}
		entries = append(entries, GrantEntry{Type: typ, Bucket: bucket, Tokens: tokens})
	}
	return entries
}
