#!/usr/bin/env bash
# Checks that a Riak pod with the operator's security settings starts on a freshly provisioned PVC of
# the given StorageClass, without and with spec.podSecurityContext.fsGroup. Use it to verify a new
# platform (AKS, RKE2, ...) or storage driver. It creates one throwaway namespace and deletes it.
#
#   hack/verify-fsgroup.sh --context my-aks --storage-class managed-csi
#   hack/verify-fsgroup.sh --context my-okd --storage-class lvms-vg1 --openshift
#
# --openshift skips the fsGroup pod: SCC assigns UID and fsGroup there, so the plain pod is the test.
set -euo pipefail

CTX="" SC="" IMAGE="ghcr.io/marthydavid/riak:3.2.6" NS="riak-fsgroup-verify" OPENSHIFT=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --context) CTX="$2"; shift 2 ;;
    --storage-class) SC="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --namespace) NS="$2"; shift 2 ;;
    --openshift) OPENSHIFT=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$CTX" && -n "$SC" ]] || { echo "usage: $0 --context CTX --storage-class SC [--image IMG] [--openshift]" >&2; exit 2; }

k() { kubectl --context "$CTX" --request-timeout=30s -n "$NS" "$@"; }
trap 'kubectl --context "$CTX" delete ns "$NS" --wait=false >/dev/null 2>&1 || true' EXIT

kubectl --context "$CTX" create ns "$NS" >/dev/null

# The riak group differs per image build, so read it from the image.
k run riak-id --restart=Never --image="$IMAGE" --command -- id riak >/dev/null
for _ in $(seq 1 60); do k logs riak-id 2>/dev/null | grep -q uid && break; sleep 3; done
GID=$(k logs riak-id | sed -n 's/.*gid=\([0-9]*\).*/\1/p')
[[ -n "$GID" ]] || { echo "could not read the riak gid from $IMAGE" >&2; exit 1; }
echo "image $IMAGE: riak gid=$GID"

pod() { # name, extra pod securityContext fields
  cat <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data-$1}
spec: {accessModes: [ReadWriteOnce], storageClassName: $SC, resources: {requests: {storage: 1Gi}}}
---
apiVersion: v1
kind: Pod
metadata: {name: $1}
spec:
  securityContext: {seccompProfile: {type: RuntimeDefault}$2}
  containers:
  - name: riak
    image: $IMAGE
    stdin: true
    tty: true
    env:
    - {name: POD_NAME, valueFrom: {fieldRef: {fieldPath: metadata.name}}}
    - {name: POD_IP, valueFrom: {fieldRef: {fieldPath: status.podIP}}}
    - {name: POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
    - {name: RIAK_CLUSTER_NAME, value: fsverify}
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    resources: {requests: {memory: 512Mi, cpu: 100m}}
    volumeMounts: [{name: data, mountPath: /var/lib/riak}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: data-$1}}]
EOF
}

PODS=(nofs)
pod nofs "" | k apply -f - >/dev/null
if [[ $OPENSHIFT -eq 0 ]]; then
  PODS+=(withfs)
  pod withfs ", fsGroup: $GID, fsGroupChangePolicy: OnRootMismatch" | k apply -f - >/dev/null
fi

status=0
for p in "${PODS[@]}"; do
  result=timeout
  for _ in $(seq 1 60); do
    # Both markers appear in the entrypoint output: a start prints the Erlang Root line, a failure the ERROR line.
    if k logs "$p" 2>/dev/null | grep -q "ERROR: Riak exited during startup"; then result=FAILED; break; fi
    if k exec "$p" -- sh -c 'ls /var/lib/riak/generated.conf' >/dev/null 2>&1; then result=STARTED; break; fi
    sleep 5
  done
  echo "== $p: $result"
  k exec "$p" -- sh -c 'id; ls -ld /var/lib/riak' 2>/dev/null || true
  if [[ $result != STARTED ]]; then
    k logs "$p" --tail=5 2>&1 | cut -c1-160 || true
    # A plain pod failing on a CSI volume is the expected, informative result; only the pod that
    # is supposed to work (withfs, or nofs under --openshift) fails the script.
    if [[ $p == withfs || $OPENSHIFT -eq 1 ]]; then status=1; fi
  fi
done

echo
echo "nofs FAILED + withfs STARTED  -> this driver needs spec.podSecurityContext.fsGroup"
echo "both STARTED                  -> volumes are writable as is (e.g. local-path); fsGroup is optional"
echo "nofs STARTED (--openshift)    -> the SCC assigned a usable fsGroup; leave podSecurityContext unset"
exit $status
