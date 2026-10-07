#!/usr/bin/env python3
"""Turn the artifacts of a soak run into SVG charts for the docs.

    hack/soak-report.py ARTIFACT_DIR OUT_DIR

ARTIFACT_DIR is what `test/scale -soak -soak-artifacts DIR` saved (samples.jsonl, metrics.jsonl,
nodes.jsonl, summary.json). For every chart it writes NAME-light.svg and NAME-dark.svg (the docs use
Material's `#only-light` / `#only-dark` image suffixes), and numbers.json with the statistics quoted in
the text. Standard library only, so it runs anywhere and the output is reproducible.

Charts:
  overview     a tile panel: throughput, errors, latency, resources, verdict
  throughput   achieved ops/s against the target
  latency      client-side p50/p99 of puts and gets over the run (log scale)
  histogram    distribution of the per-sample worst p99
  server       Riak's own p99 (fsm time) next to the clients' p99
  memory       working set of each Riak container
  cpu          total CPU use of each Kubernetes node
  disk         data volume use per node against the PVC size
  capacity     what a 1 GbE / 10 GbE link allows: ops/s against object size, with the runs measured
"""
import html
import json
import math
import os
import statistics
import sys
from datetime import datetime

W, H = 860, 330
ML, MR, MT, MB = 70, 24, 58, 50
FONT = "system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif"

PALETTES = {
    "light": {"fg": "#1f2328", "muted": "#656d76", "grid": "#d8dee4", "tile": "#f6f8fa", "edge": "#d0d7de",
              "series": ["#0969da", "#cf222e", "#1a7f37", "#bf8700", "#8250df", "#bc4c00"],
              "ok": "#1a7f37", "bad": "#cf222e"},
    "dark": {"fg": "#e6edf3", "muted": "#8b949e", "grid": "#30363d", "tile": "#161b22", "edge": "#30363d",
             "series": ["#58a6ff", "#ff7b72", "#3fb950", "#d29922", "#bc8cff", "#ffa657"],
             "ok": "#3fb950", "bad": "#ff7b72"},
}

# Runs shown on the capacity chart: (label, object KiB, ops/s, outcome). "ok" = the run held its rate,
# "ceiling" = the throughput the cluster topped out at, "target" = a target it could not reach.
RUNS = [
    ("16 KiB, 200 ops/s (4 h, held)", 16, 200, "ok"),
    ("128 KiB, 600 ops/s (4 h)", 128, 600, "ok"),
    ("500 KiB: ceiling ~150 ops/s", 500, 150, "ceiling"),
    ("500 KiB, 1000 ops/s target", 500, 1000, "target"),
]
# Measured on the test cluster: bytes through one node's NIC per operation, as a multiple of the object size
# (80 MB/s in at about 150 ops/s of 500 KiB is 1.04), and what a link of each speed really carries per
# direction after overlay overhead.
NIC_FACTOR = 1.04
LINKS = [("1 GbE", 90e6), ("10 GbE", 900e6)]


# ── helpers ────────────────────────────────────────────────────────────────────────────────────
def read_jsonl(path):
    out = []
    if not os.path.exists(path):
        return out
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                try:
                    out.append(json.loads(line))
                except ValueError:
                    pass
    return out


def pct(values, q):
    if not values:
        return 0.0
    s = sorted(values)
    return s[min(int(len(s) * q), len(s) - 1)]


def fmt(v, digits=1):
    if abs(v) >= 1000:
        return f"{v:,.0f}"
    if abs(v) >= 100 or v == int(v):
        return f"{v:.0f}"
    return f"{v:.{digits}f}"


def nice_ticks(lo, hi, n=5):
    if hi <= lo:
        hi = lo + 1
    step = (hi - lo) / n
    mag = 10 ** math.floor(math.log10(step))
    for m in (1, 2, 2.5, 5, 10):
        if step <= m * mag:
            step = m * mag
            break
    start = math.floor(lo / step) * step
    ticks, v = [], start
    while v <= hi + step * 0.001:
        if v >= lo - step * 0.001:
            ticks.append(round(v, 10))
        v += step
    return ticks


