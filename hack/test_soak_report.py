"""Offline tests for soak-report.py (no cluster, no Riak needed).

    python3 -m unittest hack/test_soak_report.py
"""
import importlib.util
import json
import os
import tempfile
import unittest
import xml.dom.minidom
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("soak_report", os.path.join(HERE, "soak-report.py"))
sr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sr)

START = datetime(2026, 10, 7, 10, 50, 0, tzinfo=timezone.utc)


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def stamp(seconds):
    """A timestamp the way Go's encoding/json writes a time.Time: RFC3339 with nanoseconds."""
    t = START + timedelta(seconds=seconds)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + "%09d" % (t.microsecond * 1000) + "Z"


def client(rate=60.0, put99=400.0, get99=300.0):
    return {"Pod": "c", "T": 0, "Ops": 1800, "OpsPerS": rate, "Errors": 0,
            "Put": {"p50": 40.0, "p95": 200.0, "p99": put99, "max": 900.0},
            "Get": {"p50": 30.0, "p95": 150.0, "p99": get99, "max": 700.0}}


def write_run(art, n=40, target=600, with_summary=True):
    with open(os.path.join(art, "samples.jsonl"), "w") as f:
        for i in range(n):
            t = i * 30
            f.write(json.dumps({
                "time": stamp(t), "elapsed_s": float(t), "nodes": 3, "memory_limit": "16Gi",
                "sample": {"Rate": float(target) if i else 0.0, "ErrRate": 0.0, "P99": 400.0, "MemPct": 8.0,
                           "DiskPct": 20.0 + i * 0.5, "OOMKills": 0, "Restarts": 0},
                "pod_memory_bytes": {"soak-0": (1 + i * 0.01) * 2**30, "soak-1": 1.1 * 2**30, "soak-2": 1.2 * 2**30},
                "pod_disk_used_gib": {"soak-0": 60.0 + i, "soak-1": 61.0 + i, "soak-2": 62.0 + i},
                "clients": {f"soak-client-{k:02d}": client(put99=300 + 10 * (i % 7) + k) for k in range(10)},
            }) + "\n")
    with open(os.path.join(art, "metrics.jsonl"), "w") as f:
        for i in range(0, n, 2):
            for pod in ("soak-0", "soak-1", "soak-2"):
                f.write(json.dumps({"time": stamp(i * 30), "pod": pod, "metrics": {
                    "riak_node_put_fsm_time_99": 250000.0 + i * 1000, "riak_node_get_fsm_time_99": 180000.0,
                    "riak_node_gets_total": 1000.0 * i}}) + "\n")
    with open(os.path.join(art, "nodes.jsonl"), "w") as f:
        for i in range(0, n, 2):
            f.write(json.dumps({"time": stamp(i * 30), "top": [
                "sm1.okd.marthy.xyz 4613m 40% 36966Mi 28%", "sm2.okd.marthy.xyz 4886m 55% 31687Mi 24%",
                "sm3.okd.marthy.xyz 3579m 31% 17261Mi 13%"]}) + "\n")
    if with_summary:
        with open(os.path.join(art, "summary.json"), "w") as f:
            json.dump({"passed": True, "oom_kills": 0, "container_restarts": 0, "timeline": [],
                       "config": {"rate_ops_per_s": target, "value_size_bytes": 131072, "users": 10, "buckets": 10,
                                  "n_val": 3, "pr": 2, "pw": 2, "replicas": 3, "memory": "16Gi", "storage": "300Gi"},
                       "results": {"Total": {"ops": 8_000_000, "puts": 4_000_000, "gets": 4_000_000, "errors": 12,
                                             "lost": 0, "corrupt": 0, "verified": 480000, "landed": 3}}}, f)


class Helpers(unittest.TestCase):
    def test_parse_time_accepts_what_go_writes(self):
        base = sr.parse_time("2026-10-07T10:50:00.123456789Z")  # sub-second precision is dropped on purpose
        self.assertEqual(sr.parse_time("2026-10-07T10:51:00Z") - base, 60.0)
        self.assertEqual(sr.parse_time("2026-10-07T12:50:00+02:00"), sr.parse_time("2026-10-07T10:50:00Z"))
        self.assertEqual(sr.parse_time("2026-10-07T08:50:00.5-02:00"), sr.parse_time("2026-10-07T10:50:00Z"))

    def test_ticks(self):
        t = sr.nice_ticks(0, 700, 5)
        self.assertEqual(t[0], 0)
        self.assertGreaterEqual(t[-1], 600)
        self.assertEqual(sr.log_ticks(10, 1000), [10, 20, 50, 100, 200, 500, 1000])

    def test_percentile_and_format(self):
        self.assertEqual(sr.pct(list(range(1, 101)), 0.5), 51)
        self.assertEqual(sr.pct([], 0.99), 0.0)
        self.assertEqual(sr.fmt(1234.6), "1,235")
        self.assertEqual(sr.fmt(600), "600")
        self.assertEqual(sr.fmt(0.3), "0.3")
        self.assertEqual(sr.fmt(12.34), "12.3")  # one decimal below 100

    def test_quantity(self):
        self.assertEqual(sr.quantity_gib("16Gi", 1), 16.0)
        self.assertEqual(sr.quantity_gib("512Mi", 1), 0.5)
        self.assertEqual(sr.quantity_gib("", 7.0), 7.0)
        self.assertEqual(sr.quantity_gib("lots", 7.0), 7.0)


