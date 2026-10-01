# RiakUser

A `RiakUser` creates a Riak security user that authenticates with an **mTLS client certificate**
and grants it permissions. It requires cert-manager and a `RiakCluster` in the same namespace.

## Example

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: app
spec:
  clusterName: demo
  username: app
  certificateRef:
    issuerRef:
      name: riak-ca-issuer
      kind: Issuer
  grants:
    - resource: bucket
      bucketName: orders
      permission: read
    - resource: bucket
      bucketName: orders
      permission: write
```

## Spec reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterName` | string | **required** | `RiakCluster` in the same namespace |
| `username` | string | **required** | Riak username; also the certificate **CommonName** |
| `certificateRef.issuerRef.name` | string | **required** | cert-manager issuer |
| `certificateRef.issuerRef.kind` | `Issuer` \| `ClusterIssuer` | `Issuer` | Issuer kind |
| `certificateRef.secretName` | string | `<riakuser-name>-client-tls` | Secret that receives the certificate |
| `grants[].resource` | `bucket` \| `any` | **required** | What the grant applies to |
| `grants[].bucketName` | string | — | Bucket, when `resource: bucket` |
| `grants[].permission` | `read` `write` `delete` `list` `admin` | **required** | Permission |

The issuer must chain to the **same CA** as the cluster's TLS certificate, otherwise Riak does not
trust the client certificate.

## What the operator does

1. Requests a cert-manager `Certificate` (`<riakuser-name>-client-tls`) with CN = `spec.username`.
2. Enables Riak security on the cluster once, if needed.
3. Creates the user and registers the `certificate` source.
4. Reconciles grants so Riak matches `spec.grants` exactly — grants removed from the spec are
   revoked, an empty list revokes everything.

Deleting the RiakUser removes the user, its grants and certificate source from Riak (best effort).

## Status

```bash
kubectl get riakuser
# NAME   PHASE   READY   CERT   CLUSTER   AGE
```

| Field | Meaning |
|-------|---------|
| `phase` | `Creating`, `Ready`, `Failed` — whether the Riak-side identity exists |
| `certificateReady` | cert-manager has issued the client certificate |
| `certificateError` | Why issuance is not complete |
| `username`, `clusterName`, `grants` | What was applied |
| `error` | Failure detail when `phase: Failed` |
| `conditions` | `Ready` and `CertificateReady` |

!!! tip "Ready is not the same as usable"
    `phase: Ready` means the user exists in Riak. Certificate issuance is asynchronous; wait for
    the `Cert` column / `certificateReady: true` before a client can authenticate. The operator
    re-checks every 30 seconds until the certificate is issued.

## Using the certificate

```bash
kubectl get secret app-client-tls -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -subject
```

Mount the Secret in your application and connect to `<cluster>:8087` over TLS — see
[Connecting a client](../mtls.md#connecting-a-client).
