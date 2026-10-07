"""Offline tests for riak_stress.py's protocol helpers (no Riak needed).

    python3 -m unittest examples/stressapp/test_riak_stress.py
"""
import argparse
import importlib.util
import os
import threading
import time
import unittest

spec = importlib.util.spec_from_file_location(
    "riak_stress", os.path.join(os.path.dirname(os.path.abspath(__file__)), "riak_stress.py"))
rs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rs)


class ProtocolHelpers(unittest.TestCase):
    def test_varint_round_trip(self):
        for n in (0, 1, 127, 128, 300, 2 ** 31):
            got, i = rs._read_varint(rs._varint(n), 0)
            self.assertEqual(got, n)

    def test_put_req_fields(self):
        msg = rs.put_req(b"type", b"bucket", b"key", b"value", b"vc")
        fields = {num: val for num, wire, val in rs._parse_fields(msg)}
        self.assertEqual(fields[1], b"bucket")
        self.assertEqual(fields[2], b"key")
        self.assertEqual(fields[3], b"vc")
        self.assertEqual(fields[16], b"type")
        self.assertEqual(fields[11], 1)  # return_head
        content = {num: val for num, wire, val in rs._parse_fields(fields[4])}
        self.assertEqual(content[1], b"value")

    def test_put_req_without_vclock_omits_it(self):
        msg = rs.put_req(b"t", b"b", b"k", b"v", None)
        self.assertNotIn(3, [num for num, _, _ in rs._parse_fields(msg)])

    def test_parse_get_found_and_not_found(self):
        content = rs._field(1, b"v1") + rs._field(2, b"x")
        body = rs._field(1, content) + rs._field(2, b"clock")
        self.assertEqual(rs.parse_get(body), ([b"v1"], b"clock"))
        self.assertEqual(rs.parse_get(b""), ([], None))

    def test_parse_get_siblings(self):
        body = rs._field(1, rs._field(1, b"a")) + rs._field(1, rs._field(1, b"b"))
        self.assertEqual(rs.parse_get(body)[0], [b"a", b"b"])

    def test_values_are_deterministic_and_distinct(self):
        a = rs.make_value("c0-", 1, 5, 1, 256)
        self.assertEqual(a, rs.make_value("c0-", 1, 5, 1, 256))
        self.assertEqual(len(a), 256)
        self.assertNotEqual(a, rs.make_value("c0-", 1, 5, 2, 256))  # new version
        self.assertNotEqual(a, rs.make_value("c0-", 2, 5, 1, 256))  # other thread
        self.assertNotEqual(a, rs.make_value("c1-", 1, 5, 1, 256))  # other client

    def test_percentiles(self):
        p = rs.percentiles(list(range(1, 101)))
        self.assertEqual(p["p50"], 51)
        self.assertEqual(p["max"], 100)
        self.assertEqual(rs.percentiles([])["p99"], 0)

    def test_error_text(self):
        body = rs._field(1, b"boom") + rs._varint(2 << 3) + rs._varint(7)
        self.assertEqual(rs.error_text(body), "errcode=7 errmsg=boom")


class Quorums(unittest.TestCase):
    def test_pw_and_pr_are_sent_when_set(self):
        put = {n: v for n, _, v in rs._parse_fields(rs.put_req(b"t", b"b", b"k", b"v", None, pw=2))}
        self.assertEqual(put[8], 2)  # RpbPutReq.pw
        get = {n: v for n, _, v in rs._parse_fields(rs.get_req(b"t", b"b", b"k", pr=2))}
        self.assertEqual(get[4], 2)  # RpbGetReq.pr

    def test_quorums_are_omitted_by_default(self):
        self.assertNotIn(8, [n for n, _, _ in rs._parse_fields(rs.put_req(b"t", b"b", b"k", b"v", None))])
        self.assertNotIn(4, [n for n, _, _ in rs._parse_fields(rs.get_req(b"t", b"b", b"k"))])