def log_ticks(lo, hi):
    ticks, e = [], math.floor(math.log10(lo))
    while 10 ** e <= hi * 1.0001:
        for m in (1, 2, 5):
            v = m * 10 ** e
            if lo * 0.9999 <= v <= hi * 1.0001:
                ticks.append(v)
        e += 1
    return ticks


def parse_time(t):
    """RFC3339 with nanoseconds (what Go writes) -> epoch seconds."""
    t = t.replace("Z", "+00:00")
    if "." in t:
        head, rest = t.split(".", 1)
        tz = ""
        for sign in ("+", "-"):
            if sign in rest:
                tz = sign + rest.split(sign, 1)[1]
                break
        t = head + tz
    return datetime.fromisoformat(t).timestamp()


class Chart:
    def __init__(self, theme, title, subtitle="", xlabel="", ylabel="", xlog=False, ylog=False, w=W, h=H):
        self.p, self.w, self.h = PALETTES[theme], w, h
        self.title, self.subtitle, self.xlabel, self.ylabel = title, subtitle, xlabel, ylabel
        self.xlog, self.ylog = xlog, ylog
        self.parts, self.legend = [], []
        self.x0, self.x1, self.y0, self.y1 = 0.0, 1.0, 0.0, 1.0

    def _m(self, v, lo, hi, log):
        if log:
            v, lo, hi = math.log10(max(v, 1e-12)), math.log10(lo), math.log10(hi)
        return (v - lo) / (hi - lo) if hi != lo else 0.0

    def px(self, x):
        return ML + self._m(x, self.x0, self.x1, self.xlog) * (self.w - ML - MR)

    def py(self, y):
        return self.h - MB - self._m(y, self.y0, self.y1, self.ylog) * (self.h - MT - MB)

    def axes(self, x0, x1, y0, y1, xticks=None, yticks=None, xfmt=fmt, yfmt=fmt):
        self.x0, self.x1, self.y0, self.y1 = x0, x1, y0, y1
        p = self.p
        xticks = xticks or (log_ticks(x0, x1) if self.xlog else nice_ticks(x0, x1, 8))
        yticks = yticks or (log_ticks(y0, y1) if self.ylog else nice_ticks(y0, y1, 5))
        g = []
        for t in yticks:
            y = self.py(t)
            g.append(f'<line x1="{ML}" x2="{self.w - MR}" y1="{y:.1f}" y2="{y:.1f}" stroke="{p["grid"]}" stroke-width="1"/>')
            g.append(f'<text x="{ML - 8}" y="{y + 4:.1f}" text-anchor="end" fill="{p["muted"]}" font-size="11">{html.escape(yfmt(t))}</text>')
        for t in xticks:
            x = self.px(t)
            g.append(f'<line x1="{x:.1f}" x2="{x:.1f}" y1="{MT}" y2="{self.h - MB}" stroke="{p["grid"]}" stroke-width="1" stroke-dasharray="2 4"/>')
            g.append(f'<text x="{x:.1f}" y="{self.h - MB + 16}" text-anchor="middle" fill="{p["muted"]}" font-size="11">{html.escape(xfmt(t))}</text>')
        g.append(f'<line x1="{ML}" x2="{self.w - MR}" y1="{self.h - MB}" y2="{self.h - MB}" stroke="{p["muted"]}"/>')
        self.parts = g + self.parts

    def line(self, pts, color, width=1.6, dash=None, label=None):
        pts = [(x, y) for x, y in pts if y is not None and (not self.ylog or y > 0)]
        if len(pts) < 2:
            return
        d = " ".join(f"{self.px(x):.1f},{self.py(y):.1f}" for x, y in pts)
        da = f' stroke-dasharray="{dash}"' if dash else ""
        self.parts.append(f'<polyline points="{d}" fill="none" stroke="{color}" stroke-width="{width}"{da} stroke-linejoin="round" stroke-linecap="round"/>')
        if label:
            self.legend.append((label, color, dash))

    def hline(self, y, color, label, dash="6 4"):
        yy = self.py(y)
        self.parts.append(f'<line x1="{ML}" x2="{self.w - MR}" y1="{yy:.1f}" y2="{yy:.1f}" stroke="{color}" stroke-width="1.4" stroke-dasharray="{dash}"/>')
        self.legend.append((label, color, dash))

    def rect(self, x, y, w, h, color, opacity=0.85):
        self.parts.append(f'<rect x="{x:.1f}" y="{y:.1f}" width="{max(w, 0.5):.1f}" height="{max(h, 0):.1f}" fill="{color}" opacity="{opacity}"/>')

    def vline(self, x, color, label):
        xx = self.px(x)
        self.parts.append(f'<line x1="{xx:.1f}" x2="{xx:.1f}" y1="{MT}" y2="{self.h - MB}" stroke="{color}" stroke-width="1.4" stroke-dasharray="5 4"/>')
        self.legend.append((label, color, "5 4"))

    def dot(self, x, y, color, label=None, r=6, ring=False, where="right"):
        fill = "none" if ring else color
        self.parts.append(f'<circle cx="{self.px(x):.1f}" cy="{self.py(y):.1f}" r="{r}" fill="{fill}" stroke="{color}" stroke-width="2"/>')
        if label and where == "above":
            self.parts.append(f'<text x="{self.px(x):.1f}" y="{self.py(y) - r - 6:.1f}" text-anchor="middle" fill="{self.p["fg"]}" font-size="11">{html.escape(label)}</text>')
        elif label:
            self.parts.append(f'<text x="{self.px(x) + r + 5:.1f}" y="{self.py(y) + 4:.1f}" fill="{self.p["fg"]}" font-size="11">{html.escape(label)}</text>')

    def svg(self):
        p = self.p
        head = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {self.w} {self.h}" width="{self.w}" height="{self.h}" '
                f'font-family="{FONT}" role="img" aria-label="{html.escape(self.title)}">',
                f'<title>{html.escape(self.title)}</title>',
                f'<text x="{ML}" y="22" fill="{p["fg"]}" font-size="15" font-weight="600">{html.escape(self.title)}</text>']
        if self.subtitle:
            head.append(f'<text x="{ML}" y="39" fill="{p["muted"]}" font-size="11.5">{html.escape(self.subtitle)}</text>')
        # legend: a row under the subtitle, wrapping to a second row when it is long
        x, y = ML, 52 if self.subtitle else 40
        for label, color, dash in self.legend:
            wlab = 6.6 * len(label) + 34
            if x + wlab > self.w - MR:
                x, y = ML, y + 14
            da = f' stroke-dasharray="{dash}"' if dash else ""
            head.append(f'<line x1="{x:.0f}" x2="{x + 18:.0f}" y1="{y - 4}" y2="{y - 4}" stroke="{color}" stroke-width="2.4"{da}/>')
            head.append(f'<text x="{x + 23:.0f}" y="{y}" fill="{p["muted"]}" font-size="11">{html.escape(label)}</text>')
            x += wlab
        if self.xlabel:
            head.append(f'<text x="{(ML + self.w - MR) / 2:.0f}" y="{self.h - 8}" text-anchor="middle" fill="{p["muted"]}" font-size="11.5">{html.escape(self.xlabel)}</text>')
        if self.ylabel:
            head.append(f'<text transform="translate(16 {(MT + self.h - MB) / 2:.0f}) rotate(-90)" text-anchor="middle" fill="{p["muted"]}" font-size="11.5">{html.escape(self.ylabel)}</text>')
        return "\n".join(head + self.parts + ["</svg>\n"])


