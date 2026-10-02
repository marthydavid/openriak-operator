#!/bin/bash
# Riak KV 3.2 entrypoint for Kubernetes StatefulSets.
#
# Environment variables (all optional):
#   POD_NAME          - injected by operator via fieldRef metadata.name
#   POD_IP            - injected by operator via fieldRef status.podIP
#   POD_NAMESPACE     - injected by operator via fieldRef metadata.namespace
#   RIAK_CLUSTER_NAME - injected by operator (equals cluster CR name)
#   RIAK_NODE         - override full Erlang node name
#   RIAK_COOKIE       - Erlang distribution cookie (default: riak)
#   RIAK_RING_SIZE    - number of consistent-hash partitions (default: 128)
#   RIAK_STORAGE_BACKEND - bitcask | leveldb | memory (default: bitcask)
#   RIAK_CONFIG_*     - arbitrary riak.conf overrides (see below)
#
# RIAK_CONFIG_* env vars map to riak.conf keys:
#   double-underscore → dot,  single-underscore → underscore, all lowercase
#   e.g.  RIAK_CONFIG_TRANSFER__LIMIT=4  →  transfer_limit = 4
#         RIAK_CONFIG_ANTI__ENTROPY=on   →  anti_entropy = on

set -eo pipefail

# NOTE: no runtime mkdir/chown here — the entrypoint runs as the riak user
# (USER riak), so mkdir/chown under root-owned paths fails and set -e kills
# the container before Riak starts. /var/run/riak and directory ownership
# are baked into the image at build time (Dockerfile, as root).

# Raise the open-file soft limit to Riak's recommended 65536. Containers default
# it to 1024 (CRI-O), which throttles Riak under load and logs a startup warning.
# The hard limit is high enough that raising the soft limit needs no privileges;
# ignore failure if the hard cap happens to be lower than 65536.
ulimit -n 65536 2>/dev/null || true

# ---------------------------------------------------------------------------
# Node identity
# Prefer DNS-based names (stable across pod restarts) when the StatefulSet
# headless service name can be inferred. Fall back to POD_IP if namespace or
# cluster name is not available.
# ---------------------------------------------------------------------------
# --- node-identity: begin (exercised by test/manifests/entrypoint_test.go) ---
POD_NAMESPACE="${POD_NAMESPACE:-default}"

# Node identity.
#
# Multi-node clusters need a node name every peer can resolve AND that Erlang
# accepts: with long names (-name) a dotless host such as "riak-0" is rejected as
# illegal by remote nodes, so a bare pod name only works for a single node. Use the
# pod's FQDN (<pod>.<headless-svc>.<ns>.svc.<cluster-domain>), which the kubelet
# writes into /etc/hosts for StatefulSet pods and which resolves cluster-wide
# through the headless service. Reading it from /etc/hosts avoids assuming the
# cluster DNS domain. (riak-admin itself must be pointed at the generated vm.args
# via VMARGS_PATH to find a node named this way; the operator does that.)
#
# The name must be the SAME on every start: Riak persists its ring on the data
# volume under the node name, and a node whose name is not a member of its own
# persisted ring cannot boot (riak_core_capability crashes with
# {function_clause,[{orddict,fetch,[<node>,[]]}...]} on every start; issue #59).
#
# /etc/hosts can be EMPTY for a moment right after this container starts: the
# kubelet regenerates the pod's shared hosts file (truncate + write) whenever it
# creates another container of the pod, e.g. the metrics-exporter sidecar that it
# starts right after this one. A single read in that window found no FQDN, so
# the node fell back to riak@<pod>, never became ready (riak-admin cannot reach
# a dotless long name; peers reject "Hostname <pod> is illegal"), and its ring
# was persisted under the short name; every later start, under the FQDN, then
# crashed (issue #59). So the lookup is retried, has a fallback that does not
# read /etc/hosts, and a pod of an operator-managed cluster (RIAK_CLUSTER_NAME
# set) never falls back to a dotless name.
_pod_fqdn=""
_name_source=""
lookup_hosts_fqdn() {
    # 1. The kubelet-managed /etc/hosts line for the pod IP.
    if [[ -n "${POD_IP}" ]]; then
        _pod_fqdn="$(awk -v ip="${POD_IP}" '$1 == ip && $2 ~ /\./ { print $2; exit }' /etc/hosts)"
        _name_source="/etc/hosts entry of pod IP ${POD_IP}"
    fi
    # 2. Any /etc/hosts name of this pod (does not depend on POD_IP being set).
    if [[ -z "${_pod_fqdn}" && -n "${POD_NAME}" ]]; then
        _pod_fqdn="$(awk -v pod="${POD_NAME}." '$1 !~ /^#/ { for (i = 2; i <= NF; i++) if (index($i, pod) == 1) { print $i; exit } }' /etc/hosts)"
        _name_source="/etc/hosts entry of ${POD_NAME}"
    fi
}
lookup_hosts_fqdn
if [[ -z "${_pod_fqdn}" && -n "${RIAK_CLUSTER_NAME}" && -z "${RIAK_NODE}" ]]; then
    # Operator-managed: give a concurrent hosts-file rewrite time to finish.
    RIAK_HOSTS_RETRIES="${RIAK_HOSTS_RETRIES:-25}"
    _try=1
    while [[ -z "${_pod_fqdn}" && "${_try}" -lt "${RIAK_HOSTS_RETRIES}" ]]; do
        sleep 0.2
        _try=$((_try + 1))
        lookup_hosts_fqdn
    done
    if [[ -n "${_pod_fqdn}" ]]; then
        _name_source="${_name_source}, read ${_try} times"
    fi
