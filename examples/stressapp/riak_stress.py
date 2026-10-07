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

Constant-rate / soak mode
  --rate OPS paces the client at a constant OPS operations per second in total (open loop: each
  thread follows a fixed schedule and never bursts to catch up). --pr/--pw send the read/write
  quorums with every request. --bucket may list several buckets (comma separated); operations
  rotate over them, and --bucket-type is one type for all of them or one type per bucket. --keyspace N bounds the keys each thread keeps per bucket, so a long run
  overwrites instead of growing client memory. --window S prints one `WINDOW {json}` line to
  stderr every S seconds with that interval's rate, errors and latency percentiles.

Usage
  riak_stress.py --host HOST --user USER --cert tls.crt --key tls.key --cacert ca.crt \
      --bucket-type TYPE[,TYPE...] --bucket BUCKET[,BUCKET...] [--threads 16] [--duration 60] \
      [--value-size 1024] [--read-ratio 0.7] [--port 8087] [--key-prefix c0-] \
      [--rate 0] [--pr N] [--pw N] [--keyspace 0] [--window 0]
Exit status: 0 when there were no errors and no lost, corrupt, stale or unexplained values; 2 otherwise.
A write that timed out but did reach Riak shows up as `landed`, which is not a failure.
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


def _uint_field(num, value):
    """One unsigned varint (wire type 0) field."""
    return _varint(num << 3) + _varint(value)


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


def put_req(btype, bucket, key, value, vclock, pw=None):
    # RpbPutReq: bucket=1 key=2 vclock=3 content=4 pw=8 return_head=11 type=16.
    content = _field(1, value) + _field(2, b"application/octet-stream")  # RpbContent
    msg = _field(1, bucket) + _field(2, key)
    if vclock:
        msg += _field(3, vclock)
    msg += _field(4, content)
    if pw:
        msg += _uint_field(8, pw)
    return msg + _bool_field(11, True) + _field(16, btype)


def get_req(btype, bucket, key, pr=None):
    # RpbGetReq: bucket=1 key=2 pr=4 type=13.
    msg = _field(1, bucket) + _field(2, key)
    if pr:
        msg += _uint_field(4, pr)
    return msg + _field(13, btype)


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


def judge(values, expect, uncertain, prefix, vt, vn, size):
    """Compare what a read returned with what this client last wrote.

    Returns (kind, version): kind is
      ok       the expected version is among the values;
      landed   a newer version than expected, exactly the one of an earlier write whose outcome
               was unknown (it timed out or the connection broke) and that did reach Riak: benign;
      ahead    a newer version nobody can explain;
      stale    an older version than expected: an acknowledged write was lost;
      corrupt  a value that is not any version this client could have written (wrong bytes).
    """
    head = ("%st%d-%d:v" % (prefix, vt, vn)).encode()
    seen = []
    for v in values:
        if not v.startswith(head):
            return "corrupt", None
        try:
            vo = int(v[len(head):].split(b":", 1)[0])
        except ValueError:
            return "corrupt", None
        if v != make_value(prefix, vt, vn, vo, size):
            return "corrupt", None
        seen.append(vo)
    if expect in seen:
        return "ok", expect
    top = max(seen)
    if uncertain is not None and top == uncertain:
        return "landed", top
    return ("ahead" if top > expect else "stale"), top


class Stats:
    """Per-thread counters; merged at the end so threads never share state."""

    def __init__(self):
        self.puts = self.gets = self.errors = self.lost = self.corrupt = self.siblings = 0
        self.stale = self.ahead = self.landed = 0
        self.put_ms, self.get_ms = [], []
        self.win_put, self.win_get = [], []   # latencies since the last WINDOW line
        self.win_errors = self.late = 0
        self.error_kinds = {}
        self.verified = self.final_lost = self.final_corrupt = 0

    def error(self, exc):
        self.errors += 1
        self.win_errors += 1
        kind = type(exc).__name__ + ": " + str(exc)[:80]
        self.error_kinds[kind] = self.error_kinds.get(kind, 0) + 1


MAX_SAMPLES = 200000


def sample(lst, value):
    if len(lst) < MAX_SAMPLES:
        lst.append(value)
    else:  # keep the sample bounded on long runs
        lst[random.randrange(MAX_SAMPLES)] = value


class BucketState:
    """What one thread has written to one bucket: the latest version and vclock per key."""

    def __init__(self):
        self.versions = {}   # n -> latest version written
        self.vclocks = {}    # n -> vclock of the latest write
        self.uncertain = {}  # n -> version of a write whose outcome is unknown (it failed or timed out)
        self.next_key = 0


