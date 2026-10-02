"""Offline tests for riak_stress.py's protocol helpers (no Riak needed).

    python3 -m unittest examples/stressapp/test_riak_stress.py
"""
import importlib.util
import os
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


if __name__ == "__main__":
    unittest.main()
