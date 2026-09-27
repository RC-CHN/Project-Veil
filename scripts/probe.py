#!/usr/bin/env python3
"""Local differential smoke checks, not a claim of resistance to censorship.
Captures one owned REALITY session, replays only its ClientHello to owned
listeners, and compares normal TLS, malformed input and cover failure.
"""

import argparse
import collections
import hashlib
import json
import socket
import ssl
import subprocess
import threading
import time

from build import ROOT
from fixtures import configurations, fixture, port, spawn, stop, wait_port


def tls_fetch(addr, cert):
    ctx = ssl.create_default_context(cafile=cert)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    ctx.maximum_version = ssl.TLSVersion.TLSv1_3
    ctx.set_alpn_protocols(["http/1.1"])
    with socket.create_connection(addr, timeout=3) as raw:
        with ctx.wrap_socket(raw, server_hostname="cover.test") as c:
            der = c.getpeercert(binary_form=True)
            cipher = c.cipher()
            c.sendall(
                b"GET / HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\n\r\n"
            )
            data = b""
            while b"\r\n\r\n" not in data and len(data) < 65536:
                part = c.recv(4096)
                if not part:
                    break
                data += part
            return {
                "certificate_sha256": hashlib.sha256(der).hexdigest(),
                "version": c.version(),
                "cipher": cipher[0],
                "alpn": c.selected_alpn_protocol(),
                "status": data.split(b"\r\n")[0].decode(errors="replace"),
            }


def sample(addr, payload, timeout=1.2):
    started = time.monotonic()
    data = b""
    outcome = "timeout"
    with socket.create_connection(addr, timeout=2) as c:
        c.settimeout(timeout)
        if payload:
            c.sendall(payload)
        while len(data) < 65536:
            try:
                p = c.recv(65536 - len(data))
                if not p:
                    outcome = "eof"
                    break
                data += p
            except socket.timeout:
                break
            except ConnectionResetError:
                outcome = "reset"
                break
    lengths = []
    i = 0
    while len(data) >= i + 5:
        n = int.from_bytes(data[i + 3 : i + 5], "big")
        if len(data) < i + 5 + n:
            break
        lengths.append({"type": data[i], "length": n})
        i += 5 + n
    return {
        "bytes": len(data),
        "records": lengths,
        "outcome": outcome,
        "seconds": round(time.monotonic() - started, 4),
    }


class Capture:
    def __init__(self, dest):
        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen()
        self.port = self.listener.getsockname()[1]
        self.dest = dest
        self.wire = {"up": bytearray(), "down": bytearray()}
        self.events = []
        self.pairs = []
        self.done = threading.Event()
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def run(self):
        try:
            a, _ = self.listener.accept()
            b = socket.create_connection(self.dest, timeout=3)
            self.pairs = [a, b]

            def pump(src, dst, direction):
                try:
                    while True:
                        data = src.recv(131072)
                        if not data:
                            break
                        if len(self.wire[direction]) + len(data) <= 16 << 20:
                            self.wire[direction].extend(data)
                        self.events.append(
                            {
                                "direction": direction,
                                "bytes": len(data),
                                "time": time.monotonic(),
                            }
                        )
                        dst.sendall(data)
                except OSError:
                    pass
                finally:
                    try:
                        dst.shutdown(socket.SHUT_WR)
                    except OSError:
                        pass

            threads = [
                threading.Thread(target=pump, args=(a, b, "up")),
                threading.Thread(target=pump, args=(b, a, "down")),
            ]
            for t in threads:
                t.start()
            for t in threads:
                t.join()
        finally:
            self.done.set()

    def close(self):
        self.listener.close()
        for c in self.pairs:
            c.close()
        self.thread.join(timeout=4)