class FakeRiak:
    """An in-memory Riak shared by every FakeConn: records the requests it served."""

    def __init__(self):
        self.lock = threading.Lock()
        self.data = {}      # (bucket, key) -> value
        self.history = {}   # (bucket, key) -> the value before the latest one
        self.puts, self.gets = [], []
        self.timeout_every = 0   # every Nth put stores the value, then times out (outcome unknown to the client)
        self.garble_every = 0    # every Nth get returns a value with flipped bytes
        self.stale_every = 0     # every Nth get returns the previous version of the key
        self.n_put = self.n_get = 0

    def put(self, msg):
        f = {n: v for n, _, v in rs._parse_fields(msg)}
        content = {n: v for n, _, v in rs._parse_fields(f[4])}
        with self.lock:
            if (f[1], f[2]) in self.data:
                self.history[(f[1], f[2])] = self.data[(f[1], f[2])]
            self.data[(f[1], f[2])] = content[1]
            self.puts.append(f)
            self.n_put += 1
            timeout = self.timeout_every and self.n_put % self.timeout_every == 0
        if timeout:
            raise TimeoutError("The read operation timed out")
        return rs._field(2, b"vclock")

    def get(self, msg):
        f = {n: v for n, _, v in rs._parse_fields(msg)}
        with self.lock:
            self.gets.append(f)
            value = self.data.get((f[1], f[2]))
            self.n_get += 1
            if value is not None and self.garble_every and self.n_get % self.garble_every == 0:
                value = b"X" + value[1:]
            if value is not None and self.stale_every and self.n_get % self.stale_every == 0:
                value = self.history.get((f[1], f[2]), value)
        return rs._field(1, rs._field(1, value)) + rs._field(2, b"vc") if value is not None else b""


def fake_connection(riak):
    class FakeConn:
        def __init__(self, args):
            self.sock = object()

        def open(self):
            self.sock = object()

        def close(self):
            self.sock = None

        def call(self, code, data, want):
            return riak.put(data) if code == rs.MSG_PUT_REQ else riak.get(data)  # may raise: a timeout
    return FakeConn


def run_workers(riak, **overrides):
    args = argparse.Namespace(
        bucket="b0", bucket_type="t", key_prefix="c0-", seed=1, value_size=64, read_ratio=0.5,
        threads=2, rate=0, pr=0, pw=0, keyspace=0)
    for k, v in overrides.items():
        setattr(args, k, v)
    original = rs.Connection
    rs.Connection = fake_connection(riak)
    try:
        deadline = time.time() + args.duration
        stats = [rs.Stats() for _ in range(args.threads)]
        ready = threading.Event()
        threads = [threading.Thread(target=rs.worker, args=(args, i, deadline, stats[i], ready))
                   for i in range(args.threads)]
        for t in threads:
            t.start()
        ready.set()
        for t in threads:
            t.join()
    finally:
        rs.Connection = original
    return args, stats


class Worker(unittest.TestCase):
    def test_constant_rate_is_held(self):
        riak = FakeRiak()
        args, stats = run_workers(riak, duration=3, rate=100, threads=4)
        ops = sum(s.puts + s.gets for s in stats)
        # 100 ops/s for 3 s; the final verification reads are not counted in puts/gets.
        self.assertGreater(ops, 270)
        self.assertLess(ops, 330)
        self.assertEqual(sum(s.errors + s.lost + s.corrupt for s in stats), 0)

    def test_quorums_reach_every_request_and_data_verifies(self):
        riak = FakeRiak()
        _, stats = run_workers(riak, duration=1, rate=200, pr=2, pw=2)
        self.assertTrue(riak.puts and riak.gets)
        self.assertTrue(all(f.get(8) == 2 for f in riak.puts))
        self.assertTrue(all(f.get(4) == 2 for f in riak.gets))
        self.assertGreater(sum(s.verified for s in stats), 0)
        self.assertEqual(sum(s.final_lost + s.final_corrupt + s.lost + s.corrupt for s in stats), 0)

    def test_operations_rotate_over_all_buckets(self):
        riak = FakeRiak()
        buckets = ",".join("b%d" % i for i in range(5))
        run_workers(riak, duration=1, rate=250, bucket=buckets)
        self.assertEqual({k[0] for k in riak.data}, {("b%d" % i).encode() for i in range(5)})
        # Same key number in two buckets must not collide: keys carry the bucket index.
        self.assertTrue(any(b"-b3-" in k[1] for k in riak.data))

    def test_one_bucket_type_per_bucket(self):
        riak = FakeRiak()
        run_workers(riak, duration=1, rate=200, bucket="a,b,c", bucket_type="ta,tb,tc")
        pairs = {(f[1], f[16]) for f in riak.puts}
        self.assertEqual(pairs, {(b"a", b"ta"), (b"b", b"tb"), (b"c", b"tc")})

    def test_bucket_type_count_must_match(self):
        argv = ["--host", "h", "--user", "u", "--cert", "c", "--key", "k", "--cacert", "ca",
                "--bucket", "a,b,c", "--bucket-type", "ta,tb", "--duration", "1"]
        with self.assertRaises(SystemExit):
            rs.main(argv)

    def test_keyspace_bounds_the_keys_per_thread_and_bucket(self):
        riak = FakeRiak()
        _, stats = run_workers(riak, duration=2, rate=0, read_ratio=0.0, keyspace=25, threads=2)
        # 2 threads x 1 bucket x 25 keys, however many writes happened.
        self.assertEqual(len(riak.data), 50)
        self.assertGreater(sum(s.puts for s in stats), 200)

    def test_unbounded_by_default(self):
        riak = FakeRiak()
        run_workers(riak, duration=1, rate=0, read_ratio=0.0, keyspace=0)
        self.assertGreater(len(riak.data), 50)