# ── data ───────────────────────────────────────────────────────────────────────────────────────
def load(art):
    samples = read_jsonl(os.path.join(art, "samples.jsonl"))
    metrics = read_jsonl(os.path.join(art, "metrics.jsonl"))
    nodes = read_jsonl(os.path.join(art, "nodes.jsonl"))
    summary = {}
    p = os.path.join(art, "summary.json")
    if os.path.exists(p):
        with open(p, encoding="utf-8") as f:
            summary = json.load(f)
    return samples, metrics, nodes, summary


def hours(row):
    return row.get("elapsed_s", 0) / 3600.0


def client_stat(row, op, key, agg):
    vals = [c.get(op, {}).get(key, 0) for c in (row.get("clients") or {}).values()]
    vals = [v for v in vals if v and v > 0]
    if not vals:
        return None
    return max(vals) if agg == "max" else statistics.median(vals)


def series_clients(samples, op, key, agg):
    return [(hours(r), client_stat(r, op, key, agg)) for r in samples]


def server_series(metrics, name, scale=1.0):
    """Per timestamp, the worst node's value of a riak_* series (zeros dropped): [(time string, value)]."""
    by_t = {}
    for m in metrics:
        v = m.get("metrics", {}).get(name)
        if v:
            by_t.setdefault(m["time"], []).append(v * scale)
    return sorted((t, max(v)) for t, v in by_t.items())


