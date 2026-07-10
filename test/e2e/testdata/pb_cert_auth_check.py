#!/usr/bin/env python3
"""Verify Riak certificate-based auth end to end over the protobuf API.

Speaks the Riak protobuf protocol directly: STARTTLS on the protobuf port,
client-certificate TLS (Riak matches the cert CN against the username), then
a put/get roundtrip proving the granted permissions work. Protobuf messages
are hand-encoded — every field used is length-delimited (wire type 2), so no
protobuf library is required and the script runs on a bare python image.

Environment: RIAK_HOST (required), RIAK_PORT (default 8087), RIAK_USER
(required, must equal the client cert CN), CERT_DIR (default /certs, expects
cert-manager keys tls.crt / tls.key / ca.crt).
"""
import os
import socket
import ssl
import struct
import sys
import time

HOST = os.environ["RIAK_HOST"]
PORT = int(os.environ.get("RIAK_PORT", "8087"))
USER = os.environ["RIAK_USER"]
CERT_DIR = os.environ.get("CERT_DIR", "/certs")

MSG_ERROR, MSG_PING, MSG_PONG = 0, 1, 2
MSG_GET, MSG_GET_RESP, MSG_PUT, MSG_PUT_RESP = 9, 10, 11, 12
MSG_AUTH, MSG_AUTH_RESP, MSG_STARTTLS = 253, 254, 255


def varint(n):
    out = b""
    while True:
        b7 = n & 0x7F
        n >>= 7
        out += bytes([b7 | (0x80 if n else 0)])
        if not n:
            return out


def field(num, data):  # wire type 2 (length-delimited)
    return varint(num << 3 | 2) + varint(len(data)) + data


def read_varint(buf, i):
    n = shift = 0
    while True:
        n |= (buf[i] & 0x7F) << shift
        i += 1
        if not buf[i - 1] & 0x80:
            return n, i
        shift += 7


def fields(buf):
    i = 0
    while i < len(buf):
        tag, i = read_varint(buf, i)
        num, wt = tag >> 3, tag & 7
        if wt == 2:
            ln, i = read_varint(buf, i)
            yield num, buf[i:i + ln]
            i += ln
        elif wt == 0:
            v, i = read_varint(buf, i)
            yield num, v
        else:
            raise ValueError("unexpected wire type %d" % wt)


def send(sock, code, payload=b""):
    sock.sendall(struct.pack(">IB", len(payload) + 1, code) + payload)


def recv(sock):
    hdr = b""
    while len(hdr) < 5:
        chunk = sock.recv(5 - len(hdr))
        if not chunk:
            raise ConnectionError("connection closed")
        hdr += chunk
    (ln,), code = struct.unpack(">I", hdr[:4]), hdr[4]
    payload = b""
    while len(payload) < ln - 1:
        payload += sock.recv(ln - 1 - len(payload))
    return code, payload


def expect(sock, want, what):
    code, payload = recv(sock)
    if code == MSG_ERROR:
        msg = next((v for n, v in fields(payload) if n == 1), b"?")
        raise RuntimeError("%s: RpbErrorResp: %s" % (what, msg.decode()))
    if code != want:
        raise RuntimeError("%s: expected msg code %d, got %d" % (what, want, code))
    return payload


def connect(with_cert, username=None):
    raw = socket.create_connection((HOST, PORT), timeout=10)
    send(raw, MSG_STARTTLS)
    expect(raw, MSG_STARTTLS, "starttls")

    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.load_verify_locations(CERT_DIR + "/ca.crt")
    if with_cert:
        ctx.load_cert_chain(CERT_DIR + "/tls.crt", CERT_DIR + "/tls.key")
    sock = ctx.wrap_socket(raw, server_hostname=HOST)

    if username:
        send(sock, MSG_AUTH, field(1, username.encode()) + field(2, b""))
        expect(sock, MSG_AUTH_RESP, "auth")
    return sock


def main():
    # The operator may still be converging (security enable, grants); retry
    # the full handshake+auth before declaring failure.
    sock = None
    for attempt in range(20):
        try:
            sock = connect(with_cert=True, username=USER)
            break
        except Exception as e:  # noqa: BLE001 - retry any transient failure
            print("attempt %d: %s: %s" % (attempt + 1, type(e).__name__, e), flush=True)
            time.sleep(5)
    if sock is None:
        print("FAILED: could not authenticate with client certificate")
        sys.exit(1)
    print("[ok] STARTTLS + mTLS + certificate auth (CN=%s), %s" % (USER, sock.version()))

    send(sock, MSG_PING)
    expect(sock, MSG_PONG, "ping")
    print("[ok] ping")

    value = ("written-over-mtls-%d" % time.time()).encode()
    content = field(1, value) + field(2, b"text/plain")
    send(sock, MSG_PUT, field(1, b"pb-smoke") + field(2, b"hello") + field(4, content))
    expect(sock, MSG_PUT_RESP, "put")
    print("[ok] PUT pb-smoke/hello (bucket writable with granted permissions)")

    send(sock, MSG_GET, field(1, b"pb-smoke") + field(2, b"hello"))
    payload = expect(sock, MSG_GET_RESP, "get")
    got = None
    for num, v in fields(payload):
        if num == 1:  # RpbContent
            got = next((cv for cn, cv in fields(v) if cn == 1), None)
    if got != value:
        print("FAILED: GET mismatch: %r != %r" % (got, value))
        sys.exit(1)
    print("[ok] GET roundtrip returned identical value")
    sock.close()

    try:
        connect(with_cert=False, username=USER)
        print("FAILED: auth succeeded without a client certificate")
        sys.exit(1)
    except Exception as e:  # noqa: BLE001 - any rejection is a pass here
        print("[ok] no-certificate auth rejected: %s" % e)

    print("ALL CHECKS PASSED")


if __name__ == "__main__":
    main()