class Integrity(unittest.TestCase):
    """What counts as lost, corrupt, stale or merely 'landed'."""

    def total(self, stats, name):
        return sum(getattr(s, name) for s in stats)

    def test_a_write_that_timed_out_but_landed_is_not_corruption(self):
        riak = FakeRiak()
        riak.timeout_every = 7
        _, stats = run_workers(riak, duration=2, rate=0, read_ratio=0.5, keyspace=20, threads=2)
        self.assertGreater(self.total(stats, "errors"), 0, "the writes did time out")
        self.assertGreater(self.total(stats, "landed"), 0, "and some of them were read back later")
        self.assertEqual(self.total(stats, "corrupt") + self.total(stats, "final_corrupt"), 0)
        self.assertEqual(self.total(stats, "stale") + self.total(stats, "ahead") + self.total(stats, "lost"), 0)

    def test_wrong_bytes_are_corrupt(self):
        riak = FakeRiak()
        riak.garble_every = 5
        _, stats = run_workers(riak, duration=1, rate=0, read_ratio=0.5, keyspace=20, threads=2)
        self.assertGreater(self.total(stats, "corrupt") + self.total(stats, "final_corrupt"), 0)

    def test_an_older_version_is_stale_not_corrupt(self):
        riak = FakeRiak()
        riak.stale_every = 4
        _, stats = run_workers(riak, duration=2, rate=0, read_ratio=0.5, keyspace=10, threads=2)
        self.assertGreater(self.total(stats, "stale"), 0, "a lost update must be reported")
        self.assertEqual(self.total(stats, "corrupt"), 0)

    def test_judge(self):
        mk = lambda v: rs.make_value("c0-", 1, 5, v, 128)
        j = lambda values, expect, uncertain=None: rs.judge(values, expect, uncertain, "c0-", 1, 5, 128)
        self.assertEqual(j([mk(3)], 3), ("ok", 3))
        self.assertEqual(j([mk(2), mk(3)], 3), ("ok", 3), "siblings that include the expected version")
        self.assertEqual(j([mk(4)], 3, uncertain=4), ("landed", 4))
        self.assertEqual(j([mk(4)], 3), ("ahead", 4), "a newer version nobody can explain")
        self.assertEqual(j([mk(5)], 3, uncertain=4), ("ahead", 5), "newer than even the unknown write")
        self.assertEqual(j([mk(2)], 3), ("stale", 2))
        self.assertEqual(j([b"c0-t1-5:v3:garbage"], 3)[0], "corrupt", "right header, wrong body")
        self.assertEqual(j([b"something else entirely"], 3)[0], "corrupt")
        self.assertEqual(j([rs.make_value("c0-", 1, 6, 3, 128)], 3)[0], "corrupt", "another key's value")
        self.assertEqual(j([b"c0-t1-5:vX:abc"], 3)[0], "corrupt", "unparseable version")


class Windows(unittest.TestCase):
    def test_window_record_summarises_and_resets(self):
        a, b = rs.Stats(), rs.Stats()
        a.win_put, a.win_get, a.win_errors, a.late = [1.0, 3.0], [2.0], 1, 4
        b.win_put, b.win_get = [5.0], [4.0, 6.0]
        r = rs.window_record([a, b], 10.0)
        self.assertEqual((r["puts"], r["gets"], r["ops"], r["errors"], r["late"]), (3, 3, 6, 1, 4))
        self.assertEqual(r["ops_per_s"], 0.6)
        self.assertEqual(r["put"]["max"], 5.0)
        self.assertEqual(r["get"]["max"], 6.0)
        again = rs.window_record([a, b], 10.0)
        self.assertEqual((again["ops"], again["errors"], again["late"]), (0, 0, 0))


if __name__ == "__main__":
    unittest.main()
