#!/usr/bin/env python3
"""Example application: a Riak KV stress client.

Hammers a Riak cluster with a mix of writes and reads over the Protocol Buffers
interface, authenticating with a client certificate (mTLS, certificate auth), and
reports throughput, latency percentiles and data-integrity results.

It is deliberately self-contained: only the Python standard library (socket, ssl,
struct, threading), so it runs in any stock python image with the RiakUser's client
certificate Secret mounted. No pip install, no registry of its own.

How it checks the data
  * Every worker thread owns its keys (`<prefix>t<thread>-<n>`), so there is exactly one
    writer per key and no sibling conflicts, and every value is derived deterministically
    from (thread, key, version).
  * Reads pick a key the thread has already written and compare the returned value with
    the expected one: a missing key counts as LOST, a different value as CORRUPT.
  * After the timed phase every key is read once more (the final verification).

Output
  Progress lines every 10 s on stderr, and one final line on stdout:
      RESULT {"ops": ..., "puts": ..., "gets": ..., "errors": ..., "lost": ..., "corrupt": ..., ...}

Usage
  riak_stress.py --host HOST --user USER --cert tls.crt --key tls.key --cacert ca.crt \
      --bucket-type TYPE --bucket BUCKET [--threads 16] [--duration 60] [--value-size 1024] \
      [--read-ratio 0.7] [--port 8087] [--key-prefix c0-]
Exit status: 0 when there were no errors, lost or corrupt values; 2 otherwise.
"""
import argparse
import hashlib
import json
import random
import socket
import ssl
import struct
import sys
import threading
import time

# Riak PB message codes.
MSG_ERROR = 0
MSG_GET_REQ = 9
MSG_GET_RESP = 10
MSG_PUT_REQ = 11
MSG_PUT_RESP = 12
MSG_AUTH_REQ = 253
MSG_AUTH_RESP = 254
MSG_START_TLS = 255


# ── minimal protobuf wire encoding ──────────────────────────────────────────
def _varint(n):
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def _field(num, data):
    """One length-delimited (wire type 2) field."""
    return _varint((num << 3) | 2) + _varint(len(data)) + data


def _bool_field(num, value):
    """One varint (wire type 0) field."""
    return _varint(num << 3) + _varint(1 if value else 0)


def _read_varint(data, i):
    shift = result = 0
    while True:
        b = data[i]
        i += 1
        result |= (b & 0x7F) << shift
        if not b & 0x80:
            return result, i
        shift += 7


def _parse_fields(data):
    """Yield (field_num, wire_type, value) for a serialized message."""
    i = 0
    while i < len(data):
        tag, i = _read_varint(data, i)
        num, wire = tag >> 3, tag & 7
        if wire == 2:
            length, i = _read_varint(data, i)
            yield num, wire, data[i:i + length]
            i += length
        elif wire == 0:
            val, i = _read_varint(data, i)
            yield num, wire, val
        elif wire == 5:
            yield num, wire, data[i:i + 4]
            i += 4
        elif wire == 1:
            yield num, wire, data[i:i + 8]
            i += 8
        else:
            raise ValueError("unsupported wire type %d" % wire)


def put_req(btype, bucket, key, value, vclock):
    # RpbPutReq: bucket=1 key=2 vclock=3 content=4 return_head=11 type=16.
    content = _field(1, value) + _field(2, b"application/octet-stream")  # RpbContent
    msg = _field(1, bucket) + _field(2, key)
    if vclock:
        msg += _field(3, vclock)
    return msg + _field(4, content) + _bool_field(11, True) + _field(16, btype)


def get_req(btype, bucket, key):
    # RpbGetReq: bucket=1 key=2 type=13.
    return _field(1, bucket) + _field(2, key) + _field(13, btype)


def parse_get(body):
    """-> (list of sibling values, vclock). An empty list means not found."""
    values, vclock = [], None
    for num, wire, val in _parse_fields(body):
        if num == 1 and wire == 2:  # RpbContent
            for cnum, cwire, cval in _parse_fields(val):
                if cnum == 1 and cwire == 2:
                    values.append(cval)
        elif num == 2 and wire == 2:
            vclock = val
    return values, vclock


def parse_put_vclock(body):
    for num, wire, val in _parse_fields(body):
        if num == 2 and wire == 2:
            return val
    return None


def error_text(body):
    msg, code = b"", None
    for num, wire, val in _parse_fields(body):
        if num == 1 and wire == 2:
            msg = val
        elif num == 2 and wire == 0:
            code = val
    return "errcode=%s errmsg=%s" % (code, msg.decode("utf-8", "replace"))


# ── framing ─────────────────────────────────────────────────────────────────
def send_msg(sock, code, data=b""):
    sock.sendall(struct.pack(">IB", len(data) + 1, code) + data)


