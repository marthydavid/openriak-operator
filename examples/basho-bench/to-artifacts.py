#!/usr/bin/env python3
"""Merge a Basho Bench run into the artifact format of `test/scale -soak`, for hack/soak-report.py.

    python3 to-artifacts.py RUN_DIR LOGS_DIR ART_DIR [--rate 600 --value-size 131072 ...]
    python3 hack/soak-report.py ART_DIR docs/assets/basho-bench

RUN_DIR is what collect.py recorded; LOGS_DIR holds each client Job's log as client-NN.log
(`kubectl -n bench logs job/bsoak-client-NN`), which carries the @@START line and the latency CSVs
that soak-like.py makes every client print. Writes samples.jsonl, metrics.jsonl, nodes.jsonl and
summary.json to ART_DIR.

Basho Bench does not read keys back, so the summary carries no integrity result; "passed" means
at least 95 % of the target rate, an error rate under 0.1 %, no OOM kill and no restart.
"""
import argparse, glob, json, os, re, shutil, statistics, subprocess
from datetime import datetime

p = argparse.ArgumentParser()
p.add_argument("run"); p.add_argument("logs"); p.add_argument("art")
p.add_argument("--rate", type=float, default=600); p.add_argument("--value-size", type=int, default=131072)
p.add_argument("--users", type=int, default=10); p.add_argument("--buckets", type=int, default=10)
p.add_argument("--memory", default="16Gi"); p.add_argument("--storage", default="300Gi")
p.add_argument("--cpu", default="4"); p.add_argument("--replicas", type=int, default=3)
p.add_argument("--namespace", default="bench"); p.add_argument("--cluster", default="bsoak")
a = p.parse_args()
os.makedirs(a.art, exist_ok=True)


def ts(t): return datetime.fromisoformat(t).timestamp()


def jl(path):
    return [json.loads(l) for l in open(path) if l.strip()] if os.path.exists(path) else []


def parse_log(path):
    """-> (start epoch, {csv name: rows}); rows are lists of floats, header skipped."""
    start, csvs, cur = None, {}, None
    for line in open(path):
        line = line.rstrip("\n")
        m = re.match(r"@@START (\d+)", line)
        if m:
            start = int(m.group(1)); continue
        m = re.match(r"@@CSV (\w+)", line)
        if m:
            cur = m.group(1); csvs[cur] = []; continue
        if line.startswith("@@END"):
            cur = None; continue
        if cur and line and not line.startswith(("elapsed", '"error"')):
            try:
                csvs[cur].append([float(x) for x in line.split(",")])
            except ValueError:
                pass
    return start, csvs


clients = {}
for path in sorted(glob.glob(os.path.join(a.logs, "client-*.log"))):
    name = "bsoak-" + os.path.basename(path)[:-4]
    start, csvs = parse_log(path)
    if start and csvs.get("get_latencies"):
        clients[name] = (start, csvs)
if not clients:
    raise SystemExit("no client logs with @@START and latency CSVs in " + a.logs)
t_load = min(s for s, _ in clients.values())


def window(rows, lo, hi):
    """Latency rows (elapsed, window, n, min, mean, median, p95, p99, p99.9, max, errors) in (lo, hi]."""
    return [r for r in rows if lo < r[0] <= hi and r[2] > 0]


def op_stats(rows):
    if not rows:
        return None, 0, 0, 0
    n = sum(r[2] for r in rows); secs = sum(r[1] for r in rows) or 1
    lat = {"p50": statistics.median(r[5] for r in rows) / 1000, "p95": max(r[6] for r in rows) / 1000,
           "p99": max(r[7] for r in rows) / 1000, "max": max(r[9] for r in rows) / 1000}
    return lat, n, secs, sum(r[10] for r in rows)