def to_hours(series, t0):
    base = parse_time(t0)
    return [((parse_time(t) - base) / 3600.0, v) for t, v in series]


def node_cpu(nodes, t0):
    per = {}
    for row in nodes:
        for line in row.get("top", []):
            f = line.split()
            if len(f) >= 3 and f[2].endswith("%"):
                per.setdefault(f[0].split(".")[0], []).append((row["time"], float(f[2][:-1])))
    return {n: to_hours(v, t0) for n, v in per.items()}


def worst_p99(r):
    a, b = client_stat(r, "Put", "p99", "max"), client_stat(r, "Get", "p99", "max")
    vals = [x for x in (a, b) if x]
    return max(vals) if vals else None


# ── charts ─────────────────────────────────────────────────────────────────────────────────────
def timed_phase(samples):
    """The samples taken while the clients were loading, without the ramp-up and the tail.

    Samples at the very start and end whose rate is under half the run's median are the clients
    still connecting and the load winding down; they would show as artifacts at the edges. Only the
    edges are trimmed: a dip in the middle of the run is real and stays."""
    live = [r for r in samples if r["sample"].get("Rate", 0) > 0]
    if len(live) < 5:
        return live
    floor = 0.5 * statistics.median(r["sample"]["Rate"] for r in live)
    lo, hi = 0, len(live)
    while lo < hi and live[lo]["sample"]["Rate"] < floor:
        lo += 1
    while hi > lo and live[hi - 1]["sample"]["Rate"] < floor:
        hi -= 1
    return live[lo:hi]


def chart_throughput(samples, target):
    pts = [(hours(r), r["sample"].get("Rate")) for r in timed_phase(samples)]

    def make(theme):
        c = Chart(theme, "Throughput", f"all clients together; the target is {fmt(target)} ops/s", "hours into the run", "operations per second")
        top = max(target * 1.2, max((y for _, y in pts), default=0) * 1.05)
        c.axes(0, max((x for x, _ in pts), default=1), 0, top)
        c.hline(target, c.p["muted"], "target")
        c.line(pts, c.p["series"][0], 1.8, label="achieved")
        return c
    return make


def chart_latency(samples):
    series = [("put p99 (worst client)", series_clients(samples, "Put", "p99", "max"), 1),
              ("get p99 (worst client)", series_clients(samples, "Get", "p99", "max"), 0),
              ("put p50 (median client)", series_clients(samples, "Put", "p50", "med"), 3),
              ("get p50 (median client)", series_clients(samples, "Get", "p50", "med"), 2)]
    allv = [y for _, s, _ in series for _, y in s if y]

    def make(theme):
        c = Chart(theme, "Latency seen by the clients", "one point per 30 s sample; log scale", "hours into the run", "milliseconds", ylog=True)
        lo = 10 ** math.floor(math.log10(max(min(allv, default=1), 0.5)))
        hi = 10 ** math.ceil(math.log10(max(allv, default=10)))
        c.axes(0, max((x for _, s, _ in series for x, _ in s), default=1), lo, max(hi, lo * 10))
        for label, s, color in series:
            c.line(s, c.p["series"][color], 1.4, "3 3" if "p50" in label else None, label)
        return c
    return make