def worker(args, tid, deadline, stats, ready):
    buckets = [b.encode() for b in args.bucket.split(",")]
    key_bytes = lambda bi, n: ("%st%d-b%d-%d" % (args.key_prefix, tid, bi, n)).encode() \
        if len(buckets) > 1 else ("%st%d-%d" % (args.key_prefix, tid, n)).encode()
    # The value only depends on (thread, key, version); fold the bucket into the "thread" part so
    # the same key number in two buckets does not share values.
    value_id = lambda bi, n: (tid * 1000 + bi, n)
    # One bucket type for all buckets, or one per bucket (same order as --bucket).
    types = [t.encode() for t in args.bucket_type.split(",")]
    if len(types) not in (1, len(buckets)):
        raise SystemExit("--bucket-type needs 1 entry or one per --bucket entry")
    btypes = types * len(buckets) if len(types) == 1 else types
    rnd = random.Random(args.seed * 1000 + tid)
    states = [BucketState() for _ in buckets]
    conn = Connection(args)
    interval = (args.threads / args.rate) if args.rate > 0 else 0.0
    ready.wait()
    due = time.time()
    count = 0
    while time.time() < deadline:
        if interval:
            # Open-loop pacing: one fixed schedule per thread. Never burst to catch up; after a
            # stall longer than a second, resynchronise and count the missed slots as late.
            due += interval
            delay = due - time.time()
            if delay > 0:
                time.sleep(delay)
            elif delay < -1.0:
                stats.late += int(-delay / interval)
                due = time.time()
        bi = count % len(buckets)
        count += 1
        st = states[bi]
        bucket = buckets[bi]
        try:
            if conn.sock is None:
                conn.open()
            if st.versions and rnd.random() < args.read_ratio:
                n = rnd.choice(list(st.versions)) if len(st.versions) < 64 else rnd.randrange(st.next_key)
                if n not in st.versions:
                    continue
                t0 = time.time()
                body = conn.call(MSG_GET_REQ, get_req(btypes[bi], bucket, key_bytes(bi, n), args.pr), MSG_GET_RESP)
                ms = (time.time() - t0) * 1000
                sample(stats.get_ms, ms)
                stats.win_get.append(ms)
                stats.gets += 1
                values, vclock = parse_get(body)
                vt, vn = value_id(bi, n)
                if not values:
                    stats.lost += 1
                else:
                    if len(values) > 1:
                        stats.siblings += 1
                    record_read(stats, st, n, values, vclock, args, vt, vn)
            else:
                # Overwrite an existing key a third of the time, otherwise write a new one; once
                # the keyspace is full every write overwrites.
                full = args.keyspace > 0 and st.next_key >= args.keyspace
                if st.versions and (full or rnd.random() < 0.33):
                    n = rnd.randrange(st.next_key)
                else:
                    n, st.next_key = st.next_key, st.next_key + 1
                version = st.versions.get(n, 0) + 1
                vt, vn = value_id(bi, n)
                value = make_value(args.key_prefix, vt, vn, version, args.value_size)
                t0 = time.time()
                st.uncertain[n] = version   # until Riak answers we do not know whether it was stored
                body = conn.call(MSG_PUT_REQ, put_req(btypes[bi], bucket, key_bytes(bi, n), value,
                                                       st.vclocks.get(n), args.pw), MSG_PUT_RESP)
                st.uncertain.pop(n, None)
                ms = (time.time() - t0) * 1000
                sample(stats.put_ms, ms)
                stats.win_put.append(ms)
                stats.puts += 1
                st.versions[n] = version
                st.vclocks[n] = parse_put_vclock(body)
        except (OSError, EOFError, RiakError, ssl.SSLError) as exc:
            stats.error(exc)
            conn.close()
            time.sleep(0.2)  # brief backoff before reconnecting
    # Final verification: read back every key this thread wrote.
    for bi, st in enumerate(states):
        for n, version in st.versions.items():
            try:
                if conn.sock is None:
                    conn.open()
                body = conn.call(MSG_GET_REQ, get_req(btypes[bi], buckets[bi], key_bytes(bi, n), args.pr), MSG_GET_RESP)
                values, vclock = parse_get(body)
                stats.verified += 1
                vt, vn = value_id(bi, n)
                if not values:
                    stats.final_lost += 1
                else:
                    record_read(stats, st, n, values, vclock, args, vt, vn, final=True)
            except (OSError, EOFError, RiakError, ssl.SSLError) as exc:
                stats.error(exc)
                conn.close()
    conn.close()


