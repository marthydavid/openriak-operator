#!/usr/bin/env python3
"""Authenticated Riak KV protobuf write/read check using mTLS certificate auth.

Speaks the Riak Protocol Buffers wire protocol directly with only the Python
standard library (socket / ssl / struct) — no third-party client, so it needs no
pip install or network access in CI. The flow is:

  1. TCP connect to the PB port (8087)
  2. RpbStartTls (code 255)          -> upgrade the socket to TLS
  3. TLS handshake presenting the client cert (CN == username == certificate auth)
  4. RpbAuthReq  (code 253)          -> authenticate as the user
  5. RpbPutReq   (code 11)           -> WRITABLE check
  6. RpbGetReq   (code 9)            -> WORKING check (read back and compare)

Prints a final "PASS" line and exits 0 on success; otherwise prints a diagnostic
and exits non-zero.

Usage:
  pb_cert_auth_check.py HOST PORT USER CERT KEY CACERT BUCKET_TYPE BUCKET
"""
import socket
import ssl
import struct
import sys

# Riak PB message codes.
MSG_ERROR = 0
MSG_GET_REQ = 9
MSG_GET_RESP = 10
MSG_PUT_REQ = 11
MSG_PUT_RESP = 12
MSG_AUTH_REQ = 253
MSG_AUTH_RESP = 254
MSG_START_TLS = 255

TEST_KEY = b"verify-key"
TEST_VAL = b"verify-value-openriak"


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


def _field(field_num, data):
    """Encode one length-delimited (wire type 2) field."""
    tag = (field_num << 3) | 2
    return _varint(tag) + _varint(len(data)) + data


def _read_varint(data, i):
    shift = 0
    result = 0
    while True:
        b = data[i]
        i += 1
        result |= (b & 0x7F) << shift
        if not (b & 0x80):
            return result, i
        shift += 7


def _parse_fields(data):
    """Yield (field_num, wire_type, value) for a serialized message."""
    i = 0
    while i < len(data):
        tag, i = _read_varint(data, i)
        field_num, wire = tag >> 3, tag & 7
        if wire == 2:
            length, i = _read_varint(data, i)
            yield field_num, wire, data[i:i + length]
            i += length
        elif wire == 0:
            val, i = _read_varint(data, i)
            yield field_num, wire, val
        elif wire == 5:
            yield field_num, wire, data[i:i + 4]
            i += 4
        elif wire == 1:
            yield field_num, wire, data[i:i + 8]
            i += 8
        else:
            raise ValueError("unsupported wire type %d" % wire)


# ── message builders ────────────────────────────────────────────────────────
def rpb_auth_req(user, password):
    return _field(1, user) + _field(2, password)


def rpb_put_req(btype, bucket, key, value, content_type):
    content = _field(1, value) + _field(2, content_type)  # RpbContent
    # RpbPutReq: bucket=1, key=2, content=4, type=16 (field 14 is sloppy_quorum).
    return _field(1, bucket) + _field(2, key) + _field(4, content) + _field(16, btype)


def rpb_get_req(btype, bucket, key):
    # RpbGetReq: bucket=1, key=2, type=13.
    return _field(1, bucket) + _field(2, key) + _field(13, btype)


def get_resp_value(data):
    """Extract the first RpbContent.value from an RpbGetResp."""
    for fn, wire, val in _parse_fields(data):
        if fn == 1 and wire == 2:  # content
            for cfn, cwire, cval in _parse_fields(val):
                if cfn == 1 and cwire == 2:  # value
                    return cval
    return None


def error_text(data):
    msg, code = b"", None
    for fn, wire, val in _parse_fields(data):
        if fn == 1 and wire == 2:
            msg = val
        elif fn == 2 and wire == 0:
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


def main(argv):
    if len(argv) != 8:
        sys.stderr.write(
            "usage: pb_cert_auth_check.py HOST PORT USER CERT KEY CACERT BUCKET_TYPE BUCKET\n"
        )
        return 64

    host, port_s, user, cert_file, key_file, cacert_file, btype, bucket = argv
    port = int(port_s)

    raw = socket.create_connection((host, port), timeout=30)

    print("START_TLS to %s:%d" % (host, port))
    send_msg(raw, MSG_START_TLS)
    code, _ = recv_msg(raw)
    if code != MSG_START_TLS:
        print("FAIL: server refused StartTls (code=%d)" % code)
        return 3

    # Present the client certificate; the CN is what authenticates the user.
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.load_verify_locations(cafile=cacert_file)
    ctx.load_cert_chain(certfile=cert_file, keyfile=key_file)
    tls = ctx.wrap_socket(raw, server_hostname=host)
    print("TLS established: %s" % (tls.cipher(),))

    print("AUTH as %s (certificate auth)" % user)
    send_msg(tls, MSG_AUTH_REQ, rpb_auth_req(user.encode(), b""))
    code, body = recv_msg(tls)
    if code == MSG_ERROR:
        print("FAIL: auth error: %s" % error_text(body))
        return 4
    if code != MSG_AUTH_RESP:
        print("FAIL: unexpected auth response code %d" % code)
        return 4

    print("WRITE %s/%s/%s" % (btype, bucket, TEST_KEY.decode()))
    send_msg(tls, MSG_PUT_REQ,
             rpb_put_req(btype.encode(), bucket.encode(), TEST_KEY, TEST_VAL, b"text/plain"))
    code, body = recv_msg(tls)
    if code == MSG_ERROR:
        print("FAIL: put error: %s" % error_text(body))
        return 5
    if code != MSG_PUT_RESP:
        print("FAIL: unexpected put response code %d" % code)
        return 5
    print("WRITE_OK")

    print("READ %s/%s/%s" % (btype, bucket, TEST_KEY.decode()))
    send_msg(tls, MSG_GET_REQ, rpb_get_req(btype.encode(), bucket.encode(), TEST_KEY))
    code, body = recv_msg(tls)
    if code == MSG_ERROR:
        print("FAIL: get error: %s" % error_text(body))
        return 6
    if code != MSG_GET_RESP:
        print("FAIL: unexpected get response code %d" % code)
        return 6

    got = get_resp_value(body)
    print("READ_OK %r" % got)
    if got != TEST_VAL:
        print("FAIL: value mismatch want=%r got=%r" % (TEST_VAL, got))
        return 1

    print("PASS: bucket type %r bucket %r is reachable and writable via certificate auth"
          % (btype, bucket))
    tls.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
