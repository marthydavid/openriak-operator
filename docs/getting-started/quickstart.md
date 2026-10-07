# Quick start

Works on any cluster where the operator and cert-manager are installed (this walkthrough uses
cert-manager for user certificates; to use your own CA see [mTLS authentication](../mtls.md))
([Kubernetes](kubernetes.md) / [OpenShift](openshift.md)). Commands use `kubectl`; substitute `oc`
on OpenShift.

## 1. A certificate authority

Users and the cluster need certificates signed by the same CA:

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

## 2. A cluster

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata: { name: demo, namespace: default }
spec:
  size: 3
  storageSize: 10Gi
  tls:
    enabled: true
    certManager: { issuerName: riak-ca-issuer, issuerKind: Issuer }
```

```bash
kubectl get riakcluster demo -w
# NAME   PHASE   READY   NODES   TOTAL   AGE
# demo   Ready   True    3       3       2m
```

## 3. A bucket and a user

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata: { name: orders, namespace: default }
spec:
  clusterName: demo
  bucketName: orders
  bucketType: orders
  nVal: 3
---
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata: { name: app, namespace: default }
spec:
  clusterName: demo
  username: app
  certificateRef:
    issuerRef: { name: riak-ca-issuer, kind: Issuer }
  grants:
    - { resource: bucket, bucketName: orders, permission: read }
    - { resource: bucket, bucketName: orders, permission: write }
```

```bash
kubectl get riakbuckets,riakusers
```

## 4. Connect

The operator exposes a Service named after the cluster (`demo.default.svc`) on the protobuf port
8087. The user's certificate is in the Secret `app-client-tls` (`tls.crt`, `tls.key`, `ca.crt`).
Mount it in your application pod and connect with TLS — see
[Connecting a client](../mtls.md#connecting-a-client).

## Clean up

```bash
kubectl delete riakuser app && kubectl delete riakbucket orders && kubectl delete riakcluster demo
```

Delete users and buckets **before** the cluster so they can remove themselves from Riak.