fi
# 3. Built from the StatefulSet's governing headless Service, which the operator
#    names <cluster>-headless, and the namespace's DNS search domain
#    (<namespace>.svc.<cluster-domain>) from /etc/resolv.conf.
if [[ -z "${_pod_fqdn}" && -n "${POD_NAME}" && -n "${RIAK_CLUSTER_NAME}" ]]; then
    _svc_domain="$(awk -v ns="${POD_NAMESPACE}.svc." '$1 == "search" { for (i = 2; i <= NF; i++) if (index($i, ns) == 1) { print $i; exit } }' /etc/resolv.conf 2>/dev/null || true)"
    if [[ -n "${_svc_domain}" ]]; then
        _pod_fqdn="${POD_NAME}.${RIAK_CLUSTER_NAME}-headless.${_svc_domain%.}"
        _name_source="headless Service ${RIAK_CLUSTER_NAME}-headless and /etc/resolv.conf"
    fi
fi
if [[ -n "${_pod_fqdn}" ]]; then
    _default_node="riak@${_pod_fqdn}"
elif [[ -n "${RIAK_CLUSTER_NAME}" && -z "${RIAK_NODE}" ]]; then
    # Operator-managed pod: refuse to start under a name that is not the pod's
    # FQDN. Exiting before Riak starts leaves the data volume untouched, and the
    # kubelet retries the container.
    echo "ERROR: cannot determine this pod's FQDN for the Riak node name" >&2
    echo "  (POD_NAME='${POD_NAME}' POD_IP='${POD_IP}' POD_NAMESPACE='${POD_NAMESPACE}')." >&2
    echo "  Refusing to start under a short name: it would persist a ring that later" >&2
    echo "  starts under the real name cannot boot from. /etc/hosts:" >&2
    sed 's/^/    /' /etc/hosts >&2 || true
    echo "  /etc/resolv.conf:" >&2
    sed 's/^/    /' /etc/resolv.conf >&2 || true
    exit 1
elif [[ -n "${POD_NAME}" ]]; then
    _default_node="riak@${POD_NAME}"
    _name_source="POD_NAME (single node only: Erlang peers reject dotless host names)"
else
    _default_node="riak@${POD_IP:-127.0.0.1}"
    _name_source="POD_IP"
fi

if [[ -n "${RIAK_NODE}" ]]; then
    _name_source="RIAK_NODE"
fi
RIAK_NODE="${RIAK_NODE:-${_default_node}}"
RIAK_COOKIE="${RIAK_COOKIE:-riak}"
RIAK_RING_SIZE="${RIAK_RING_SIZE:-128}"
RIAK_STORAGE_BACKEND="${RIAK_STORAGE_BACKEND:-bitcask}"

echo "Starting Riak node: ${RIAK_NODE} (name from ${_name_source})"
# --- node-identity: end ---

# ---------------------------------------------------------------------------
# Generate /etc/riak/riak.conf
# ---------------------------------------------------------------------------
cat > /etc/riak/riak.conf << EOF
## Auto-generated by entrypoint.sh — do not edit at runtime.

nodename = ${RIAK_NODE}
distributed_cookie = ${RIAK_COOKIE}

## Listeners
listener.http.internal = 0.0.0.0:8098
listener.protobuf.internal = 0.0.0.0:8087

## Ring / partitioning
ring_size = ${RIAK_RING_SIZE}

## Storage
storage_backend = ${RIAK_STORAGE_BACKEND}

## Logging — write to file; the entrypoint tails the file to stdout so
## kubectl logs still captures output.
log.console = file
log.console.file = /var/log/riak/console.log
log.console.level = info
log.crash = on
log.crash.size = 10MB

## Paths
platform_data_dir = /var/lib/riak
platform_log_dir = /var/log/riak
EOF

