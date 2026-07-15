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
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "enable")
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}

	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "add-user", username)
	return err
}

// AddSecuritySource configures how a Riak user authenticates.
// The operator always uses the "certificate" source (mTLS client cert, CN == username).
func (e *Executor) AddSecuritySource(ctx context.Context, namespace, podName, containerName, username, sourceType string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName,
		"security", "add-source", username, "0.0.0.0/0", sourceType)
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
	target := []string{"any"}
	if resource == "bucket" && bucket != "" {
		target = strings.Fields(bucket)
	}

	args := []string{"security", "grant", riakKVPermissions(permission), "on"}
	args = append(args, target...)
	args = append(args, "to", username)
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, args...)
	return err
}
