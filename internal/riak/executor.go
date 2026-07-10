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

// CreateUser creates a Riak user with the given password.
func (e *Executor) CreateUser(ctx context.Context, namespace, podName, containerName, username, password string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "enable")
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}

	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "add-user", username, fmt.Sprintf("password=%s", password))
	return err
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
// sourceType is "password" or "certificate".
func (e *Executor) AddSecuritySource(ctx context.Context, namespace, podName, containerName, username, sourceType string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName,
		"security", "add-source", username, "0.0.0.0/0", sourceType)
	return err
}

// riakPermissions maps the RiakUser CRD permission enum to Riak security
// permission sets. Riak's CLI only accepts fully-qualified <app>.<perm> names
// ({error,{unknown_permission,...}} otherwise); verified against Riak 3.2.6.
var riakPermissions = map[string]string{
	"read":   "riak_kv.get",
	"write":  "riak_kv.put",
	"delete": "riak_kv.delete",
	"list":   "riak_kv.list_keys,riak_kv.list_buckets",
	"admin": "riak_kv.get,riak_kv.put,riak_kv.delete,riak_kv.list_keys," +
		"riak_kv.list_buckets,riak_kv.mapreduce,riak_kv.index," +
		"riak_core.get_bucket,riak_core.set_bucket",
}

// GrantPermission grants a permission to a user on a resource.
// permission is a RiakUser CRD enum value (read, write, delete, list, admin).
// resource is "any", or "bucket" with bucket naming the Riak bucket type
// created by a RiakBucket CR — the grant then covers every bucket of that type.
func (e *Executor) GrantPermission(ctx context.Context, namespace, podName, containerName, username, resource, permission, bucket string) error {
	perms, ok := riakPermissions[permission]
	if !ok {
		return fmt.Errorf("unknown permission %q", permission)
	}
	target := "any"
	if resource == "bucket" {
		if bucket == "" {
			return fmt.Errorf("resource %q requires a bucket name", resource)
		}
		target = bucket
	}
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName,
		"security", "grant", perms, "on", target, "to", username)
	return err
}
