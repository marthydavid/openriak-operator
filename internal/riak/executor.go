package riak

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// Executor handles shell command execution to Riak nodes.
type Executor struct {
	log logr.Logger
}

// NewExecutor creates a new Riak command executor.
func NewExecutor(log logr.Logger) *Executor {
	return &Executor{log: log}
}

// ExecuteRiakAdmin executes a riak-admin command in a pod.
// namespace: target namespace
// podName: target pod name
// containerName: container name in the pod
// args: riak-admin command arguments (without "riak-admin" prefix)
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

	cmd := exec.CommandContext(ctx, "kubectl", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		e.log.Error(err, "command failed", "stderr", stderr.String())
		return "", fmt.Errorf("riak-admin failed: %w: %s", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// GetClusterMembers retrieves the list of cluster members from a node.
func (e *Executor) GetClusterMembers(ctx context.Context, namespace, podName, containerName string) ([]string, error) {
	output, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "member-status")
	if err != nil {
		return nil, err
	}

	var members []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "---") && !strings.Contains(line, "Status") {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				members = append(members, parts[0])
			}
		}
	}

	return members, nil
}

// PlanCluster stages a cluster membership change.
func (e *Executor) PlanCluster(ctx context.Context, namespace, podName, containerName, action string) (string, error) {
	output, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "cluster", action)
	if err != nil {
		return "", err
	}
	return output, nil
}

// CommitCluster applies staged cluster changes.
func (e *Executor) CommitCluster(ctx context.Context, namespace, podName, containerName string) (string, error) {
	output, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "cluster", "commit")
	if err != nil {
		return "", err
	}
	return output, nil
}

// GetStatus gets the status of a node.
func (e *Executor) GetStatus(ctx context.Context, namespace, podName, containerName string) (string, error) {
	output, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "status")
	if err != nil {
		return "", err
	}
	return output, nil
}

// CreateBucket creates a bucket with specified properties.
func (e *Executor) CreateBucket(ctx context.Context, namespace, podName, containerName, bucketType, bucket string, properties map[string]string) error {
	args := []string{"bucket-type", "create", bucketType}

	for key, value := range properties {
		args = append(args, fmt.Sprintf("%s=%s", key, value))
	}

	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, args...)
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}

	// Activate the bucket type
	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "bucket-type", "activate", bucketType)
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}

	return nil
}

// CreateUser creates a Riak user.
func (e *Executor) CreateUser(ctx context.Context, namespace, podName, containerName, username, password string) error {
	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "enable")
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}

	_, err = e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, "security", "add-user", username, fmt.Sprintf("password=%s", password))
	if err != nil {
		return err
	}

	return nil
}

// GrantPermission grants a permission to a user.
func (e *Executor) GrantPermission(ctx context.Context, namespace, podName, containerName, username, resource, permission, bucket string) error {
	args := []string{"security", "grant", permission, "on", resource}
	if bucket != "" {
		args = append(args, bucket)
	}
	args = append(args, "to", username)

	_, err := e.ExecuteRiakAdmin(ctx, namespace, podName, containerName, args...)
	return err
}
