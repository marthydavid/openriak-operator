#!/usr/bin/env bash
# First disaster-recovery game day (issue #98): hold a constant load on one TLS cluster with the soak
# harness and inject failures one after another, timing how long the cluster takes to recover.
#
#   hack/dr-gameday.sh                       # needs a kubectl context, the operator installed, cert-manager
#   DRY_RUN=1 hack/dr-gameday.sh             # print what it would do
#   RATE=200 BASELINE=600 hack/dr-gameday.sh # knobs, see below
#   SKIP=D9 hack/dr-gameday.sh               # leave faults out (comma separated IDs)
#
# Faults, in order (IDs are the scenarios of issue #98):
#   D1  delete one Riak pod
#   D7  delete two Riak pods at once (no quorum for a while)
#   D5  delete one node's PVC and pod (its data is lost)
#   D9  kill the operator while the cluster is scaled from 3 to 4 nodes
#   D3  isolate one Riak pod with a NetworkPolicy, then lift it
#
# Run it detached (a terminal timeout would kill a long run): nohup hack/dr-gameday.sh > gameday.log 2>&1 &
# Recovery times are measured by polling kubectl, so their resolution is a few seconds (more on a slow link).
# Everything lands in $OUT: events.log (one line per step), soak/ (the harness artifacts), summary.txt.
#
# Only touches namespace $NS (created and deleted by the soak harness) and, for D9, the operator pod
# in $OP_NS. Never run it against a cluster whose control plane is the thing under test.
set -uo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"
SCALE=${SCALE:-"go run ./test/scale"}   # or a prebuilt test/scale binary (GOOS=linux go build -o scale ./test/scale)

NS=${NS:-dr-gameday}
OP_NS=${OP_NS:-openriak-operator}
OUT=${OUT:-dr-gameday-$(date +%Y%m%d-%H%M)}
RATE=${RATE:-200}                    # ops/s, all clients together
BASELINE=${BASELINE:-600}            # seconds of load before the first fault
SETTLE=${SETTLE:-180}                # seconds of load after each recovery
RECOVER_TIMEOUT=${RECOVER_TIMEOUT:-1500}
STORAGE_CLASS=${STORAGE_CLASS:-lvms-vg1}
IMAGE=${IMAGE:-ghcr.io/marthydavid/riak:3.2.6}
SOAK_MIN=${SOAK_MIN:-120}            # minutes of load the harness holds; must cover baseline, all faults and settling
CLUSTER=soak                         # the harness names its cluster "soak"
NODES=3                              # how many nodes the cluster should have (D9 raises it to 4)
skip() { case ",${SKIP:-}," in *",$1,"*) ev "SKIPPED $1"; return 0 ;; esac; return 1; }

mkdir -p "$OUT"
EV="$OUT/events.log"
run() { if [ -n "${DRY_RUN:-}" ]; then echo "+ $*"; else "$@"; fi; }
ev() { local line; line="$(date +%s) $*"; echo "$line" | tee -a "$EV"; }
k() { kubectl -n "$NS" "$@"; }

# status "phase readyNodes totalNodes" of the cluster
cstat() { k get riakcluster "$CLUSTER" -o jsonpath='{.status.phase} {.status.readyNodes} {.status.totalNodes}' 2>/dev/null; }

# wait_ready N: until the cluster is Ready with N nodes (all pods Running); prints the seconds taken
wait_ready() {
  local want=$1 t0 s
  t0=$(date +%s)
  while :; do
    s=$(cstat)
    case "$s" in "Ready $want $want") if [ "$(k get pods -l app=riak,cluster=$CLUSTER --no-headers 2>/dev/null | grep -c ' Running ')" -ge "$want" ]; then break; fi ;; esac
    [ $(( $(date +%s) - t0 )) -gt "$RECOVER_TIMEOUT" ] && { echo "TIMEOUT"; return 1; }
    sleep 5
  done
  echo $(( $(date +%s) - t0 ))
}

fault() { # fault ID "description" command...
  local id=$1 desc=$2; shift 2
  ev "FAULT_START $id $desc"
  run "$@"
}
recovered() { # recovered ID nodes
  local secs
  # The operator refreshes the status every 10 s: without a pause the old "Ready" would still be read.
  sleep 25
  secs=$(wait_ready "$2") || { ev "RECOVERY_TIMEOUT $1 after ${RECOVER_TIMEOUT}s: $(cstat)"; return 1; }
  ev "FAULT_END $1 ready_after_s=$secs"
  k get pods -l "app=riak,cluster=$CLUSTER" --no-headers >> "$OUT/pods-after-$1.txt" 2>&1
  sleep "$SETTLE"
}

ev "START namespace=$NS rate=$RATE image=$IMAGE operator=$(kubectl -n "$OP_NS" get deploy -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null)"