class Build(unittest.TestCase):
    def test_renders_every_chart_in_both_themes_as_valid_svg(self):
        with tempfile.TemporaryDirectory() as art, tempfile.TemporaryDirectory() as out:
            write_run(art)
            numbers = sr.build(art, out)
            names = ["overview", "throughput", "latency", "histogram", "server", "memory", "cpu", "disk", "capacity"]
            for n in names:
                for theme in ("light", "dark"):
                    path = os.path.join(out, f"{n}-{theme}.svg")
                    self.assertTrue(os.path.exists(path), path)
                    doc = xml.dom.minidom.parse(path)  # well-formed
                    self.assertEqual(doc.documentElement.tagName, "svg")
                    self.assertIn("<title>", read(path))
            self.assertTrue(os.path.exists(os.path.join(out, "numbers.json")))
            light = read(os.path.join(out, "throughput-light.svg"))
            dark = read(os.path.join(out, "throughput-dark.svg"))
            self.assertNotEqual(light, dark, "the two themes must differ")
            self.assertIn("#1f2328", light)
            self.assertIn("#e6edf3", dark)
            self.assertEqual(numbers["target_ops"], 600)

    def test_numbers(self):
        with tempfile.TemporaryDirectory() as art, tempfile.TemporaryDirectory() as out:
            write_run(art, n=40, target=600)
            n = sr.build(art, out)
            self.assertEqual(n["samples"], 40)
            self.assertAlmostEqual(n["tput_avg"], 600.0)
            self.assertAlmostEqual(n["tput_pct"], 100.0)
            self.assertEqual(n["share_samples_ge_95pct"], 100.0)
            self.assertEqual(n["ops"], 8_000_000)
            self.assertEqual(n["errors"], 12)
            self.assertAlmostEqual(n["error_rate"], 12 / 8_000_012 * 100, places=6)
            self.assertEqual(n["integrity"], "clean")
            self.assertEqual(n["oom"], 0)
            self.assertEqual(n["mem_limit_gib"], 16.0)
            self.assertEqual(n["pvc_gib"], 300.0)
            self.assertGreater(n["p99_median"], 300)
            self.assertIn("600 ops/s, 128 KiB objects, 10 users", n["subtitle"])
            self.assertEqual(json.loads(read(os.path.join(out, "numbers.json")))["samples"], 40)

    def test_ramp_up_and_tail_windows_are_left_out_of_the_statistics(self):
        with tempfile.TemporaryDirectory() as art, tempfile.TemporaryDirectory() as out:
            write_run(art, n=40, target=600)
            rows = [json.loads(line) for line in read(os.path.join(art, "samples.jsonl")).splitlines()]
            rows[1]["sample"]["Rate"] = 60.0    # the first window: clients still connecting
            rows[-2]["sample"]["Rate"] = 8.0    # the load winding down: two near-idle windows
            rows[-1]["sample"]["Rate"] = 10.0
            rows[20]["sample"]["Rate"] = 120.0  # a real stall in the middle must stay visible
            with open(os.path.join(art, "samples.jsonl"), "w", encoding="utf-8") as f:
                f.write("".join(json.dumps(r) + "\n" for r in rows))
            n = sr.build(art, out)
            self.assertEqual(n["tput_min"], 120.0)
            self.assertEqual(len(sr.timed_phase(rows)), 36)
            self.assertEqual(len(sr.timed_phase(rows[:3])), 2, "too few samples to trim anything")

    def test_bad_data_shows_in_the_verdict_tile(self):
        with tempfile.TemporaryDirectory() as art, tempfile.TemporaryDirectory() as out:
            write_run(art)
            summary = json.loads(read(os.path.join(art, "summary.json")))
            summary["passed"] = False
            summary["results"]["Total"]["stale"] = 2
            with open(os.path.join(art, "summary.json"), "w", encoding="utf-8") as f:
                json.dump(summary, f)
            n = sr.build(art, out)
            self.assertEqual(n["integrity"], "2 bad")
            self.assertIn("FAIL", read(os.path.join(out, "overview-light.svg")))

    def test_works_on_a_run_still_in_progress(self):
        """No summary.json, no metrics, no node usage yet: the charts that can be drawn are drawn."""
        with tempfile.TemporaryDirectory() as art, tempfile.TemporaryDirectory() as out:
            write_run(art, with_summary=False)
            os.remove(os.path.join(art, "metrics.jsonl"))
            os.remove(os.path.join(art, "nodes.jsonl"))
            n = sr.build(art, out)
            self.assertEqual(n["samples"], 40)
            xml.dom.minidom.parse(os.path.join(out, "server-light.svg"))
            xml.dom.minidom.parse(os.path.join(out, "cpu-dark.svg"))

    def test_an_empty_directory_is_an_error(self):
        with tempfile.TemporaryDirectory() as art, tempfile.TemporaryDirectory() as out:
            with self.assertRaises(SystemExit):
                sr.build(art, out)

    def test_capacity_chart_marks_every_run_and_both_links(self):
        svg = sr.chart_capacity()("light").svg()
        for label, _, _, _ in sr.RUNS:
            self.assertIn(label, svg)
        for name, _ in sr.LINKS:
            self.assertIn(name, svg)
        self.assertEqual(svg.count("<circle"), len(sr.RUNS))

    def test_the_network_model_matches_what_was_measured(self):
        """500 KiB objects at the measured ~150 ops/s must sit on the 1 GbE budget (within 15%)."""
        budget = dict(sr.LINKS)["1 GbE"]
        limit = budget / (sr.NIC_FACTOR * 500 * 1024)
        self.assertAlmostEqual(limit, 169, delta=15)
        self.assertGreater(budget / (sr.NIC_FACTOR * 500 * 1024) * 10, 1000, "10 GbE carries 500 KiB at 1000 ops/s")


if __name__ == "__main__":
    unittest.main()