# ---------------------------------------------------------------------------
# Apply RIAK_CONFIG_* overrides
# ---------------------------------------------------------------------------
while IFS='=' read -r key value; do
    config_key="${key#RIAK_CONFIG_}"
    config_key="${config_key,,}"        # → lowercase
    config_key="${config_key//__/.}"    # __ → dot (e.g. anti__entropy → anti_entropy)
    printf '%s = %s\n' "${config_key}" "${value}"
done < <(env | grep '^RIAK_CONFIG_') >> /etc/riak/riak.conf

# ---------------------------------------------------------------------------
# Ensure the log file exists before we tail it.
# ---------------------------------------------------------------------------
touch /var/log/riak/console.log

# ---------------------------------------------------------------------------
# Keep the logs of a failed run on the data volume. The kubelet keeps only the
# previous container's output, so after two failed restarts the first failure
# (usually the interesting one) is gone. The newest RIAK_CRASH_LOG_KEEP runs are
# kept under /var/lib/riak/crash-logs/<UTC time>/.
# ---------------------------------------------------------------------------
CRASH_LOG_DIR=/var/lib/riak/crash-logs
RIAK_CRASH_LOG_KEEP="${RIAK_CRASH_LOG_KEEP:-5}"
save_crash_logs() {
    local dest
    dest="${CRASH_LOG_DIR}/$(date -u +%Y%m%dT%H%M%SZ)"
    mkdir -p "${dest}" 2>/dev/null || return 0
    {
        echo "reason: $1"
        echo "nodename: ${RIAK_NODE}"
        echo "name source: ${_name_source}"
        echo "POD_IP: ${POD_IP}"
    } > "${dest}/reason" 2>/dev/null || true
    cp -p /var/log/riak/*.log "${dest}/" 2>/dev/null || true
    if [[ -f /var/log/riak/erl_crash.dump ]] && command -v gzip >/dev/null 2>&1; then
        gzip -c /var/log/riak/erl_crash.dump > "${dest}/erl_crash.dump.gz" 2>/dev/null || true
    fi
    ls -1d "${CRASH_LOG_DIR}"/*/ 2>/dev/null | head -n "-${RIAK_CRASH_LOG_KEEP}" | xargs -r rm -rf || true
    echo "Saved this run's Riak logs to ${dest}" >&2
}
_last_crash="$(ls -1d "${CRASH_LOG_DIR}"/*/ 2>/dev/null | tail -1 || true)"
if [[ -n "${_last_crash}" ]]; then
    echo "Logs of the latest failed run are kept in ${_last_crash}:"
    sed 's/^/    /' "${_last_crash}reason" 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# The persisted ring must belong to this node name.
#
# Riak stores the ring on the data volume. If the newest ring file does not list
# this node as a member, riak_core cannot boot (riak_core_capability crashes with
# {function_clause,[{orddict,fetch,[<this node>,[]]}...]}) and the pod
# crash-loops forever (issue #59). That happens when the node once ran under a
# different name, e.g. a short name before the FQDN lookup above was hardened.
#  - A standalone ring (one member: the node never joined a cluster) is moved
#    aside; Riak then creates a fresh one-member ring for this name that owns
#    every partition again, so the vnode data on the volume is kept.
#  - A ring with several members holds cluster membership that cannot be
#    rewritten safely here, so the start is refused with instructions.
# ---------------------------------------------------------------------------
RIAK_RING_DIR=/var/lib/riak/ring
ring_members() {
    local script=/tmp/riak-ring-members.escript
    cat > "${script}" <<'ERL'
#!/usr/bin/env escript
%% Print the member node names of a riak_core ring file, one per line.
main([File]) ->
    {ok, Bin} = file:read_file(File),
    Ring = binary_to_term(Bin),
    chstate_v2 = element(1, Ring),                                   % #chstate_v2{}
    [io:format("~s~n", [Node]) || {Node, _} <- element(8, Ring)].  % .members
ERL
    # escript execs `erl`, which only the bundled ERTS provides (next to the
    # escript that /usr/bin/escript links to).
    local erts_bin
    erts_bin="$(dirname "$(readlink -f "$(command -v escript)")")"
    PATH="${erts_bin}:${PATH}" escript "${script}" "$1"
}
check_ring_identity() {
    local latest members count stale
    latest="$(ls -1 "${RIAK_RING_DIR}"/riak_core_ring.* 2>/dev/null | tail -1 || true)"
    [[ -n "${latest}" ]] || return 0
    if ! members="$(ring_members "${latest}" 2>&1)"; then
        echo "WARNING: could not read ring file ${latest}; skipping the node-name check: ${members}" >&2
        return 0
    fi
    if grep -qxF "${RIAK_NODE}" <<< "${members}"; then
        return 0
    fi
    count="$(grep -c . <<< "${members}" || true)"
    if [[ "${count}" -le 1 ]]; then
        stale="${RIAK_RING_DIR}.stale-$(date -u +%Y%m%dT%H%M%SZ)"
        echo "WARNING: the persisted ring belongs to '${members}', not to ${RIAK_NODE}." >&2
        echo "  It is a standalone ring (the node never joined a cluster), so it is moved" >&2
        echo "  to ${stale} and Riak starts with a fresh ring under its current name." >&2
        mv "${RIAK_RING_DIR}" "${stale}"
        return 0
    fi
    echo "ERROR: this node is ${RIAK_NODE}, but the persisted ring in ${RIAK_RING_DIR}" >&2
    echo "  does not list it as a member. Its members are:" >&2
    sed 's/^/    /' <<< "${members}" >&2
    echo "  Riak cannot start from this ring under a different node name. Either restore" >&2
    echo "  the node's previous name, or remove this node's data volume and replace the" >&2
    echo "  old member from a running node (riak-admin cluster force-replace <old> <new>," >&2
    echo "  then cluster plan / cluster commit)." >&2
    exit 1
}
check_ring_identity