def recv_exact(sock, n):
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise EOFError("connection closed after %d/%d bytes" % (len(buf), n))
        buf += chunk
    return bytes(buf)


def recv_msg(sock):
    (length,) = struct.unpack(">I", recv_exact(sock, 4))
    body = recv_exact(sock, length)
    return body[0], body[1:]


class RiakError(Exception):
    pass


class Connection:
    """One authenticated mTLS protocol-buffers connection."""

    def __init__(self, args):
        self.args = args
        self.sock = None

    def open(self):
        a = self.args
        raw = socket.create_connection((a.host, a.port), timeout=a.timeout)
        send_msg(raw, MSG_START_TLS)
        code, _ = recv_msg(raw)
        if code != MSG_START_TLS:
            raise RiakError("server refused StartTls (code %d)" % code)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.load_verify_locations(cafile=a.cacert)
        ctx.load_cert_chain(certfile=a.cert, keyfile=a.key)
        self.sock = ctx.wrap_socket(raw, server_hostname=a.host)
        # Certificate auth: the CN of the client certificate authenticates the user; the
        # password is empty.
        send_msg(self.sock, MSG_AUTH_REQ, _field(1, a.user.encode()) + _field(2, b""))
        code, body = recv_msg(self.sock)
        if code == MSG_ERROR:
            raise RiakError("auth: " + error_text(body))
        if code != MSG_AUTH_RESP:
            raise RiakError("unexpected auth response code %d" % code)

    def close(self):
        try:
            if self.sock:
                self.sock.close()
        except OSError:
            pass
        self.sock = None

    def call(self, code, data, want):
        send_msg(self.sock, code, data)
        rcode, body = recv_msg(self.sock)
        if rcode == MSG_ERROR:
            raise RiakError(error_text(body))
        if rcode != want:
            raise RiakError("unexpected response code %d (want %d)" % (rcode, want))
        return body