pods = jl(os.path.join(a.run, "pods.jsonl"))
samples, interval = [], 30
for row in pods:
    t = ts(row["time"]); elapsed = t - t_load
    if elapsed < 0:
        continue
    cl, rate, errs, ops, p99 = {}, 0.0, 0, 0, 0.0
    for name, (start, csvs) in clients.items():
        e = t - start
        rec = {}
        for op, key in (("Put", "put_latencies"), ("Get", "get_latencies")):
            lat, n, secs, err = op_stats(window(csvs.get(key, []), e - interval, e))
            if lat:
                rec[op] = lat; rec[op + "s"] = n; rate += n / secs; errs += err; ops += n
                p99 = max(p99, lat["p99"])
        if rec:
            rec["OpsPerS"] = (rec.get("Puts", 0) + rec.get("Gets", 0)) / interval
            cl[name] = rec
    if not cl:
        continue
    mem = row.get("pod_memory_bytes") or {}
    samples.append({"time": row["time"], "elapsed_s": elapsed, "nodes": a.replicas, "memory_limit": a.memory,
                    "pod_memory_bytes": mem, "pod_disk_used_gib": row.get("pod_disk_used_gib") or {},
                    "sample": {"ClusterReady": True, "AllNodesUp": True, "Rate": rate,
                               "ErrRate": errs / (ops + errs) if ops + errs else 0.0, "P99": p99,
                               "DiskPct": row.get("disk_pct", 0), "OOMKills": 0, "Restarts": 0,
                               "MemPct": max(mem.values(), default=0) / (2**30 * float(re.sub("[A-Za-z]", "", a.memory))) * 100},
                    "clients": cl})
with open(os.path.join(a.art, "samples.jsonl"), "w") as f:
    for s in samples:
        f.write(json.dumps(s) + "\n")
for name in ("metrics.jsonl", "nodes.jsonl"):
    shutil.copy(os.path.join(a.run, name), os.path.join(a.art, name)) if os.path.exists(os.path.join(a.run, name)) else None


def pod_counts():
    """OOM kills and restarts of the Riak containers, from the live pods."""
    out = subprocess.run(["kubectl", "-n", a.namespace, "get", "pods", "-o", "json"], capture_output=True, text=True).stdout
    oom = restarts = 0
    for pod in json.loads(out or '{"items":[]}')["items"]:
        if re.fullmatch(a.cluster + r"-\d", pod["metadata"]["name"]):
            for cs in pod["status"].get("containerStatuses", []):
                if cs["name"] == "riak":
                    restarts += cs.get("restartCount", 0)
                    oom += int(((cs.get("lastState") or {}).get("terminated") or {}).get("reason") == "OOMKilled")
    return oom, restarts


puts = int(sum(r[2] for _, c in clients.values() for r in c.get("put_latencies", [])))
gets = int(sum(r[2] for _, c in clients.values() for r in c.get("get_latencies", [])))
errors = int(sum(r[10] for _, c in clients.values() for k in ("put_latencies", "get_latencies") for r in c.get(k, [])))
oom, restarts = pod_counts()
live = [s["sample"]["Rate"] for s in samples]
avg = statistics.mean(live) if live else 0
passed = avg >= 0.95 * a.rate and errors / max(puts + gets + errors, 1) < 0.001 and oom == 0 and restarts == 0
summary = {
    "passed": passed, "oom_kills": oom, "container_restarts": restarts, "timeline": [],
    "config": {"rate_ops_per_s": a.rate, "users": a.users, "buckets": a.buckets, "value_size_bytes": a.value_size,
               "n_val": 3, "pr": 2, "pw": 2, "memory": a.memory, "storage": a.storage, "cpu_request": a.cpu,
               "replicas": a.replicas, "tool": "basho_bench", "workers_per_client": 16, "read_ratio": 0.5},
    "results": {"Total": {"ops": puts + gets, "puts": puts, "gets": gets, "errors": errors}},
}
json.dump(summary, open(os.path.join(a.art, "summary.json"), "w"), indent=2)
print(f"{len(samples)} samples, {puts + gets:,} ops ({puts:,} puts, {gets:,} gets), {errors} errors, "
      f"avg {avg:.1f} ops/s of {a.rate:.0f}, OOM {oom}, restarts {restarts}, passed={passed}")