# ---------------------------------------------------------------------------
# Start Riak in the foreground.
#
# 'riak start' daemonizes via run_erl + `riak console`. In some container
# environments (notably under Kubernetes with a fully-qualified Erlang node
# name) run_erl busy-loops at 100% CPU and the node never becomes responsive,
# so `riak start` blocks forever waiting on the node and no listeners bind.
# `riak foreground` runs the BEAM directly with -noinput (no run_erl, no stdin
# needed), which avoids that hang. We background it so we can wait for readiness
# and forward the console log to stdout. Riak's own deprecation notice also
# recommends `foreground` over `start`.
# ---------------------------------------------------------------------------
echo "Starting Riak (foreground)..."
/usr/sbin/riak foreground &
RIAK_PID=$!

# Stop Riak gracefully when the container is asked to terminate.
TERMINATING=""
trap 'TERMINATING=1; echo "Termination signal received, stopping Riak..."; /usr/sbin/riak stop 2>/dev/null || kill "${RIAK_PID}" 2>/dev/null || true' TERM INT

# ---------------------------------------------------------------------------
# Wait for Riak to fully start. 'riak ping' is NOT used: in this image it never
# reports pong even though the node is healthy (riak-admin RPC works), so gating
# on it made the entrypoint exit 1 after RIAK_START_TIMEOUT and crash-loop every
# pod. 'riak-admin status' reaches the node over the same RPC path the operator
# uses and prints its stats once riak_kv is serving.
# ---------------------------------------------------------------------------
echo "Waiting for Riak to be ready..."
RIAK_START_TIMEOUT="${RIAK_START_TIMEOUT:-120}"
ELAPSED=0
# riak-admin must be pointed at the generated vm.args (see the node-identity note
# above); the newest file is the one this start just generated. Until the first
# one exists (first start, before cuttlefish ran) riak-admin would fall back to
# the release's `-sname riak` vm.args and knock on the node under a short name,
# which the node logs as "Hostname <pod> is illegal", so it is not run then.
riak_admin() {
    local vmargs
    vmargs="$(ls -1 /var/lib/riak/generated.conf/vm.*.args 2>/dev/null | tail -1 || true)"
    [[ -n "${vmargs}" ]] || return 1
    VMARGS_PATH="${vmargs}" /usr/sbin/riak-admin "$@"
}
until riak_admin status 2>/dev/null | grep -q 'stats for'; do
    if ! kill -0 "${RIAK_PID}" 2>/dev/null; then
        echo "ERROR: Riak exited during startup" >&2
        cat /var/log/riak/console.log >&2
        save_crash_logs "Riak exited during startup"
        exit 1
    fi
    if [ "${ELAPSED}" -ge "${RIAK_START_TIMEOUT}" ]; then
        echo "ERROR: Riak did not become ready within ${RIAK_START_TIMEOUT}s" >&2
        cat /var/log/riak/console.log >&2
        save_crash_logs "Riak did not become ready within ${RIAK_START_TIMEOUT}s"
        exit 1
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done
echo "Riak is ready (riak-admin status)."

# ---------------------------------------------------------------------------
# Forward the console log to stdout so Kubernetes captures it via kubectl logs.
# --pid ties the tail's lifetime to the Riak process.
# ---------------------------------------------------------------------------
tail --pid "${RIAK_PID}" -F /var/log/riak/console.log &

# Block until Riak exits (crash or graceful stop); the container lifecycle
# tracks the node process. A crash (not a requested stop) keeps its logs.
RIAK_RC=0
wait "${RIAK_PID}" || RIAK_RC=$?
if [[ "${RIAK_RC}" -ne 0 && -z "${TERMINATING}" ]]; then
    save_crash_logs "Riak exited with status ${RIAK_RC}"
fi
exit "${RIAK_RC}"