def chart_histogram(samples):
    vals = [v for v in (worst_p99(r) for r in samples) if v]
    nice = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 20000, 50000]
    lo = max([b for b in nice if b <= min(vals, default=10) / 1.4] or [1])   # a nice bound just under the data
    hi = min([b for b in nice if b >= max(vals, default=100) * 1.4] or [nice[-1]])
    n = 28
    edges = [lo * (hi / lo) ** (i / n) for i in range(n + 1)]
    counts = [0] * n
    for v in vals:
        i = min(max(int(math.log(max(v, lo) / lo) / math.log(hi / lo) * n), 0), n - 1)
        counts[i] += 1

    def make(theme):
        c = Chart(theme, "Where the worst p99 sits", f"{len(vals)} samples; in each, the worst client's p99 (put or get)", "milliseconds (log scale)", "samples", xlog=True)
        c.axes(lo, hi, 0, max(counts, default=1) * 1.15)
        for i, k in enumerate(counts):
            x0, x1 = c.px(edges[i]), c.px(edges[i + 1])
            c.rect(x0 + 0.5, c.py(k), x1 - x0 - 1, c.py(0) - c.py(k), c.p["series"][0])
        for q, color, label in ((0.5, 2, "median"), (0.95, 3, "p95"), (0.99, 1, "p99")):
            if vals:
                c.vline(pct(vals, q), c.p["series"][color], f"{label} {fmt(pct(vals, q))} ms")
        return c
    return make


def chart_server(samples, metrics, t0):
    srv_put = to_hours(server_series(metrics, "riak_node_put_fsm_time_99", 1e-3), t0)
    srv_get = to_hours(server_series(metrics, "riak_node_get_fsm_time_99", 1e-3), t0)
    cl_put, cl_get = series_clients(samples, "Put", "p99", "max"), series_clients(samples, "Get", "p99", "max")
    allv = [y for s in (srv_put, srv_get, cl_put, cl_get) for _, y in s if y]

    def make(theme):
        c = Chart(theme, "Riak's own latency vs what the clients see", "p99: Riak's own measurement (worst node) next to the clients'; they track each other", "hours into the run", "milliseconds", ylog=True)
        lo = 10 ** math.floor(math.log10(max(min(allv, default=1), 0.1)))
        hi = max(10 ** math.ceil(math.log10(max(allv, default=10))), lo * 10)
        c.axes(0, max((x for s in (srv_put, srv_get, cl_put, cl_get) for x, _ in s), default=1), lo, hi)
        c.line(cl_put, c.p["series"][1], 1.5, label="put, client")
        c.line(srv_put, c.p["series"][3], 1.5, "3 3", "put, Riak (worst node)")
        c.line(cl_get, c.p["series"][0], 1.5, label="get, client")
        c.line(srv_get, c.p["series"][2], 1.5, "3 3", "get, Riak (worst node)")
        return c
    return make


def chart_memory(samples, mem_limit_gib):
    pods = {}
    for r in samples:
        for pod, b in (r.get("pod_memory_bytes") or {}).items():
            pods.setdefault(pod, []).append((hours(r), b / 2**30))

    def make(theme):
        c = Chart(theme, "Riak memory", f"working set of each Riak container; the limit is {fmt(mem_limit_gib)} GiB", "hours into the run", "GiB")
        top = max([y for s in pods.values() for _, y in s] + [0.5])
        c.axes(0, max([x for s in pods.values() for x, _ in s] + [1]), 0, math.ceil(top * 1.3 * 2) / 2)
        for i, (pod, s) in enumerate(sorted(pods.items())):
            c.line(s, c.p["series"][i % 3], 1.7, label=pod)
        return c
    return make


def chart_cpu(nodes, t0):
    cpu = node_cpu(nodes, t0)

    def make(theme):
        c = Chart(theme, "Node CPU", "each Kubernetes node's total CPU use (Riak shares the nodes with the platform)", "hours into the run", "% of the node")
        c.axes(0, max([x for s in cpu.values() for x, _ in s] + [1]), 0, 100, yticks=[0, 20, 40, 60, 80, 100])
        for i, (n, s) in enumerate(sorted(cpu.items())):
            c.line(s, c.p["series"][i % 3], 1.5, label=n)
        return c
    return make