# 1. the load: -soak-pod-anti-affinity Preferred so a 4th node can be scheduled for D9; -keep so the
#    namespace survives for -verify-only.
LOG="$OUT/harness.log"
run nohup $SCALE -soak -keep -namespace "$NS" \
  -soak-rate "$RATE" -soak-duration "${SOAK_MIN}m" -soak-no-scale -soak-pod-anti-affinity Preferred \
  -soak-storage 50Gi -soak-memory 8Gi -soak-cpu 2 -soak-value-size 16384 -soak-threads 4 \
  -soak-artifacts "$OUT/soak" -storage-class "$STORAGE_CLASS" -image "$IMAGE" \
  -operator-namespace "$OP_NS" > "$LOG" 2>&1 &
HPID=$!
[ -n "${DRY_RUN:-}" ] && echo "(dry run: stopping before waiting for the cluster)" && exit 0

ev "waiting for the cluster and the clients"
until [ "$(cstat)" = "Ready 3 3" ] && [ "$(k get pods --no-headers 2>/dev/null | grep -c 'soak-client.*Running')" -ge 1 ]; do
  grep -q "scale test failed" "$LOG" && { ev "ABORT harness failed: $(tail -2 "$LOG")"; exit 1; }
  sleep 10
done
ev "LOAD_STEADY baseline ${BASELINE}s"
sleep "$BASELINE"

# D1: one pod
fault D1 "delete pod $CLUSTER-1" k delete pod "$CLUSTER-1" --wait=false
recovered D1 3

# D7: two pods at once
fault D7 "delete pods $CLUSTER-1 and $CLUSTER-2" k delete pod "$CLUSTER-1" "$CLUSTER-2" --wait=false
recovered D7 3

# D5: lose one node's data
fault D5 "delete PVC data-$CLUSTER-2 and pod $CLUSTER-2" bash -c "kubectl -n $NS delete pvc data-$CLUSTER-2 --wait=false; kubectl -n $NS delete pod $CLUSTER-2 --wait=false"
recovered D5 3
# did the node really rejoin the ring with its old name? record the ring as Riak sees it
k exec "$CLUSTER-0" -c riak -- sh -c 'VMARGS_PATH=$(ls -1 /var/lib/riak/generated.conf/vm.*.args | tail -1) riak-admin member-status' \
  > "$OUT/member-status-after-D5.txt" 2>&1

# D9: operator killed mid scale-up (3 -> 4)
if ! skip D9; then
NODES=4
fault D9 "scale to 4 and kill the operator" bash -c "kubectl -n $NS patch riakcluster $CLUSTER --type merge -p '{\"spec\":{\"size\":4}}'; sleep 3; kubectl -n $OP_NS delete pod -l control-plane=controller-manager --wait=false"
recovered D9 "$NODES"
fi

# D3: network isolation of one pod, 3 minutes
fault D3 "isolate $CLUSTER-1 with a NetworkPolicy" bash -c "cat <<EOF | kubectl -n $NS apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: dr-isolate}
spec:
  podSelector: {matchLabels: {statefulset.kubernetes.io/pod-name: $CLUSTER-1}}
  policyTypes: [Ingress, Egress]
EOF"
sleep 180
ev "HEAL D3 removing the NetworkPolicy"
run k delete networkpolicy dr-isolate
recovered D3 "$NODES"

# the harness runs until its duration is over, then reads every key back
ev "waiting for the load to finish (up to ${SOAK_MIN} minutes after start)"
while kill -0 "$HPID" 2>/dev/null; do sleep 30; done
ev "LOAD_DONE"

# the Riak side must equal the CRs on every node
$SCALE -verify-only -namespace "$NS" -operator-namespace "$OP_NS" > "$OUT/verify.log" 2>&1
ev "VERIFY exit=$? $(grep -E 'MATCH|MISMATCH' "$OUT/verify.log" | head -1)"

# summary: per fault, RTO and the lowest rate / highest error share while it lasted
python3 - "$OUT" <<'PY' | tee "$OUT/summary.txt"
import json, sys, os
out = sys.argv[1]
ev = [l.split(" ", 2) for l in open(os.path.join(out, "events.log")) if l.strip()]
samples = [json.loads(l) for l in open(os.path.join(out, "soak", "samples.jsonl"))] if os.path.exists(os.path.join(out, "soak", "samples.jsonl")) else []
def at(t): return [s for s in samples if s["time"]]
import datetime
def ts(s): return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
target = max([s["sample"]["Rate"] for s in samples] + [1])
print(f"{'fault':6} {'ready after':>12} {'min rate':>9} {'max err %':>9}  (target ~{target:.0f} ops/s)")
starts = {}
for t, kind, rest in ev:
    t = int(t)
    if kind == "FAULT_START": starts[rest.split()[0]] = t
    if kind == "FAULT_END":
        fid = rest.split()[0]; secs = rest.split("ready_after_s=")[1].split()[0]
        win = [s for s in samples if starts[fid] <= ts(s["time"]) <= t + 60]
        mn = min((s["sample"]["Rate"] for s in win), default=float("nan"))
        er = max((s["sample"]["ErrRate"] for s in win), default=float("nan")) * 100
        print(f"{fid:6} {secs+'s':>12} {mn:9.0f} {er:9.2f}")
    if kind in ("RECOVERY_TIMEOUT", "ABORT", "VERIFY"):
        print(kind, rest)
PY
ev "DONE results in $OUT; delete the namespace with: kubectl delete namespace $NS"
