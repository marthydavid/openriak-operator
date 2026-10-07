#!/usr/bin/env python3
"""Record the server-side series of a Basho Bench run, in the format of a soak run's artifacts.

    python3 collect.py OUT_DIR MINUTES [--namespace bench] [--cluster bsoak] [--interval 30]

Every cycle (at least INTERVAL seconds; it takes as long as the slowest kubectl call) it writes a row to
OUT_DIR/pods.jsonl (working set of each Riak container and data volume use), one row per Riak node to
metrics.jsonl (the node's riak-admin status as riak_* series) and one to nodes.jsonl (`kubectl top nodes`). Start it just before the
clients; to-artifacts.py merges it with the clients' own latency CSVs.
"""
import argparse, json, os, re, subprocess, sys, time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

p = argparse.ArgumentParser()
p.add_argument("out"); p.add_argument("minutes", type=float)
p.add_argument("--namespace", default="bench"); p.add_argument("--cluster", default="bsoak")
p.add_argument("--interval", type=int, default=30)
a = p.parse_args()
os.makedirs(a.out, exist_ok=True)


def k(*args, timeout=60):
    try:
        return subprocess.run(["kubectl", "-n", a.namespace, *args], capture_output=True, text=True, timeout=timeout).stdout
    except subprocess.TimeoutExpired:
        return ""


def now(): return datetime.now(timezone.utc).isoformat()


def mem_bytes(s):
    m = re.match(r"^(\d+)(Ki|Mi|Gi)?$", s)
    return int(m.group(1)) * {"Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, None: 1}[m.group(2)] if m else 0


def status(pod):
    out = k("exec", pod, "-c", "riak", "--", "sh", "-c",
            "VMARGS_PATH=$(ls -1 /var/lib/riak/generated.conf/vm.*.args | tail -1) riak-admin status")
    m = {}
    for line in out.splitlines():
        f = line.split(" : ", 1)
        if len(f) == 2:
            try:
                m["riak_" + f[0].strip()] = float(f[1].strip())
            except ValueError:
                pass
    return m


pods = [f"{a.cluster}-{i}" for i in range(3)]
meta = {"started": now(), "interval_s": a.interval}
json.dump(meta, open(os.path.join(a.out, "meta.json"), "w"))


def disk_of(pod):
    df = k("exec", pod, "-c", "riak", "--", "df", "-Pk", "/var/lib/riak", timeout=120).splitlines()
    if len(df) >= 2:
        c = df[1].split()
        return pod, int(c[2]) / 1024 / 1024, float(c[4].rstrip("%"))
    return pod, None, 0.0


def top_pods():
    mem = {}
    for line in k("top", "pod", "--containers", "--no-headers", timeout=120).splitlines():
        f = line.split()
        if len(f) >= 4 and f[1] == "riak" and f[0] in pods:
            mem[f[0]] = mem_bytes(f[3])
    return mem


def top_nodes():
    return [l for l in subprocess.run(["kubectl", "top", "nodes", "--no-headers"], capture_output=True, text=True,
                                      timeout=120).stdout.splitlines() if l.strip()]


# The API is slow from some machines (tens of seconds per call), so every call of a cycle runs at once.
t_end = time.time() + a.minutes * 60
n = 0
with open(os.path.join(a.out, "pods.jsonl"), "a") as fp, open(os.path.join(a.out, "metrics.jsonl"), "a") as fm, \
        open(os.path.join(a.out, "nodes.jsonl"), "a") as fn, ThreadPoolExecutor(max_workers=10) as ex:
    while time.time() < t_end:
        t0 = time.time()
        stamp = now()
        f_mem, f_nodes = ex.submit(top_pods), ex.submit(top_nodes)
        f_disk = [ex.submit(disk_of, p) for p in pods]
        f_stat = [(p, ex.submit(status, p)) for p in pods]
        mem = f_mem.result()
        disk, pct = {}, 0.0
        for fut in f_disk:
            pod, gib, pc = fut.result()
            if gib is not None:
                disk[pod] = gib
                pct = max(pct, pc)
        fp.write(json.dumps({"time": stamp, "pod_memory_bytes": mem, "pod_disk_used_gib": disk, "disk_pct": pct}) + "\n"); fp.flush()
        for pod, fut in f_stat:
            m = fut.result()
            if m:
                fm.write(json.dumps({"time": stamp, "pod": pod, "metrics": m}) + "\n")
        try:
            fn.write(json.dumps({"time": stamp, "top": f_nodes.result()}) + "\n")
        except Exception:
            pass
        fm.flush(); fn.flush()
        n += 1
        time.sleep(max(0, a.interval - (time.time() - t0)))
print("collected", n, "samples into", a.out)
