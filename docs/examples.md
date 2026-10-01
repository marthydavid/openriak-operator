# Examples

Ready-to-apply manifests. The same files live in the repository's
[`examples/`](https://github.com/marthydavid/openriak-operator/tree/main/examples) directory.
Replace the issuer and StorageClass names with ones from your cluster.

## Single node for local development (kind, minikube)

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: { name: riak-local, namespace: default }
spec:
  size: 1
  resources:
    requests: { cpu: 250m, memory: 512Mi }
    limits:   { cpu: 500m, memory: 1Gi }
  storageClassName: standard
  storageSize: 2Gi
  riakConfig:
    ring_size: "8"          # small ring for one node; power of two
    transfer_limit: "2"
```

## Three-node development cluster

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: { name: riak-cluster-dev, namespace: default }
spec:
  size: 3
  resources:
    requests: { cpu: 500m, memory: 1Gi }
    limits:   { cpu: "1",  memory: 2Gi }
  storageClassName: standard
  storageSize: 10Gi
  riakConfig:
    ring_size: "128"
    transfer_limit: "2"
```

## Production cluster with TLS and metrics

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: { name: riak-prod, namespace: riak-system }
spec:
  size: 5
  resources:
    requests: { cpu: "2", memory: 4Gi }
    limits:   { cpu: "4", memory: 8Gi }
  storageClassName: fast-ssd
  storageSize: 100Gi
  riakConfig:
    ring_size: "256"
    transfer_limit: "4"
    anti_entropy: "on"
  nodeSelector:
    node-role: riak
  tls:
    enabled: true
    certManager: { issuerName: riak-ca-issuer, issuerKind: Issuer }
  monitoring:
    enabled: true
```

## Test cluster without a StorageClass

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: { name: ci, namespace: default }
spec:
  size: 3
  ephemeralStorage: true   # data is lost when a pod restarts
```

## OpenShift cluster with LVM Storage

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: { name: riak, namespace: riak }
spec:
  size: 3
  storageClassName: lvms-vg1
  storageSize: 20Gi
  tls:
    enabled: true
    certManager: { issuerName: riak-ca-issuer }
  monitoring:
    enabled: true
```

## Memory backend with TTL (cache cluster)

```yaml
spec:
  riakConfig:
    storage_backend: memory
    memory_backend.ttl: 60s
    memory_backend.max_memory_per_vnode: 128MB
```

## Durable default plus a TTL cache backend

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: { name: mixed }
spec:
  size: 3
  riakConfig:
    storage_backend: multi
    multi_backend.default: bitcask_data
    multi_backend.bitcask_data.storage_backend: bitcask
    multi_backend.mem_ttl.storage_backend: memory
    multi_backend.mem_ttl.memory_backend.ttl: 60s
    multi_backend.mem_ttl.memory_backend.max_memory_per_vnode: 32MB
---
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata: { name: cache }
spec:
  clusterName: mixed
  bucketName: cache
  bucketType: cache
  properties:
    backend: mem_ttl
```

## Certificate authority for cluster and users

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: selfsigned, namespace: default }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: riak-ca, namespace: default }
spec:
  isCA: true
  commonName: riak-ca
  secretName: riak-ca-secret
  issuerRef: { name: selfsigned, kind: Issuer }
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: riak-ca-issuer, namespace: default }
spec:
  ca: { secretName: riak-ca-secret }
```

## Users

Read-only on every bucket:

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata: { name: reporting }
spec:
  clusterName: riak-cluster-dev
  username: reporting
  certificateRef:
    issuerRef: { name: riak-ca-issuer, kind: Issuer }
  grants:
    - { resource: any, permission: read }
    - { resource: any, permission: list }
```

Read/write on one bucket, using a `ClusterIssuer` and a custom Secret name:

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata: { name: orders-service }
spec:
  clusterName: riak-cluster-dev
  username: orders-service
  certificateRef:
    issuerRef: { name: platform-ca, kind: ClusterIssuer }
    secretName: orders-riak-client
  grants:
    - { resource: bucket, bucketName: orders, permission: read }
    - { resource: bucket, bucketName: orders, permission: write }
```

## Buckets

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata: { name: app-data }
spec:
  clusterName: riak-cluster-dev
  bucketName: app-data
  bucketType: appdata
  nVal: 3
---
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata: { name: sessions }
spec:
  clusterName: riak-cluster-dev
  bucketName: sessions
  bucketType: sessions
  nVal: 3
  allowMulti: false
  properties:
    last_write_wins: "true"
```

## Application pod using a user's certificate

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: orders }
spec:
  replicas: 2
  selector: { matchLabels: { app: orders } }
  template:
    metadata: { labels: { app: orders } }
    spec:
      containers:
        - name: app
          image: registry.example.com/orders:1.0
          env:
            - { name: RIAK_HOST, value: riak-cluster-dev.default.svc }
            - { name: RIAK_PB_PORT, value: "8087" }
          volumeMounts:
            - { name: riak-tls, mountPath: /certs, readOnly: true }
      volumes:
        - name: riak-tls
          secret: { secretName: orders-riak-client }
```
