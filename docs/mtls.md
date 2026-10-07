# mTLS authentication

Every `RiakUser` authenticates to Riak with a **client certificate** (mTLS); there are no
passwords. The operator secures a cluster in two parts, and each has its own certificate source:

| What | Certificate source | Configured in |
|------|--------------------|---------------|
| **Node (server) certificate**: Riak's HTTPS and protobuf TLS listeners | [cert-manager](https://cert-manager.io) | `RiakCluster.spec.tls.certManager` |
| **User (client) certificates**: how a `RiakUser` proves its identity | **cert-manager** *or* an **external CA** (corporate PKI, Vault, ...) | `RiakUser.spec.certificateRef` |

Pick the user-certificate source per user; both can be mixed in one cluster. In both cases Riak
matches the user by the certificate's **CommonName**, which must equal `spec.username`, and the
certificate must chain to a CA the cluster trusts.

| | cert-manager | External CA |
|---|---|---|
| `certificateRef` | `issuerRef` | `externalSecretName` |
| Who issues and renews | cert-manager, through a `Certificate` the operator creates | You (your PKI, Vault, ...) |
| What the operator does with the certificate | Requests it | Validates it and reports the result in status |
| Trust | Same CA as the cluster certificate | List the CA in `spec.tls.additionalClientCAs` |
| Details | [below](#client-certificates-from-cert-manager) | [below](#client-certificates-from-an-external-ca) |

The operator never generates keys or runs its own CA.

## Prerequisites

- A cluster with TLS enabled (`spec.tls.enabled: true`).
- **Node certificate:** cert-manager v1.x with an `Issuer` or `ClusterIssuer`
  (`kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml`).
  This is needed for the cluster certificate even when every user's certificate comes from an
  external CA; external server certificates are not supported yet.
- **User certificates from cert-manager:** an issuer whose CA is the same one the cluster certificate
  chains to, because Riak verifies client certificates against the CA bundle it was given.
- **User certificates from an external CA:** the CA certificate (PEM) in a Secret or ConfigMap, and
  each user's certificate and key in a Secret.

### Example: a namespace-local CA for cert-manager

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

## Cluster TLS (node certificate)

Enable TLS on a `RiakCluster` by setting `spec.tls`. cert-manager issues the node certificate:

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

`check_crl` is disabled because client certificates usually carry no CRL distribution
point (cert-manager-issued ones do not); leaving it on makes Riak's protobuf TLS handshake fail
on such certificates.

Both the headless and the client `Service` expose the HTTPS listener as port
`https` (8443) alongside the plaintext `http` (8098) and `protobuf` (8087) ports.

### Certificate rotation

cert-manager renews the node certificate before expiry and updates the secret in place.
Kubernetes propagates the new files into the running pods' mounted volume; no
operator action is required. Riak reads the certificate files at connection setup,
so new connections pick up the renewed certificate automatically.

## Client certificates for RiakUsers

`spec.certificateRef` is required and takes **exactly one** of `issuerRef` (cert-manager) or
`externalSecretName` (external CA). Whichever you use, the operator then:

1. Enables Riak security if not already enabled (`riak-admin security enable`), then creates the
   user (`riak-admin security add-user`).
2. Registers the certificate source
   (`riak-admin security add-source <username> 0.0.0.0/0 certificate`).
3. Makes the user's Riak grants equal `spec.grants`: it grants everything the spec lists, then
   reads `riak-admin security print-grants` and revokes any permission the spec no longer lists.
   Removing a grant from the RiakUser removes the access in Riak; an empty `grants` list revokes
   all of them.
4. Reports the state of the certificate in `status.certificateReady` / `status.certificateError`
   and the `CertificateReady` condition. `status.phase: Ready` only means the Riak-side identity
   exists.

Deleting a RiakUser runs `riak-admin security del-user`, which removes the user together with its
grants and certificate source. This is best effort: it is skipped when the cluster is missing,
being deleted or not Ready, and given up after two minutes of failures so the RiakUser never gets
stuck.

### Client certificates from cert-manager

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

The operator creates a cert-manager `Certificate` named `<riakuser-name>-client-tls` with
**`commonName` set to `spec.username`**. Usages: `client auth`, `digital signature`,
`key encipherment`. cert-manager writes the issued certificate to the Secret (default
`<riakuser-name>-client-tls`) with `tls.crt`, `tls.key` and `ca.crt`, and renews it.

### Connecting a client

Mount the Secret that holds the user's certificate into the application pod (for cert-manager
users that is `<riakuser-name>-client-tls`, for external users the Secret you named in
`externalSecretName`) and connect to the protobuf port with TLS, presenting the client
certificate. `ca.crt` must be the CA that signed the cluster's node certificate:

