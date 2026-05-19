# OpenRiak Operator

A production-ready Kubernetes operator for managing Riak clusters with full lifecycle automation, user management, and bucket provisioning.

## ✨ Features

- **Cluster Lifecycle Management**: Automatic creation and management of Riak clusters using StatefulSets
- **Configuration Management**: Dynamic Riak configuration through RiakCluster CRDs
- **User & ACL Management**: Create users and manage granular access grants via RiakUser resources
- **Bucket Management**: Automated bucket type and bucket provisioning via RiakBucket resources
- **TLS Support**: Prepared for cert-manager integration for secure inter-node communication
- **Health Checks**: Built-in liveness and readiness probes for all nodes
- **Graceful Scaling**: Support for rolling updates and node scaling with pod affinity
- **Operator Framework**: Built with kubebuilder following Kubernetes best practices

## Prerequisites

- Kubernetes 1.24+
- kubectl configured to access your cluster
- A storage class available for persistent volumes
- (Optional) cert-manager for TLS support

## Installation

### Option 1: Deploy to Local Kind Cluster

```bash
# Create Kind cluster
kind create cluster --name riak-dev

# Build and load operator image into Kind
make docker-build IMG=riak-operator:latest
kind load docker-image riak-operator:latest --name riak-dev

# Install CRDs
make install

# Deploy operator
make deploy IMG=riak-operator:latest
```

### Option 2: Deploy to Production Cluster

```bash
# Build and push to registry
make docker-build docker-push IMG=registry.example.com/riak-operator:v0.1.0

# Install CRDs
make install

# Deploy operator
make deploy IMG=registry.example.com/riak-operator:v0.1.0
```

## Quick Start

### 1. Create a Development Cluster

```bash
kubectl apply -f examples/1-dev-cluster.yaml
```

Watch cluster status:
```bash
watch kubectl get riakclusters
kubectl describe riakcluster riak-cluster-dev
```

Wait until `Status.Phase` shows `Ready` and `ReadyNodes` equals the desired size.

### 2. Access the Cluster

```bash
# Port-forward to access Riak
kubectl port-forward svc/riak-cluster-dev 8087:8087 8098:8098 &

# Test connection (install riak-client if needed)
echo "ping" | nc localhost 8087
```

### 3. Create Buckets

```bash
kubectl apply -f examples/4-buckets.yaml

# Verify
kubectl get riakbuckets
kubectl describe riakbucket app-data-bucket
```

### 4. Create Users with Grants

```bash
kubectl apply -f examples/3-user-with-grants.yaml

# Verify
kubectl get riakusers
kubectl describe riakuser app-user
```

## CRD Reference

### RiakCluster

The main resource that defines a complete Riak cluster deployment.

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: my-cluster
  namespace: default
spec:
  # Number of Riak nodes
  size: 3

  # Container image
  image: riak/riak:latest
  imagePullPolicy: IfNotPresent

  # Compute resources per node
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: 1000m
      memory: 2Gi

  # Storage configuration
  storageClassName: standard
  storageSize: 10Gi

  # Riak configuration (riak.conf settings)
  riakConfig:
    "ring_size": "64"
    "transfer_limit": "2"
    "handoff_port": "8099"

  # Protocol buffer port
  servicePort: 8087

  # Pod scheduling constraints
  nodeSelector:
    disktype: ssd

  # TLS configuration (cert-manager integration)
  tls:
    enabled: false
    certManager:
      issuerName: letsencrypt-prod
      issuerKind: ClusterIssuer

status:
  phase: Ready
  readyNodes: 3
  members:
    - name: riak@my-cluster-0.my-cluster-headless.default.svc.cluster.local
      pod: my-cluster-0
      ready: true
  conditions:
    - type: Ready
      status: "True"
      reason: ClusterReady
      message: Riak cluster is ready
```

### RiakBucket

Creates and manages buckets in a Riak cluster.

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata:
  name: mydata-bucket
  namespace: default
spec:
  # Reference to target cluster
  clusterName: my-cluster

  # Bucket name in Riak
  bucketName: mydata

  # Bucket type
  bucketType: default

  # Replication factor (n_val)
  replicationFactor: 3
  nVal: 3

  # Allow sibling values (conflicts)
  allowMulti: false

  # Custom bucket properties
  properties:
    "search_index": "_dont_index_"
    "consistent": "false"

status:
  phase: Ready
  created: true
  lastUpdateTime: "2024-05-18T10:30:00Z"
```

### RiakUser

Creates users and grants permissions in a Riak cluster.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: appuser-password
  namespace: default
type: Opaque
data:
  password: c2VjdXJlcGFzc3dvcmQ=  # base64 encoded

---
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: appuser
  namespace: default