def chart_disk(samples, pvc_gib):
    disks = {}
    for r in samples:
        for pod, g in (r.get("pod_disk_used_gib") or {}).items():
            disks.setdefault(pod, []).append((hours(r), g))

    def make(theme):
        c = Chart(theme, "Data volume use", f"bitcask keeps overwritten data until it merges; the {fmt(pvc_gib)} GiB volume is never close to full", "hours into the run", "GiB used")
        top = max([y for s in disks.values() for _, y in s] + [1])
        c.axes(0, max([x for s in disks.values() for x, _ in s] + [1]), 0, max(top * 1.25, 10))
        for i, (pod, s) in enumerate(sorted(disks.items())):
            c.line(s, c.p["series"][i % 3], 1.7, label=pod)
        return c
    return make


def chart_capacity():
    def make(theme):
        c = Chart(theme, "What the network allows", "ops/s one node's link can carry, by object size (about 1.04 x the object size crosses each NIC per operation)", "object size in KiB (log)", "operations per second (log)", xlog=True, ylog=True)
        c.axes(4, 2048, 20, 20000)
        sizes = [4 * (2048 / 4) ** (i / 60) for i in range(61)]
        for i, (name, bytes_per_s) in enumerate(LINKS):
            c.line([(s, bytes_per_s / (NIC_FACTOR * s * 1024)) for s in sizes], c.p["series"][(1, 4)[i]], 2.2,
                   None if i == 0 else "6 4", f"{name} (about {fmt(bytes_per_s / 1e6)} MB/s)")
        for label, kib, ops, outcome in RUNS:
            color = {"ok": c.p["ok"], "ceiling": c.p["series"][3], "target": c.p["bad"]}[outcome]
            c.dot(kib, ops, color, label, ring=(outcome == "target"), where="right" if kib < 100 or outcome == "target" else "above")
        return c
    return make


