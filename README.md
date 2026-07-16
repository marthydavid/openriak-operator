# OpenRiak Operator

A production-ready Kubernetes operator for managing Riak clusters with full lifecycle automation, user management, and bucket provisioning.

## ✨ Features

- **Cluster Lifecycle Management**: Automatic creation and management of Riak clusters using StatefulSets
- **Configuration Management**: Dynamic Riak configuration through RiakCluster CRDs
- **User & ACL Management**: Create users and manage granular access grants via RiakUser resources
- **Bucket Management**: Automated bucket type and bucket provisioning via RiakBucket resources
- **mTLS via cert-manager**: Enable TLS on a cluster (`spec.tls`) to have cert-manager issue the
  server certificate, and authenticate users with client certificates (`spec.certificateRef`) — the
  issued certificate's CommonName is the Riak username ([full guide](docs/mtls.md))
- **Configurable Operand Image**: Set a fleet-wide default Riak image via the operator's
  `--riak-image` flag, overridable per cluster with `spec.image` ([config reference](docs/operator-configuration.md))
- **Health Checks**: Lightweight TCP liveness/readiness probes on the protobuf port
- **Graceful Scaling**: Support for rolling updates and node scaling with pod affinity
- **Operator Framework**: Built with kubebuilder following Kubernetes best practices

## Prerequisites

- Kubernetes 1.24+
- kubectl configured to access your cluster
- Helm 3.8+ (for the recommended install path; OCI registry support)
- A storage class available for persistent volumes
- (Optional) cert-manager for TLS support

## Installation

### Option 1: Helm (recommended)

```bash
helm install openriak-operator oci://ghcr.io/marthydavid/charts/openriak-operator \
  --namespace openriak-system --create-namespace
```

Or from a checkout: `helm install openriak-operator charts/openriak-operator ...`.
See [charts/openriak-operator](charts/openriak-operator/README.md) for all values
(operator image, default operand image, metrics, ServiceMonitor, RBAC toggles).

### Option 2: Deploy to Local Kind Cluster

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

### Option 3: Deploy to Production Cluster

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

  # Container image (optional; defaults to ghcr.io/marthydavid/riak:3.2.6).
  # ghcr.io/marthydavid/riak:3.4.0 (RHEL 9 / UBI9 base) is also published.
  image: ghcr.io/marthydavid/riak:3.2.6
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

  # TLS configuration (cert-manager integration). When enabled, the operator has
  # cert-manager issue the node's server certificate and turns on protobuf STARTTLS.
  # Use an internal CA issuer (client certs for mTLS users must chain to the same CA).
  tls:
    enabled: false
    certManager:
      issuerName: riak-ca-issuer
      issuerKind: Issuer   # Issuer or ClusterIssuer

status:
  phase: Ready
  readyNodes: 3
  members:
    - name: my-cluster-0
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

  # mTLS client-certificate authentication (required — the only auth mode).
  # cert-manager issues a certificate with CommonName == spec.username.
  certificateRef:
    issuerRef:
      name: my-ca-issuer
      kind: Issuer

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

#### Certificate-based (mTLS) authentication

Users authenticate exclusively with mTLS client certificates — `certificateRef` is required.
The operator asks cert-manager to issue a client certificate whose
**CommonName equals `spec.username`**, which is what Riak matches for certificate auth. The cluster
must have TLS enabled (`spec.tls.enabled: true`), and the user's issuer should chain to the same CA
as the cluster's issuer so the node trusts the client certificate.

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: appuser
  namespace: default
spec:
  clusterName: my-cluster
  username: appuser

  # mTLS client-certificate authentication (required)
  certificateRef:
    issuerRef:
      name: my-ca-issuer
      kind: Issuer            # Issuer or ClusterIssuer
    # secretName is optional; defaults to <riakuser-name>-client-tls
    secretName: appuser-client-tls

  grants:
    - resource: bucket
      bucketName: mydata
      permission: read
    - resource: bucket
      bucketName: mydata
      permission: write
```

Clients then connect over the protobuf port using the issued certificate (`tls.crt` / `tls.key` /
`ca.crt` from the secret) and authenticate as the user matching the certificate CommonName. A
minimal, dependency-free reference client that performs a STARTTLS write/read is at
`test/e2e/scripts/pb_cert_auth_check.py`.

> **Note:** grant `permission` values (`read`/`write`/`delete`/`list`/`admin`) map to Riak KV
> permissions (`riak_kv.get`, `riak_kv.put`, …). A grant with `resource: bucket` applies to the
> named bucket **type**.

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

- [x] TLS/cert-manager integration (server TLS + mTLS client-certificate user auth)
- [ ] Automated backups
- [ ] Cluster monitoring and metrics
- [ ] Helm chart
- [ ] Riak search integration
- [ ] Multi-datacenter support
