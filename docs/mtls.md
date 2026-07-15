# mTLS with cert-manager

The operator integrates with [cert-manager](https://cert-manager.io) to secure a Riak
cluster end to end:

1. **Cluster TLS** — the operator requests a server certificate for every node in a
   `RiakCluster` and configures Riak's HTTPS listener with it.
2. **mTLS client authentication** — a `RiakUser` can authenticate with a client
   certificate instead of a password. The operator requests the client certificate
   from cert-manager and registers the user with Riak's `certificate` security source.

The operator never generates keys or runs its own CA; issuance and renewal are fully
delegated to cert-manager.

## Prerequisites

- cert-manager v1.x installed in the cluster
  (`kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml`)
- An `Issuer` or `ClusterIssuer` that can sign the certificates. Cluster and client
  certificates must chain to the same CA — Riak verifies client certificates against
  the CA bundle from its own TLS secret.

### Example: a namespace-local CA

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned
  namespace: default
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: riak-ca
  namespace: default
spec:
  isCA: true
  commonName: riak-ca
  secretName: riak-ca-secret
  issuerRef:
    name: selfsigned
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: riak-ca-issuer
  namespace: default
spec:
  ca:
    secretName: riak-ca-secret
```

## Cluster TLS

Enable TLS on a `RiakCluster` by setting `spec.tls`:

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: my-cluster
  namespace: default
spec:
  size: 3
  tls:
    enabled: true
    certManager:
      issuerName: riak-ca-issuer
      issuerKind: Issuer        # or ClusterIssuer; defaults to Issuer
```

### What the operator creates

| Object | Name | Notes |
|--------|------|-------|
| cert-manager `Certificate` | `<cluster>-tls` | Owned by the RiakCluster; deleted with it |
| TLS `Secret` (created by cert-manager) | `<cluster>-tls` | Contains `tls.crt`, `tls.key`, `ca.crt` |

The certificate covers every pod of the StatefulSet plus the client-facing service:

- `*.<cluster>-headless.<namespace>.svc.cluster.local`
- `<cluster>-headless.<namespace>.svc.cluster.local`
- `<cluster>.<namespace>.svc.cluster.local`
- `<cluster>-headless`, `<cluster>` (short names)

Usages: `server auth`, `client auth`, `digital signature`, `key encipherment`.

### How the pods are configured

The operator mounts the TLS secret into each Riak container at `/etc/riak/certs`
and points Riak at it through `riak.conf` settings (injected as `RIAK_CONFIG_*`
environment variables):

| riak.conf key | Value |
|---------------|-------|
| `ssl.certfile` | `/etc/riak/certs/tls.crt` |
| `ssl.keyfile` | `/etc/riak/certs/tls.key` |
| `ssl.cacertfile` | `/etc/riak/certs/ca.crt` |
| `listener.https.internal` | `0.0.0.0:8443` |
| `check_crl` | `off` |

`check_crl` is disabled because cert-manager-issued client certificates have no
CRL distribution point; leaving it on makes Riak's protobuf TLS handshake fail
on such certificates.

Both the headless and the client `Service` expose the HTTPS listener as port
`https` (8443) alongside the plaintext `http` (8098) and `protobuf` (8087) ports.

### Certificate rotation

cert-manager renews the certificate before expiry and updates the secret in place.
Kubernetes propagates the new files into the running pods' mounted volume; no
operator action is required. Riak reads the certificate files at connection setup,
so new connections pick up the renewed certificate automatically.

## mTLS client authentication for RiakUsers

Every `RiakUser` authenticates with an mTLS client certificate — `spec.certificateRef`
is required:

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: app-cert-user
  namespace: default
spec:
  clusterName: my-cluster
  username: appuser
  certificateRef:
    issuerRef:
      name: riak-ca-issuer    # must chain to the same CA as the cluster cert
      kind: Issuer            # or ClusterIssuer; defaults to Issuer
    # secretName: my-custom-secret   # optional; defaults to <riakuser-name>-client-tls
  grants:
    - resource: bucket
      bucketName: mydata
      permission: read
    - resource: bucket
      bucketName: mydata
      permission: write
```

`certificateRef` is required: client certificates are the only supported authentication mode.

### What the operator does

1. Creates a cert-manager `Certificate` named `<riakuser-name>-client-tls` with
   **`commonName` set to `spec.username`** — Riak's certificate security source
   matches users by certificate CN, so the two must be identical. Usages:
   `client auth`, `digital signature`, `key encipherment`.
2. Enables Riak security if not already enabled (`riak-admin security enable`),
   then creates the user (`riak-admin security add-user`).
3. Registers the certificate source
   (`riak-admin security add-source <username> 0.0.0.0/0 certificate`).
4. Applies `spec.grants`.

cert-manager writes the issued certificate to the secret (default
`<riakuser-name>-client-tls`) containing `tls.crt`, `tls.key`, and `ca.crt`.

### Connecting a client

Mount the client secret into the application pod and connect to the protobuf port
with TLS, presenting the client certificate:

```yaml
volumes:
  - name: riak-client-tls
    secret:
      secretName: app-cert-user-client-tls
```

```python
# Example: python riak client
client = riak.RiakClient(
    host="my-cluster.default.svc.cluster.local",
    pb_port=8087,
    credentials=riak.security.SecurityCreds(
        username="appuser",
        cacert_file="/certs/ca.crt",
        cert_file="/certs/tls.crt",
        pkey_file="/certs/tls.key",
    ),
)
```

For a dependency-free reference, `test/e2e/scripts/pb_cert_auth_check.py` speaks the
Riak protobuf STARTTLS handshake directly (standard library only) and performs an
authenticated write/read — useful for verifying cert auth from a debug pod.

## Troubleshooting

**`RiakUser` stuck in `Failed` with a certificate error** — check that the
`Certificate` was issued:

```bash
kubectl get certificate <riakuser-name>-client-tls -o wide
kubectl describe certificate <riakuser-name>-client-tls
```

**Clients get `certificate verify failed`** — the client certificate and the
cluster certificate must chain to the same CA. Verify both issuers reference the
same CA secret, and check what Riak trusts:

```bash
kubectl get secret <cluster>-tls -o jsonpath='{.data.ca\.crt}' | base64 -d | \
  openssl x509 -noout -subject -issuer
```

**Authentication fails despite a valid certificate** — the certificate CN must
equal the Riak username. Inspect the issued certificate:

```bash
kubectl get secret <riakuser-name>-client-tls -o jsonpath='{.data.tls\.crt}' | \
  base64 -d | openssl x509 -noout -subject
```

**Certificates are created but pods have no TLS volume** — `spec.tls.enabled`
must be `true`; setting only `certManager` is not enough.