def record_read(stats, st, n, values, vclock, args, vt, vn, final=False):
    """Judge one read and count it; a newer version we can account for is adopted as the state."""
    kind, version = judge(values, st.versions[n], st.uncertain.get(n), args.key_prefix, vt, vn, args.value_size)
    if kind == "ok":
        return
    if kind == "corrupt":
        if final:
            stats.final_corrupt += 1
        else:
            stats.corrupt += 1
        return
    if kind == "landed":
        stats.landed += 1
        st.uncertain.pop(n, None)
    elif kind == "ahead":
        stats.ahead += 1
    else:
        stats.stale += 1
    # Count each discrepancy once: continue from what Riak actually holds.
    st.versions[n] = version
    if vclock is not None:
        st.vclocks[n] = vclock


def window_record(all_stats, seconds):
    """The WINDOW record for the interval just ended; resets the per-interval counters.

    Threads keep appending while this swaps their lists, so a sample may land in the previous
    window: harmless for rates and percentiles over a minute-long interval."""
    put_ms, get_ms, errors, late = [], [], 0, 0
    for st in all_stats:
        p, st.win_put = st.win_put, []
        g, st.win_get = st.win_get, []
        put_ms += p
        get_ms += g
        errors += st.win_errors
        st.win_errors = 0
        late += st.late
        st.late = 0
    ops = len(put_ms) + len(get_ms)
    return {
        "t": int(time.time()), "seconds": round(seconds, 1), "ops": ops,
        "puts": len(put_ms), "gets": len(get_ms),
        "ops_per_s": round(ops / max(seconds, 1e-9), 1), "errors": errors, "late": late,
        "put": percentiles(put_ms), "get": percentiles(get_ms),
    }


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
    p.add_argument("--rate", type=float, default=0,
                   help="constant total operations per second for this client (0 = as fast as possible)")
    p.add_argument("--pr", type=int, default=0, help="primary read quorum sent with every GET (0 = bucket default)")
    p.add_argument("--pw", type=int, default=0, help="primary write quorum sent with every PUT (0 = bucket default)")
    p.add_argument("--keyspace", type=int, default=0,
                   help="keys each thread keeps per bucket; once full, writes overwrite (0 = unbounded)")
    p.add_argument("--window", type=float, default=0,
                   help="print a WINDOW json line to stderr every this many seconds (0 = off)")
    args = p.parse_args(argv)
    n_buckets, n_types = len(args.bucket.split(",")), len(args.bucket_type.split(","))
    if n_types not in (1, n_buckets):
        p.error("--bucket-type needs 1 entry or one per --bucket entry (got %d types for %d buckets)"
                % (n_types, n_buckets))

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
    next_window = started + args.window if args.window > 0 else None
    last_window = started
    while any(t.is_alive() for t in threads):
        time.sleep(0.5)
        now = time.time()
        if next_window is not None and now >= next_window:
            sys.stderr.write("WINDOW " + json.dumps(window_record(all_stats, now - last_window), sort_keys=True) + "\n")
            sys.stderr.flush()
            last_window, next_window = now, next_window + args.window
        elif next_window is None and now >= next_report:
            done = sum(s.puts + s.gets for s in all_stats)
            sys.stderr.write("PROGRESS %.0fs ops=%d (%.0f/s) errors=%d\n" % (
                now - started, done, done / max(now - started, 1e-9),
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
        "stale": sum(s.stale for s in all_stats),
        "ahead": sum(s.ahead for s in all_stats),
        "landed": sum(s.landed for s in all_stats),
        "siblings": sum(s.siblings for s in all_stats),
        "verified": sum(s.verified for s in all_stats),
        "final_lost": sum(s.final_lost for s in all_stats),
        "final_corrupt": sum(s.final_corrupt for s in all_stats),
        "duration_s": round(elapsed, 2),
        "ops_per_s": round((puts + gets) / elapsed, 1),
        "threads": args.threads, "value_size": args.value_size, "read_ratio": args.read_ratio,
        "target_rate": args.rate, "late": sum(s.late for s in all_stats),
        "latency_ms": {
            "put": percentiles([x for s in all_stats for x in s.put_ms]),
            "get": percentiles([x for s in all_stats for x in s.get_ms]),
        },
        "error_kinds": kinds,
    }
    print("RESULT " + json.dumps(result, sort_keys=True))
    # "landed" (a write that timed out but reached Riak) is not a failure; errors are reported on their own.
    bad = (result["errors"] + result["lost"] + result["corrupt"] + result["stale"] + result["ahead"]
           + result["final_lost"] + result["final_corrupt"])
    return 0 if bad == 0 and puts > 0 else 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