spec:
  # Reference to target cluster
  clusterName: my-cluster

  # Riak username
  username: appuser

  # Password stored in a Secret
  passwordSecret:
    name: appuser-password
    key: password

  # Access grants
  grants:
    # Read on all buckets
    - resource: any
      permission: read

    # Write on specific bucket
    - resource: bucket
      bucketName: mydata
      permission: write

    # Admin access (use with caution)
    - resource: any
      permission: admin

status:
  phase: Ready
  created: true
  lastUpdateTime: "2024-05-18T10:30:00Z"
```

## Monitoring and Troubleshooting

### Check Cluster Health

```bash
# List all clusters
kubectl get riakclusters

# Detailed status
kubectl describe riakcluster my-cluster

# View individual nodes
kubectl get pods -l app=riak,cluster=my-cluster

# Check node logs
kubectl logs my-cluster-0 -c riak

# Follow logs in real-time
kubectl logs -f my-cluster-0 -c riak
```

### Common Issues

**Cluster stuck in "Creating" phase**
```bash
# Check pod events
kubectl describe pod my-cluster-0

# Check PVC status
kubectl get pvc

# Check operator logs
kubectl logs -n riak-system -l app.kubernetes.io/name=openriak-operator -c manager

# Check that storage class exists
kubectl get storageclass
```

**User creation failed**
```bash
# Verify cluster is Ready
kubectl get riakcluster my-cluster

# Check password secret
kubectl get secret appuser-password

# Check RiakUser status
kubectl describe riakuser appuser
```

**Bucket creation failed**
```bash
# Verify all nodes are ready
kubectl get pods -l cluster=my-cluster

# Check RiakBucket status and error
kubectl describe riakbucket mydata-bucket

# Verify bucket type name
kubectl describe riakbucket mydata-bucket | grep -A 5 "Status"
```

### Debug Commands

```bash
# Execute riak-admin commands on a node
kubectl exec -it my-cluster-0 -c riak -- riak-admin status

# List cluster members
kubectl exec -it my-cluster-0 -c riak -- riak-admin member-status

# Get ring status
kubectl exec -it my-cluster-0 -c riak -- riak-admin ring_status

# Check bucket types
kubectl exec -it my-cluster-0 -c riak -- riak-admin bucket-type list
```

## Performance Tuning

### Development Setup

```yaml
spec:
  size: 3
  storageSize: 10Gi
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
```

### Production Setup

```yaml
spec:
  size: 5
  storageSize: 100Gi
  storageClassName: fast-ssd
  resources:
    requests:
      cpu: 2
      memory: 4Gi
    limits:
      cpu: 4
      memory: 8Gi
  riakConfig:
    "ring_size": "256"
    "transfer_limit": "4"
    "anti_entropy": "on"
    "handoff_port": "8099"
  nodeSelector:
    disktype: ssd
```

### Tuning Recommendations

- **Ring Size**: 64-128 for small clusters (3-5 nodes), 256+ for larger clusters
- **Transfer Limit**: Number of concurrent handoffs (2-4 typical)
- **Anti-Entropy**: Enable for production clusters
- **Backend**: Choose LevelDB for modern deployments
- **Replication**: Always set n_val >= 3 for fault tolerance

## Development

### Building from Source

```bash
# Download dependencies
go mod download

# Generate CRDs and RBAC manifests
make generate manifests

# Run tests
make test

# Build operator binary
make build

# Build Docker image
make docker-build IMG=riak-operator:dev

# Run locally (requires cluster access)
make run
```

### Project Structure

```
.
├── api/v1/              # CRD type definitions
├── internal/
│   ├── controller/      # Reconciliation logic
│   └── riak/           # Riak management (shell execution)
├── config/
│   ├── crd/            # CRD manifests
│   ├── rbac/           # RBAC definitions
│   ├── manager/        # Operator deployment
│   └── samples/        # Example resources
├── examples/           # End-to-end example manifests
├── Dockerfile          # Operator container image
└── Makefile           # Build automation
```

### Key Modules

- **api/v1/riakcluster_types.go**: RiakCluster CRD definition
- **api/v1/riakbucket_types.go**: RiakBucket CRD definition
- **api/v1/riakuser_types.go**: RiakUser CRD definition
- **internal/controller/riakcluster_controller.go**: Cluster reconciliation logic
- **internal/riak/executor.go**: Shell command execution to Riak nodes
- **internal/riak/manager.go**: Riak cluster operations (join nodes, create buckets, etc.)

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Add tests for new functionality
4. Update documentation
5. Submit a pull request with a clear description

## License

Apache License 2.0 - See LICENSE file for details

## Support

For issues, questions, or feature requests:
- Open an issue on GitHub
- Check documentation for common problems
- Review operator logs: `kubectl logs -n riak-system -l app.kubernetes.io/name=openriak-operator`

## Roadmap

- [ ] TLS/cert-manager integration
- [ ] Automated backups
- [ ] Cluster monitoring and metrics
- [ ] Helm chart
- [ ] Riak search integration
- [ ] Multi-datacenter support