```yaml
volumes:
  - name: riak-client-tls
    secret:
      secretName: app-cert-user-client-tls   # or your externalSecretName
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
authenticated write/read, which is useful for verifying certificate auth from a debug pod.

## Client certificates from an external CA

If your users' certificates are issued by another CA (a corporate PKI, Vault, ...), tell the
cluster to **trust that CA** and point each `RiakUser` at the **Secret that already holds its
certificate**. Nothing about the user's certificate is requested from cert-manager.

```yaml
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: my-cluster
spec:
  size: 3
  tls:
    enabled: true
    certManager:
      issuerName: riak-ca-issuer
    additionalClientCAs:            # extra CAs Riak trusts for client certificates
      - secretRef:
          name: corp-pki-root
          key: ca.crt
      # - configMapRef:             # e.g. a bundle published by trust-manager
      #     name: corp-trust-bundle
      #     key: ca-bundle.pem
---
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: app-ext-user
spec:
  clusterName: my-cluster
  username: appuser                 # must equal the certificate's CommonName
  certificateRef:
    externalSecretName: app-ext-user-cert   # instead of issuerRef
  grants:
    - resource: bucket
      bucketName: mydata
      permission: read
```

**What the operator does**

- It merges the cluster's own CA with every `additionalClientCAs` entry (in that order, without
  duplicates) into the Secret `<cluster>-tls-trust` and mounts it as Riak's `ca.crt`
  (`ssl.cacertfile`). The server certificate and key still come from cert-manager. An entry is a
  Secret or ConfigMap key holding one or more PEM `CERTIFICATE` blocks. Certificates are public,
  so nothing sensitive is copied.
- For a RiakUser with `externalSecretName` it creates **no** `Certificate`. It still creates the
  Riak user and its `certificate` source, then validates the Secret's `tls.crt` (leaf first, then
  any intermediates): the CommonName equals `spec.username`, the `client auth` usage is present,
  it is currently valid, and it **chains to a CA Riak trusts**. The result is in
  `status.certificateReady` / `status.certificateError` and the `CertificateReady` condition, so
  a certificate Riak would reject shows up in Kubernetes instead of as a failed connection. A
  valid external certificate is re-checked every 10 minutes (sooner when it is about to expire).
- `issuerRef` and `externalSecretName` are mutually exclusive; `secretName` only applies to
  `issuerRef`.

`status.tlsStatus.trustedClientCAs` shows how many distinct CAs are trusted, and
`status.tlsStatus.trustBundleError` explains a bad `additionalClientCAs` entry (missing
Secret/ConfigMap/key, no PEM certificate). A bad entry does not fail the reconcile: pods keep
their current bundle.

!!! note "Requirements and limits"
    - The operator needs RBAC to read Secrets (`get`) and to write the trust Secret
      (`create`/`update`); the chart and `config/rbac` include it. Secrets are read uncached, so no
      informer over every Secret in the cluster is started.
    - The kubelet updates the mounted bundle by itself after a CA change. Riak's `ssl` stack may
      read it at start-up only, so **restart the Riak pods after changing the trusted CAs** (it
      has not been verified that a running node picks up a new `ca.crt`).
    - There is no CRL/OCSP check (Riak runs with `check_crl=off`), so revoke access by removing the
      RiakUser or distrusting the CA.
    - The server (node) certificate still has to come from cert-manager; external server
      certificates are not supported yet.
    - The operator does not renew an external certificate. It reports expiry in status; renewal
      and updating the Secret are yours.

## Troubleshooting

**`RiakUser` shows `Cert` false or `certificateError` set** — read the error first:

```bash
kubectl get riakuser <name> -o jsonpath='{.status.certificateError}{"\n"}'
```

- *cert-manager users:* check that the `Certificate` was issued:

    ```bash
    kubectl get certificate <riakuser-name>-client-tls -o wide
    kubectl describe certificate <riakuser-name>-client-tls
    ```

- *External-CA users:* the error names what failed: Secret or `tls.crt` missing, CommonName not
  equal to `spec.username`, no `client auth` usage, expired, or not chaining to a trusted CA. For
  the last one, add the CA to `spec.tls.additionalClientCAs` and check
  `status.tlsStatus.trustBundleError`.

**Clients get `certificate verify failed`** — the client certificate must chain to a CA the
cluster trusts. See which CAs Riak was given:

```bash
kubectl get secret <cluster>-tls-trust -o jsonpath='{.data.ca\.crt}' | base64 -d | \
  openssl crl2pkcs7 -nocrl -certfile /dev/stdin | openssl pkcs7 -print_certs -noout
```

`<cluster>-tls-trust` is the cluster CA plus every `additionalClientCAs` entry. For cert-manager
users also verify both issuers reference the same CA secret. In the other direction, the client
must trust the CA that signed the *node* certificate (`<cluster>-tls`, key `ca.crt`).

**Authentication fails despite a valid certificate** — the certificate CN must equal the Riak
username. Inspect it (use the Secret you actually mounted):

```bash
kubectl get secret <secret-name> -o jsonpath='{.data.tls\.crt}' | \
  base64 -d | openssl x509 -noout -subject
```

**Certificates are created but pods have no TLS volume** — `spec.tls.enabled`
must be `true`; setting only `certManager` is not enough.

**External CA changed but clients still fail** — restart the Riak pods; a running node may not
re-read `ca.crt`.