def make_value(prefix, thread, n, version, size):
    """Deterministic value for (key, version): verifiable without storing it."""
    head = ("%st%d-%d:v%d:" % (prefix, thread, n, version)).encode()
    seed = hashlib.sha256(head).digest()
    body = (seed * (size // len(seed) + 1))[:max(size - len(head), 0)]
    return head + body


class Stats:
    """Per-thread counters; merged at the end so threads never share state."""

    def __init__(self):
        self.puts = self.gets = self.errors = self.lost = self.corrupt = self.siblings = 0
        self.put_ms, self.get_ms = [], []
        self.error_kinds = {}
        self.verified = self.final_lost = self.final_corrupt = 0

    def error(self, exc):
        self.errors += 1
        kind = type(exc).__name__ + ": " + str(exc)[:80]
        self.error_kinds[kind] = self.error_kinds.get(kind, 0) + 1


MAX_SAMPLES = 200000


def sample(lst, value):
    if len(lst) < MAX_SAMPLES:
        lst.append(value)
    else:  # keep the sample bounded on long runs
        lst[random.randrange(MAX_SAMPLES)] = value


def worker(args, tid, deadline, stats, ready):
    key_bytes = lambda n: ("%st%d-%d" % (args.key_prefix, tid, n)).encode()
    btype, bucket = args.bucket_type.encode(), args.bucket.encode()
    rnd = random.Random(args.seed * 1000 + tid)
    versions = {}   # n -> latest version written
    vclocks = {}    # n -> vclock of the latest write
    conn = Connection(args)
    next_key = 0
    ready.wait()
    while time.time() < deadline:
        try:
            if conn.sock is None:
                conn.open()
            if versions and rnd.random() < args.read_ratio:
                n = rnd.choice(list(versions)) if len(versions) < 64 else rnd.randrange(next_key)
                if n not in versions:
                    continue
                t0 = time.time()
                body = conn.call(MSG_GET_REQ, get_req(btype, bucket, key_bytes(n)), MSG_GET_RESP)
                sample(stats.get_ms, (time.time() - t0) * 1000)
                stats.gets += 1
                values, _ = parse_get(body)
                want = make_value(args.key_prefix, tid, n, versions[n], args.value_size)
                if not values:
                    stats.lost += 1
                else:
                    if len(values) > 1:
                        stats.siblings += 1
                    if want not in values:
                        stats.corrupt += 1
            else:
                # Overwrite an existing key a third of the time, otherwise write a new one.
                if versions and rnd.random() < 0.33:
                    n = rnd.randrange(next_key)
                else:
                    n, next_key = next_key, next_key + 1
                version = versions.get(n, 0) + 1
                value = make_value(args.key_prefix, tid, n, version, args.value_size)
                t0 = time.time()
                body = conn.call(MSG_PUT_REQ, put_req(btype, bucket, key_bytes(n), value, vclocks.get(n)),
                                 MSG_PUT_RESP)
                sample(stats.put_ms, (time.time() - t0) * 1000)
                stats.puts += 1
                versions[n] = version
                vclocks[n] = parse_put_vclock(body)
        except (OSError, EOFError, RiakError, ssl.SSLError) as exc:
            stats.error(exc)
            conn.close()
            time.sleep(0.2)  # brief backoff before reconnecting
    # Final verification: read back every key this thread wrote.
    for n, version in versions.items():
        try:
            if conn.sock is None:
                conn.open()
            body = conn.call(MSG_GET_REQ, get_req(btype, bucket, key_bytes(n)), MSG_GET_RESP)
            values, _ = parse_get(body)
            stats.verified += 1
            if not values:
                stats.final_lost += 1
            elif make_value(args.key_prefix, tid, n, version, args.value_size) not in values:
                stats.final_corrupt += 1
        except (OSError, EOFError, RiakError, ssl.SSLError) as exc:
            stats.error(exc)
            conn.close()
    conn.close()


def percentiles(samples):
    if not samples:
        return {"p50": 0, "p95": 0, "p99": 0, "max": 0}
    s = sorted(samples)
    pick = lambda q: round(s[min(int(len(s) * q), len(s) - 1)], 2)
    return {"p50": pick(0.50), "p95": pick(0.95), "p99": pick(0.99), "max": round(s[-1], 2)}


def main(argv):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", required=True, help="Riak Service host (must be in the server certificate)")
    p.add_argument("--port", type=int, default=8087)
    p.add_argument("--user", required=True, help="Riak username (== client certificate CN)")
    p.add_argument("--cert", required=True)
    p.add_argument("--key", required=True)
    p.add_argument("--cacert", required=True)
    p.add_argument("--bucket-type", required=True)
    p.add_argument("--bucket", required=True)
    p.add_argument("--threads", type=int, default=16)
    p.add_argument("--duration", type=float, default=60, help="seconds of timed load")
    p.add_argument("--value-size", type=int, default=1024, help="bytes per object")
    p.add_argument("--read-ratio", type=float, default=0.7, help="fraction of operations that are reads")
    p.add_argument("--key-prefix", default="", help="distinguishes parallel clients, e.g. c0-")
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--timeout", type=float, default=30, help="socket timeout in seconds")
    args = p.parse_args(argv)

    deadline = time.time() + args.duration + 1  # +1: let all threads connect first
    ready = threading.Event()
    all_stats = [Stats() for _ in range(args.threads)]
    threads = [threading.Thread(target=worker, args=(args, i, deadline, all_stats[i], ready), daemon=True)
               for i in range(args.threads)]
    for t in threads:
        t.start()
    started = time.time()
    ready.set()
    next_report = started + 10
    while any(t.is_alive() for t in threads):
        time.sleep(0.5)
        if time.time() >= next_report:
            done = sum(s.puts + s.gets for s in all_stats)
            sys.stderr.write("PROGRESS %.0fs ops=%d (%.0f/s) errors=%d\n" % (
                time.time() - started, done, done / max(time.time() - started, 1e-9),
                sum(s.errors for s in all_stats)))
            sys.stderr.flush()
            next_report += 10
    elapsed = max(min(time.time(), deadline) - started, 1e-9)

    puts = sum(s.puts for s in all_stats)
    gets = sum(s.gets for s in all_stats)
    kinds = {}
    for s in all_stats:
        for k, v in s.error_kinds.items():
            kinds[k] = kinds.get(k, 0) + v
    result = {
        "ops": puts + gets, "puts": puts, "gets": gets,
        "errors": sum(s.errors for s in all_stats),
        "lost": sum(s.lost for s in all_stats),
        "corrupt": sum(s.corrupt for s in all_stats),
        "siblings": sum(s.siblings for s in all_stats),
        "verified": sum(s.verified for s in all_stats),
        "final_lost": sum(s.final_lost for s in all_stats),
        "final_corrupt": sum(s.final_corrupt for s in all_stats),
        "duration_s": round(elapsed, 2),
        "ops_per_s": round((puts + gets) / elapsed, 1),
        "threads": args.threads, "value_size": args.value_size, "read_ratio": args.read_ratio,
        "latency_ms": {
            "put": percentiles([x for s in all_stats for x in s.put_ms]),
            "get": percentiles([x for s in all_stats for x in s.get_ms]),
        },
        "error_kinds": kinds,
    }
    print("RESULT " + json.dumps(result, sort_keys=True))
    bad = result["errors"] + result["lost"] + result["corrupt"] + result["final_lost"] + result["final_corrupt"]
    return 0 if bad == 0 and puts > 0 else 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
