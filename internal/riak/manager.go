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

// GrantUserPermissions applies all of a user's grants, batched by target: one
// riak-admin security-grant call per distinct (resource, bucket) instead of one
// per grant. Each riak-admin call spawns a temporary Erlang VM on the node, so
// this materially cuts provisioning cost for users with several grants and for
// large fleets. Grouping preserves first-seen order so the emitted commands are
// deterministic.
func (m *Manager) GrantUserPermissions(ctx context.Context, cluster *riakv1.RiakCluster, username string, grants []riakv1.Grant) error {
	if len(grants) == 0 {
		return nil
	}
	if len(cluster.Status.Members) == 0 {
		return fmt.Errorf("no cluster members available")
	}
	pod := cluster.Status.Members[0].Pod

	type target struct{ resource, bucket string }
	var order []target
	perms := map[target][]string{}
	for _, g := range grants {
		t := target{g.Resource, g.BucketName}
		if _, ok := perms[t]; !ok {
			order = append(order, t)
		}
		perms[t] = append(perms[t], g.Permission)
	}

	for _, t := range order {
		if err := m.executor.GrantPermissions(ctx, cluster.Namespace, pod, "riak",
			username, t.resource, t.bucket, perms[t]); err != nil {
			return err
		}
	}
	return nil
}

// CreateUserForCert creates a Riak user configured for certificate-based authentication.
func (m *Manager) CreateUserForCert(ctx context.Context, cluster *riakv1.RiakCluster, username string) error {
	if len(cluster.Status.Members) == 0 {
		return fmt.Errorf("no cluster members available")
	}

	pod := cluster.Status.Members[0].Pod
	return m.executor.CreateUserForCert(ctx, cluster.Namespace, pod, "riak", username)
}

// EnableSecurity enables Riak's security subsystem on the cluster. It is run once
// per cluster (guarded by RiakCluster.Status.SecurityEnabled), not per user,
// because repeatedly toggling security on a live node bounces its client listeners
// and destabilises it under load.
func (m *Manager) EnableSecurity(ctx context.Context, cluster *riakv1.RiakCluster) error {
	if len(cluster.Status.Members) == 0 {
		return fmt.Errorf("no cluster members available")
	}

	pod := cluster.Status.Members[0].Pod
	return m.executor.EnableSecurity(ctx, cluster.Namespace, pod, "riak")
}

// AddSecuritySource registers the certificate security source for a user.
func (m *Manager) AddSecuritySource(ctx context.Context, cluster *riakv1.RiakCluster, username string) error {
	if len(cluster.Status.Members) == 0 {
		return fmt.Errorf("no cluster members available")
	}

	pod := cluster.Status.Members[0].Pod
	return m.executor.AddSecuritySource(ctx, cluster.Namespace, pod, "riak", username)
}
