package riak

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
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

// ReconcileMembership joins every standalone node into the ring of the seed
// pod <cluster>-0 and reports whether all of podNames are now valid members.
//
// A freshly started Riak node is its own one-member ring, so nothing forms a
// multi-node cluster until the operator runs `riak-admin cluster join` on each
// joining node and then plans and commits once. Node names are read from
// member-status rather than built from the pod name, so the cluster DNS domain
// never has to be guessed. It is idempotent: a pod already in the seed's ring is
// left alone, so it is safe to call on every reconcile.
func (m *Manager) ReconcileMembership(ctx context.Context, cluster *riakv1.RiakCluster, podNames []string) (bool, error) {
	seed := cluster.Name + "-0"
	seedMembers, err := m.executor.MemberStatus(ctx, cluster.Namespace, seed, "riak")
	if err != nil {
		return false, fmt.Errorf("member-status on seed %s: %w", seed, err)
	}

	seedNode := ""
	inRing := make(map[string]bool, len(seedMembers))
	valid := 0
	for _, mem := range seedMembers {
		inRing[mem.Node] = true
		if mem.Status == memberStatusValid {
			valid++
		}
		if mem.Node == "riak@"+seed || strings.HasPrefix(mem.Node, "riak@"+seed+".") {
			seedNode = mem.Node
		}
	}
	if seedNode == "" {
		return false, fmt.Errorf("seed node for %s not found in member-status", seed)
	}

	joined := 0
	for _, pod := range podNames {
		if pod == seed {
			continue
		}
		members, err := m.executor.MemberStatus(ctx, cluster.Namespace, pod, "riak")
		if err != nil {
			return false, fmt.Errorf("member-status on %s: %w", pod, err)
		}
		// Only a standalone node (a ring of exactly itself) is joined. A node that
		// already sees other members is either in the seed's ring or a stray ring
		// that needs an operator, so it is never re-joined automatically.
		if len(members) != 1 || inRing[members[0].Node] {
			continue
		}
		m.log.Info("joining node to cluster", "cluster", cluster.Name, "pod", pod, "target", seedNode)
		if err := m.executor.JoinCluster(ctx, cluster.Namespace, pod, "riak", seedNode); err != nil {
			return false, fmt.Errorf("join %s: %w", pod, err)
		}
		joined++
	}

	if joined > 0 {
		if _, err := m.executor.ExecuteRiakAdmin(ctx, cluster.Namespace, seed, "riak", "cluster", "plan"); err != nil {
			return false, fmt.Errorf("cluster plan: %w", err)
		}
		if _, err := m.executor.ExecuteRiakAdmin(ctx, cluster.Namespace, seed, "riak", "cluster", "commit"); err != nil {
			return false, fmt.Errorf("cluster commit: %w", err)
		}
		return false, nil // re-check on the next reconcile once the ring settles
	}
	return valid == len(podNames), nil
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