def chart_overview(numbers, ok):
    def make(theme):
        c = Chart(theme, "Soak run at a glance", numbers["subtitle"], w=W, h=224)
        p = c.p
        tiles = [("throughput", f'{numbers["tput_avg"]:.0f} ops/s', f'{numbers["tput_pct"]:.0f}% of target on average'),
                 ("errors", f'{numbers["error_rate"]:.3f}%', f'{numbers["errors"]} failed of {numbers["ops"]:,} operations'),
                 ("p99 latency", f'{numbers["p99_median"]:.0f} ms', f'median sample; worst {numbers["p99_worst"]:.0f} ms'),
                 ("integrity", numbers["integrity"], numbers["integrity_sub"]),
                 ("Riak memory", f'{numbers["mem_peak_gib"]:.1f} GiB', f'peak; limit {numbers["mem_limit_gib"]:.0f} GiB'),
                 ("disk", f'{numbers["disk_peak_pct"]:.0f}%', f'peak of the {numbers["pvc_gib"]:.0f} GiB volume'),
                 ("OOM kills", str(numbers["oom"]), f'{numbers["restarts"]} container restarts'),
                 ("verdict", "PASS" if ok else "FAIL", f'{numbers["scaling_actions"]} scaling actions')]
        cols, gap, th = 4, 12, 76
        tw = (W - 32 - gap * (cols - 1)) / cols
        for i, (label, big, small) in enumerate(tiles):
            x, y = 16 + (i % cols) * (tw + gap), 52 + (i // cols) * (th + gap)
            color = p["fg"]
            if label == "verdict":
                color = p["ok"] if ok else p["bad"]
            c.parts.append(f'<rect x="{x:.0f}" y="{y}" width="{tw:.0f}" height="{th}" rx="8" fill="{p["tile"]}" stroke="{p["edge"]}"/>')
            c.parts.append(f'<text x="{x + 12:.0f}" y="{y + 20}" fill="{p["muted"]}" font-size="11.5">{html.escape(label)}</text>')
            c.parts.append(f'<text x="{x + 12:.0f}" y="{y + 46}" fill="{color}" font-size="22" font-weight="700">{html.escape(big)}</text>')
            c.parts.append(f'<text x="{x + 12:.0f}" y="{y + 64}" fill="{p["muted"]}" font-size="10.5">{html.escape(small[:34])}</text>')
        return c
    return make


# ── main ───────────────────────────────────────────────────────────────────────────────────────
def quantity_gib(q, default):
    q = q or ""
    for suffix, div in (("Gi", 1), ("Mi", 1024), ("Ti", 1 / 1024)):
        if q.endswith(suffix):
            try:
                return float(q[:-2]) / div
            except ValueError:
                pass
    return default


def build(art, out):
    os.makedirs(out, exist_ok=True)
    samples, metrics, nodes, summary = load(art)
    if not samples:
        sys.exit(f"no samples.jsonl in {art}")
    cfg = summary.get("config", {})
    t0 = samples[0]["time"]
    live = timed_phase(samples) or samples  # the timed phase only
    rates = [r["sample"].get("Rate", 0) for r in live]
    target = cfg.get("rate_ops_per_s") or max(rates)
    p99s = [v for v in (worst_p99(r) for r in live) if v]
    res = (summary.get("results") or {}).get("Total") or {}
    ops = res.get("ops") or (res.get("puts", 0) + res.get("gets", 0))
    errors = res.get("errors", 0)
    mem_limit_gib = quantity_gib(cfg.get("memory"), 16.0)
    pvc_gib = quantity_gib(cfg.get("storage"), 300.0)
    mem_peak = max([b for r in samples for b in (r.get("pod_memory_bytes") or {}).values()] + [0]) / 2**30
    disk_peak = max([r["sample"].get("DiskPct", 0) for r in samples] + [0])
    bad = sum(res.get(k, 0) for k in ("lost", "final_lost", "corrupt", "final_corrupt", "stale", "ahead"))
    numbers = {
        "samples": len(samples), "duration_h": round(samples[-1]["elapsed_s"] / 3600, 2), "target_ops": target,
        "tput_avg": statistics.mean(rates), "tput_min": min(rates), "tput_pct": statistics.mean(rates) / target * 100,
        "share_samples_ge_95pct": round(sum(1 for r in rates if r >= 0.95 * target) / len(rates) * 100, 1),
        "p99_median": statistics.median(p99s) if p99s else 0, "p99_p95": pct(p99s, 0.95), "p99_p99": pct(p99s, 0.99),
        "p99_worst": max(p99s) if p99s else 0, "ops": ops, "errors": errors,
        "error_rate": (errors / (ops + errors) * 100) if ops + errors else 0.0,
        "integrity": "clean" if not bad else f"{bad} bad",
        "integrity_sub": f'{res.get("verified", 0):,} keys read back', "landed": res.get("landed", 0),
        "mem_peak_gib": mem_peak, "mem_limit_gib": mem_limit_gib, "disk_peak_pct": disk_peak, "pvc_gib": pvc_gib,
        "oom": summary.get("oom_kills", 0), "restarts": summary.get("container_restarts", 0),
        "scaling_actions": len([t for t in summary.get("timeline", []) if "scaling:" in t]),
        "subtitle": f'{cfg.get("rate_ops_per_s", "?")} ops/s, {int(cfg.get("value_size_bytes", 0)) // 1024} KiB objects, {cfg.get("users", "?")} users, '
                    f'{cfg.get("buckets", "?")} buckets, n_val {cfg.get("n_val", "?")}, pr/pw {cfg.get("pr", "?")}/{cfg.get("pw", "?")}, {cfg.get("replicas", "?")} nodes',
    }
    ok = bool(summary.get("passed", False))
    charts = {
        "overview": chart_overview(numbers, ok), "throughput": chart_throughput(samples, target),
        "latency": chart_latency(samples), "histogram": chart_histogram(samples),
        "server": chart_server(samples, metrics, t0), "memory": chart_memory(samples, mem_limit_gib),
        "cpu": chart_cpu(nodes, t0), "disk": chart_disk(samples, pvc_gib), "capacity": chart_capacity(),
    }
    for name, make in charts.items():
        for theme in ("light", "dark"):
            with open(os.path.join(out, f"{name}-{theme}.svg"), "w", encoding="utf-8") as f:
                f.write(make(theme).svg())
    with open(os.path.join(out, "numbers.json"), "w", encoding="utf-8") as f:
        json.dump(numbers, f, indent=2, default=float)
        f.write("\n")
    return numbers


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    print(json.dumps(build(sys.argv[1], sys.argv[2]), indent=2, default=float))
