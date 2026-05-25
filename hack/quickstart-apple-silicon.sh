#!/usr/bin/env bash
# Quickstart for OpenRiak Operator on Apple Silicon (M1/M2/M3/M4).
#
# Architecture note
# -----------------
# The Riak 3.2 RPM is x86_64-only (no arm64 package available from files.tiot.jp).
# Rather than trying to pull an amd64 image into an arm64 Kind cluster — which
# containerd rejects as a platform mismatch — this script creates an amd64 Kind
# cluster that runs via Rosetta 2 emulation.  Everything in the cluster then
# runs as linux/amd64, which is the only architecture Riak supports.
#
# Trade-off: the operator binary is also built for amd64 (cross-compiled on the
# host), so container start-up is slightly slower than a native arm64 setup.
# Runtime performance of the operator itself is unaffected once running.
#
# Supported container runtimes
# ----------------------------
# Docker Desktop  — enable "Use Rosetta for x86_64/amd64 emulation on Apple Silicon"
#                   in Settings → General.
# Rancher Desktop — select "dockerd (moby)" in Preferences → Container Engine.
#
# Required tools: docker, kind, kubectl, go (≥1.22), make

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-riak-local}"
OPERATOR_IMG="${OPERATOR_IMG:-openriak-operator:dev}"
NAMESPACE="openriak-system"

# Run everything as linux/amd64 so the Riak image platform matches the cluster nodes.
export DOCKER_DEFAULT_PLATFORM=linux/amd64

# ── helpers ──────────────────────────────────────────────────────────────────

info()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()    { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
die()   { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
    for cmd in "$@"; do
        command -v "$cmd" >/dev/null 2>&1 || die "'$cmd' not found — install it first."
    done
}

# ── preflight ────────────────────────────────────────────────────────────────

require docker kind kubectl go make

ARCH=$(uname -m)
[[ "$ARCH" == "arm64" ]] || echo "Warning: this script targets Apple Silicon (arm64), got: $ARCH"

# Verify x86_64 emulation is available before spending time on a cluster.
info "Verifying x86_64 emulation (Rosetta 2 / QEMU)..."
if ! docker run --rm --platform linux/amd64 --entrypoint /bin/true alpine:3.20 2>/dev/null; then
    cat >&2 <<'HINT'
error: x86_64 emulation is not working. Fix for your runtime:

  Docker Desktop  → Settings → General →
    enable "Use Rosetta for x86_64/amd64 emulation on Apple Silicon"

  Rancher Desktop → Preferences → Container Engine →
    select "dockerd (moby)"
HINT
    exit 1
fi
ok "x86_64 emulation working"

# ── Kind cluster (amd64) ─────────────────────────────────────────────────────

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    info "Kind cluster '${CLUSTER_NAME}' already exists — skipping creation"
else
    info "Creating amd64 Kind cluster '${CLUSTER_NAME}' (Rosetta 2 emulation)..."
    kind create cluster --name "$CLUSTER_NAME" --wait 2m
    ok "Cluster created"
fi

kubectl cluster-info --context "kind-${CLUSTER_NAME}" >/dev/null

# ── Build operator image (linux/amd64 to match cluster nodes) ────────────────

info "Building operator image for linux/amd64..."
docker buildx build \
    --platform linux/amd64 \
    --load \
    -t "$OPERATOR_IMG" \
    .
ok "Built $OPERATOR_IMG"

info "Loading operator image into Kind..."
kind load docker-image "$OPERATOR_IMG" --name "$CLUSTER_NAME"
ok "Image loaded"

# ── Install CRDs and deploy operator ─────────────────────────────────────────

info "Installing CRDs..."
make install

info "Deploying operator (IMG=${OPERATOR_IMG})..."
make deploy IMG="$OPERATOR_IMG"

info "Waiting for operator to be ready..."
kubectl rollout status deployment \
    -n "$NAMESPACE" \
    -l control-plane=controller-manager \
    --timeout=120s
ok "Operator is running"

# ── Deploy a minimal single-node Riak cluster ─────────────────────────────────

info "Applying single-node Riak cluster example..."
kubectl apply -f examples/0-local-dev-cluster.yaml
ok "RiakCluster created"

cat <<'EOF'

  Riak runs as x86_64 on an amd64 Kind cluster (via Rosetta 2).
  First pod startup may take 2–3 minutes.

  Watch progress:
    kubectl get riakcluster riak-local -w
    kubectl get pods -l app.kubernetes.io/name=riak-local

  Once phase=Ready, apply buckets and users:
    kubectl apply -f examples/4-buckets.yaml
    kubectl apply -f examples/3-user-with-grants.yaml

  Forward the Riak protobuf port to localhost:
    kubectl port-forward svc/riak-local 8087:8087

  Tear down:
    kind delete cluster --name riak-local
EOF