def histogram(data):
    i = 0
    hist = collections.Counter()
    records = []
    while i + 5 <= len(data):
        n = int.from_bytes(data[i + 3 : i + 5], "big")
        if i + 5 + n > len(data):
            break
        typ = data[i]
        hist[f"{typ}:{n}"] += 1
        records.append([typ, n])
        i += 5 + n
    return {
        "record_histogram": dict(hist),
        "first_records": records[:24],
        "parsed_bytes": i,
        "captured_bytes": len(data),
    }


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--out", default=".build/probes")
    a = p.parse_args()
    folder = ROOT / a.out
    f = fixture(folder)
    procs = []
    logs = []
    capture = None

    def launch(cmd):
        log = open(folder / f"process-{len(procs)}.log", "w")
        logs.append(log)
        c = spawn(cmd, stdout=log, stderr=log)
        procs.append(c)
        return c

    try:
        coverp, sp, cp, bp = [port() for _ in range(4)]
        cover = launch(
            [
                "openssl",
                "s_server",
                "-accept",
                f"127.0.0.1:{coverp}",
                "-cert",
                f["cert"],
                "-key",
                f["key"],
                "-tls1_3",
                "-ciphersuites",
                "TLS_AES_128_GCM_SHA256",
                "-groups",
                "X25519",
                "-www",
                "-alpn",
                "http/1.1",
            ]
        )
        wait_port(coverp, cover)
        sc, cc = configurations(f, "veil-opt-reality", sp, cp, coverp)
        # Bound invalid-handshake tests to one second at the Veil edge.
        cfg = json.loads((folder / "server.json").read_text())
        cfg["handshake_seconds"] = 1
        (folder / "server.json").write_text(json.dumps(cfg))
        server = launch(sc)
        wait_port(sp, server)
        result = {
            "scope": "owned loopback only; differential observations, not anti-detection certification",
            "normal_tls": {
                "direct": tls_fetch(("127.0.0.1", coverp), f["cert"]),
                "via_veil": tls_fetch(("127.0.0.1", sp), f["cert"]),
            },
        }
        result["normal_tls"]["equal"] = (
            result["normal_tls"]["direct"] == result["normal_tls"]["via_veil"]
        )
        capture = Capture(("127.0.0.1", sp))
        cfg = json.loads((folder / "client.json").read_text())
        cfg["server"] = f"127.0.0.1:{capture.port}"
        (folder / "client.json").write_text(json.dumps(cfg))
        client = launch(cc)
        wait_port(cp, client)
        backend = launch(
            [
                ROOT / ".build/benchpeer",
                "-role",
                "backend",
                "-listen",
                f"127.0.0.1:{bp}",
            ]
        )
        wait_port(bp, backend)
        load = subprocess.run(
            [
                str(ROOT / ".build/benchpeer"),
                "-socks",
                f"127.0.0.1:{cp}",
                "-target",
                f"127.0.0.1:{bp}",
                "-mode",
                "E",
                "-rounds",
                "32",
                "-connections",
                "1",
            ],
            input="go\n",
            capture_output=True,
            text=True,
            timeout=10,
        )
        result["valid_session"] = {
            "returncode": load.returncode,
            "output": load.stdout,
            "error": load.stderr,
        }
        if load.returncode:
            raise RuntimeError("valid session failed")
        stop(client)
        capture.done.wait(timeout=4)
        capture.close()
        captured = bytes(capture.wire["up"])
        hello = captured[: 5 + int.from_bytes(captured[3:5], "big")]
        if len(hello) < 71 or hello[0] != 22:
            raise RuntimeError("no captured ClientHello")
        result["record_shapes"] = {
            d: histogram(bytes(data)) for d, data in capture.wire.items()
        }
        result["read_chunks"] = capture.events
        changed = bytearray(hello)
        changed[44] ^= 1
        inputs = {
            "empty": b"",
            "truncated_record": b"\x16\x03\x01\x00",
            "invalid_header": b"not a TLS ClientHello",
            "captured_reality_hello": hello,
            "modified_reality_auth": bytes(changed),
        }
        result["differential"] = {}
        for name, data in inputs.items():
            result["differential"][name] = {
                "direct": sample(("127.0.0.1", coverp), data),
                "via_veil": sample(("127.0.0.1", sp), data),
            }
        stop(cover)
        result["cover_unavailable"] = sample(("127.0.0.1", sp), hello)
        (folder / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        print(
            json.dumps(
                {
                    k: v
                    for k, v in result.items()
                    if k not in ("read_chunks", "record_shapes")
                },
                indent=2,
            )
        )
    finally:
        if capture:
            capture.close()
        for c in reversed(procs):
            stop(c)
        for log in logs:
            log.close()
