#!/usr/bin/env bash
# Quickstart for OpenRiak Operator on Apple Silicon (M1/M2/M3/M4).
#
# Architecture note
# -----------------
# The operator binary builds natively for linux/arm64 (Go cross-compiles fine).
# The Riak 3.2 image is x86_64-only (no ARM RPM available from files.tiot.jp).
# Docker Desktop on Apple Silicon runs x86_64 containers via Rosetta 2, so
# Riak pods work — but run under emulation and start more slowly than on Intel.
#
# Prerequisite: in Docker Desktop → Settings → General, enable
#   "Use Rosetta for x86_64/amd64 emulation on Apple Silicon".
#
# Required tools: docker, kind, kubectl, go (≥1.22), make

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-riak-local}"
OPERATOR_IMG="${OPERATOR_IMG:-openriak-operator:dev}"
NAMESPACE="openriak-system"

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
[[ "$ARCH" == "arm64" ]] || { echo "Warning: this script is designed for Apple Silicon (arm64), got: $ARCH"; }

# Confirm Docker Desktop Rosetta emulation is reachable by pulling a tiny amd64 image.
info "Verifying x86_64 emulation (Rosetta 2) via Docker Desktop..."
if ! docker run --rm --platform linux/amd64 --entrypoint /bin/true alpine:3.20 2>/dev/null; then
    die "x86_64 emulation failed. Enable 'Use Rosetta for x86_64/amd64 emulation on Apple Silicon' in Docker Desktop → Settings → General, then re-run."
fi
ok "Rosetta emulation working"

# ── Kind cluster ─────────────────────────────────────────────────────────────

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    info "Kind cluster '${CLUSTER_NAME}' already exists — skipping creation"
else
    info "Creating Kind cluster '${CLUSTER_NAME}'..."
    kind create cluster --name "$CLUSTER_NAME" --wait 2m
    ok "Cluster created"
fi

# Point kubectl at the new cluster.
kubectl cluster-info --context "kind-${CLUSTER_NAME}" >/dev/null

# ── Build operator image (linux/arm64, native speed) ─────────────────────────

info "Building operator image for linux/arm64..."
docker buildx build \
    --platform linux/arm64 \
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
# Using the local-dev example (1 node, modest resources, no anti-affinity)
# so it fits on a laptop without needing 3 schedulable nodes.

info "Applying single-node Riak cluster example..."
kubectl apply -f examples/0-local-dev-cluster.yaml
ok "RiakCluster created"

cat <<'EOF'

  Riak pods run as x86_64 under Rosetta 2 — first startup may take 2–3 minutes.

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
