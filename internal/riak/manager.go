package riak

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Manager handles Riak cluster management operations.
type Manager struct {
	executor  *Executor
	k8sClient client.Client
	log       logr.Logger
}

// NewManager creates a new Riak cluster manager.
func NewManager(executor *Executor, k8sClient client.Client, log logr.Logger) *Manager {
	return &Manager{
		executor:  executor,
		k8sClient: k8sClient,
		log:       log,
	}
}

// GetClusterStatus retrieves the status of a Riak cluster.
func (m *Manager) GetClusterStatus(ctx context.Context, cluster *riakv1.RiakCluster) (string, error) {
	if len(cluster.Status.Members) == 0 {
		return "", fmt.Errorf("no cluster members available")
	}

	pod := cluster.Status.Members[0].Pod
	return m.executor.GetStatus(ctx, cluster.Namespace, pod, "riak")
}

// InitializeCluster prepares a Riak cluster for bootstrap.
func (m *Manager) InitializeCluster(ctx context.Context, cluster *riakv1.RiakCluster) error {
	m.log.Info("initializing riak cluster", "cluster", cluster.Name)

	// Get all pods
	pods := &corev1.PodList{}
	if err := m.k8sClient.List(ctx, pods, client.InNamespace(cluster.Namespace)); err != nil {
		return err
	}

	var clusterPods []corev1.Pod
	for _, pod := range pods.Items {
		if strings.HasPrefix(pod.Name, cluster.Name+"-") {
			clusterPods = append(clusterPods, pod)
		}
	}

	if len(clusterPods) == 0 {
		return fmt.Errorf("no cluster pods found")
	}

	// Bootstrap the first node
	firstPod := clusterPods[0]
	for i := 1; i < len(clusterPods); i++ {
		nodeName := fmt.Sprintf("riak@%s.%s-headless.%s.svc.cluster.local", clusterPods[i].Name, cluster.Name, cluster.Namespace)
		_, err := m.executor.ExecuteRiakAdmin(ctx, cluster.Namespace, firstPod.Name, "riak",
			"cluster", "join", nodeName)
		if err != nil {
			m.log.Error(err, "failed to join node", "node", nodeName)
			continue
		}
	}

	// Plan and commit the cluster
	_, err := m.executor.ExecuteRiakAdmin(ctx, cluster.Namespace, firstPod.Name, "riak", "cluster", "plan")
	if err != nil {
		return err
	}

	_, err = m.executor.ExecuteRiakAdmin(ctx, cluster.Namespace, firstPod.Name, "riak", "cluster", "commit")
	if err != nil {
		return err
	}

	return nil
}

// ConfigureNode sets Riak configuration for a specific node.
func (m *Manager) ConfigureNode(ctx context.Context, namespace, podName string, config map[string]string) error {
	m.log.V(2).Info("configuring node", "pod", podName, "config", config)

	for key, value := range config {
		_, err := m.executor.ExecuteRiakAdmin(ctx, namespace, podName, "riak",
			"config", "set", key, value)
		if err != nil {
			m.log.Error(err, "failed to set config", "key", key, "value", value)
		}
	}

	return nil
}

// CreateBucketType creates a bucket type in the cluster.
func (m *Manager) CreateBucketType(ctx context.Context, cluster *riakv1.RiakCluster, bucketType string, properties map[string]string) error {
	if len(cluster.Status.Members) == 0 {
		return fmt.Errorf("no cluster members available")
	}

	pod := cluster.Status.Members[0].Pod
	return m.executor.CreateBucket(ctx, cluster.Namespace, pod, "riak", bucketType, "", properties)
}

// CreateUser creates a user in the cluster.
func (m *Manager) CreateUser(ctx context.Context, cluster *riakv1.RiakCluster, username, password string) error {
	if len(cluster.Status.Members) == 0 {
		return fmt.Errorf("no cluster members available")
	}

	pod := cluster.Status.Members[0].Pod
	return m.executor.CreateUser(ctx, cluster.Namespace, pod, "riak", username, password)
}

// GrantUserPermission grants permissions to a user.
func (m *Manager) GrantUserPermission(ctx context.Context, cluster *riakv1.RiakCluster, username, resource, permission, bucket string) error {
	if len(cluster.Status.Members) == 0 {
		return fmt.Errorf("no cluster members available")
	}

	pod := cluster.Status.Members[0].Pod
	return m.executor.GrantPermission(ctx, cluster.Namespace, pod, "riak", username, resource, permission, bucket)
}
