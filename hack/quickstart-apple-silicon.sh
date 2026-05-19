#!/usr/bin/env bash
# Quickstart for OpenRiak Operator on Apple Silicon (M1/M2/M3/M4).
#
# Architecture note
# -----------------
# The operator binary builds natively for linux/arm64 (Go cross-compiles fine).
# The Riak 3.2 image is x86_64-only (no ARM RPM available from files.tiot.jp).
# Riak pods run under x86_64 emulation — first startup is slower than on Intel.
#
# Supported container runtimes
# ----------------------------
# Docker Desktop  — enable "Use Rosetta for x86_64/amd64 emulation on Apple Silicon"
#                   in Settings → General for best emulation performance.
# Rancher Desktop — select the "dockerd (moby)" container engine in Preferences →
#                   Container Engine. x86_64 emulation runs via QEMU (slightly
#                   slower than Rosetta but fully functional).
#
# Both expose a `docker` CLI that `kind` can use without extra configuration.
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
[[ "$ARCH" == "arm64" ]] || echo "Warning: this script targets Apple Silicon (arm64), got: $ARCH"

# Verify x86_64 emulation works before spending time on a Kind cluster.
# Docker Desktop uses Rosetta 2; Rancher Desktop uses QEMU — both satisfy this check.
info "Verifying x86_64 emulation..."
if ! docker run --rm --platform linux/amd64 --entrypoint /bin/true alpine:3.20 2>/dev/null; then
    cat >&2 <<'HINT'
error: x86_64 emulation is not working. Fix for your runtime:

  Docker Desktop  → Settings → General →
    enable "Use Rosetta for x86_64/amd64 emulation on Apple Silicon"

  Rancher Desktop → Preferences → Container Engine →
    select "dockerd (moby)" (containerd does not support --platform for kind)
HINT
    exit 1
fi
ok "x86_64 emulation working"

# ── Kind cluster ─────────────────────────────────────────────────────────────

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    info "Kind cluster '${CLUSTER_NAME}' already exists — skipping creation"
else
    info "Creating Kind cluster '${CLUSTER_NAME}'..."
    kind create cluster --name "$CLUSTER_NAME" --wait 2m
    ok "Cluster created"
fi

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

info "Applying single-node Riak cluster example..."
kubectl apply -f examples/0-local-dev-cluster.yaml
ok "RiakCluster created"

cat <<'EOF'

  Riak runs as x86_64 under emulation — first startup may take 2–3 minutes.

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
