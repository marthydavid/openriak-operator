#!/usr/bin/env bash
# verify-cert-bucket.sh — verify a Riak bucket is reachable and writable over the
# Protocol Buffers interface using certificate-based (mTLS) authentication.
#
# It launches a short-lived pod with the RiakUser's client certificate (issued by
# cert-manager, CN == username) mounted, then runs riak_cert_write.py inside it to:
#   1. PUT a key   -> confirms the bucket type is WRITABLE
#   2. GET the key -> confirms the bucket is WORKING (data persisted, readable)
#
# All authentication is over protobuf (port 8087); Riak certificate auth is only
# supported on that interface.
#
# Usage:
#   verify-cert-bucket.sh <namespace> <cluster> <bucket-type> <bucket> <client-cert-secret> <riak-username>
#
# Requires: kubectl, a running TLS-enabled RiakCluster, and the client cert Secret
# (tls.crt / tls.key / ca.crt) created by cert-manager for the RiakUser.
set -euo pipefail

NS="${1:?namespace required}"
CLUSTER="${2:?cluster name required}"
BUCKET_TYPE="${3:?bucket type required}"
BUCKET="${4:?bucket name required}"
CERT_SECRET="${5:?client cert secret required}"
RIAK_USER="${6:?riak username required}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POD="riak-cert-verify-$$"
HOST="${CLUSTER}.${NS}.svc.cluster.local"
PB_PORT=8087
# Any image with a stdlib python3 works — the check speaks the Riak protobuf
# protocol directly, so no pip install (and no cluster internet access) is needed.
CLIENT_IMAGE="${CLIENT_IMAGE:-python:3.11-slim}"

cleanup() {
    kubectl delete pod "${POD}" -n "${NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> Launching client pod ${POD} with certificate ${CERT_SECRET}"
kubectl apply -n "${NS}" -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  labels:
    app: riak-cert-verify
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: ${CLIENT_IMAGE}
      command: ["sleep", "600"]
      volumeMounts:
        - name: client-cert
          mountPath: /certs
          readOnly: true
  volumes:
    - name: client-cert
      secret:
        secretName: ${CERT_SECRET}
YAML

echo "==> Waiting for the client pod to be Ready"
kubectl wait --for=condition=Ready "pod/${POD}" -n "${NS}" --timeout=120s

echo "==> Running protobuf write/read check against ${HOST}:${PB_PORT} as ${RIAK_USER}"
# Feed the script over stdin so we don't depend on `tar` (needed by kubectl cp)
# being present in the slim client image.
kubectl exec -i "${POD}" -n "${NS}" -- python3 - \
    "${HOST}" "${PB_PORT}" "${RIAK_USER}" \
    /certs/tls.crt /certs/tls.key /certs/ca.crt \
    "${BUCKET_TYPE}" "${BUCKET}" \
    < "${SCRIPT_DIR}/pb_cert_auth_check.py"
