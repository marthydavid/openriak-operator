# RiakUser

A `RiakUser` creates a Riak security user that authenticates with an **mTLS client certificate**
and grants it permissions. It requires a `RiakCluster` in the same namespace with TLS enabled. The
user's certificate is issued by cert-manager (`issuerRef`) or comes from an external CA
(`externalSecretName`); see [mTLS authentication](../mtls.md).

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
| `certificateRef.issuerRef.name` | string | exactly one of `issuerRef` / `externalSecretName` | cert-manager issuer that signs the certificate |
| `certificateRef.issuerRef.kind` | `Issuer` \| `ClusterIssuer` | `Issuer` | Issuer kind |
| `certificateRef.secretName` | string | `<riakuser-name>-client-tls` | Secret that receives the certificate (with `issuerRef` only) |
| `certificateRef.externalSecretName` | string | — | Existing Secret with the user's certificate from an external CA; no Certificate is created, the Secret is validated. See [External CA](../mtls.md#client-certificates-from-an-external-ca) |
| `grants[].resource` | `bucket` \| `any` | **required** | What the grant applies to |
| `grants[].bucketName` | string | — | Bucket, when `resource: bucket` |
| `grants[].permission` | `read` `write` `delete` `list` `admin` | **required** | Permission |

Either way the certificate must chain to a CA the cluster trusts. A cert-manager issuer must chain
to the **same CA** as the cluster's TLS certificate; an external CA must be listed in the cluster's
`spec.tls.additionalClientCAs`.

## What the operator does

1. With `issuerRef`: requests a cert-manager `Certificate` (`<riakuser-name>-client-tls`) with
   CN = `spec.username`. With `externalSecretName`: creates nothing and validates the Secret you
   provided (CN, `client auth` usage, validity, chains to a trusted CA).
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
| `certificateReady` | The client certificate is issued (cert-manager) or valid and trusted (external) |
| `certificateError` | Why the certificate is not ready: issuance incomplete, or what is wrong with the external certificate |
| `username`, `clusterName`, `grants` | What was applied |
| `error` | Failure detail when `phase: Failed` |
| `conditions` | `Ready` and `CertificateReady` |

!!! tip "Ready is not the same as usable"
    `phase: Ready` means the user exists in Riak. Issuance (cert-manager) is asynchronous; wait for
    the `Cert` column / `certificateReady: true` before a client can authenticate. The operator
    re-checks every 30 seconds until the certificate is issued.

## Using the certificate

```bash
kubectl get secret app-client-tls -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -subject
```

Mount the Secret in your application and connect to `<cluster>:8087` over TLS — see
[Connecting a client](../mtls.md#connecting-a-client).
