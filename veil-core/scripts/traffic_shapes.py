#!/usr/bin/env python3
"""Owned HTTPS vs Veil TLS-record shapes, including nested TLS and pool reuse.

This is descriptive measurement, not a GFW classifier. A userspace relay records
TLS lengths and forwarding times, not TCP packets or browser behavior. No raw
traffic, credentials or application bytes are written to the result.
"""

import argparse
import collections
import contextlib
import hashlib
import http.client
import http.server
import json
import os
import pathlib
import socket
import ssl
import statistics
import threading
import time

from fixtures import configurations, fixture, port, spawn, stop, wait_port
from probe import Capture
from reality_fingerprint import records


class Website(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        size = int(self.path[1:])
        if not 0 <= size <= 1 << 20:
            self.send_error(400)
            return
        self.send_response(200)
        self.send_header("Content-Length", str(size))
        self.end_headers()
        self.wfile.write(b"x" * size)

    def log_message(self, *_):
        pass


def receive(c, size):
    data = b""
    while len(data) < size:
        b = c.recv(size - len(data))
        if not b:
            raise EOFError("SOCKS handshake truncated")
        data += b
    return data


def connect(context, endpoint, target=None):
    c = socket.create_connection(endpoint, timeout=5)
    try:
        if target:
            c.sendall(b"\x05\x01\x00")
            if receive(c, 2) != b"\x05\x00":
                raise ValueError("SOCKS authentication")
            c.sendall(
                b"\x05\x01\x00\x01"
                + socket.inet_aton(target[0])
                + target[1].to_bytes(2, "big")
            )
            if receive(c, 10)[:4] != b"\x05\x00\x00\x01":
                raise ValueError("SOCKS open failed")
        return context.wrap_socket(c, server_hostname="cover.test")
    except BaseException:
        c.close()
        raise


def fetch(c, size, close=False):
    connection = "close" if close else "keep-alive"
    c.sendall(
        f"GET /{size} HTTP/1.1\r\nHost: cover.test\r\nConnection: {connection}\r\n\r\n".encode()
    )
    response = http.client.HTTPResponse(c)
    response.begin()
    data = response.read()
    if response.status != 200 or data != b"x" * size:
        raise ValueError("HTTPS body mismatch")


def shapes(tap):
    parsed = {d: records(bytes(b)) for d, b in tap.wire.items()}
    events = tap.events
    # TCP coalescing and thread scheduling affect these forwarding bursts.
    bursts = []
    for e in events:
        if bursts and bursts[-1]["direction"] == e["direction"]:
            bursts[-1]["bytes"] += e["bytes"]
        else:
            bursts.append(
                {"direction": e["direction"], "bytes": e["bytes"], "time": e["time"]}
            )
    start = events[0]["time"] if events else 0
    for b in bursts:
        b["time_ms"] = round((b.pop("time") - start) * 1000, 3)
    return {
        "records": {d: [[t, len(p)] for t, p in rs] for d, rs in parsed.items()},
        "wire_bytes": {d: len(b) for d, b in tap.wire.items()},
        "bursts": bursts,
    }


def trial(f, group, binaries, cover, context, sizes, folder):
    folder.mkdir()
    with contextlib.ExitStack() as cleanup:
        procs = []

        def launch(command):
            log = cleanup.enter_context(open(folder / f"process-{len(procs)}.log", "w"))
            p = spawn(command, stdout=log, stderr=log)
            cleanup.callback(stop, p)
            procs.append(p)
            return p

        if group == "https":
            tap = Capture(cover)
            cleanup.callback(tap.close)
            with connect(context, ("127.0.0.1", tap.port)) as c:
                for i, size in enumerate(sizes):
                    fetch(c, size, i == len(sizes) - 1)
        else:
            sp, cp = port(), port()
            sc, cc = configurations(f, "veil-opt-reality", sp, cp, cover[1])
            for role in ("client", "server"):
                path = f["folder"] / (role + ".json")
                cfg = json.loads(path.read_text())
                cfg["tls"]["record_padding"] = True
                cfg["max_idle"] = 1
                if role == "client":
                    cfg["tls"]["fingerprint"] = "chrome149"
                path.write_text(json.dumps(cfg))
            sc[0], cc[0] = binaries / "veil-openssl", binaries / "veil-batch"
            server = launch(sc)
            wait_port(sp, server)
            tap = Capture(("127.0.0.1", sp))
            cleanup.callback(tap.close)
            cfg = json.loads((f["folder"] / "client.json").read_text())
            cfg["server"] = f"127.0.0.1:{tap.port}"
            (f["folder"] / "client.json").write_text(json.dumps(cfg))
            client = launch(cc)
            wait_port(cp, client)
            for size in sizes:
                with connect(context, ("127.0.0.1", cp), cover) as c:
                    fetch(c, size, True)
                time.sleep(0.02)  # harness gap to allow FIN/DONE and pool return
            stop(client)
            stop(server)
            log = (folder / "process-0.log").read_text()
            if f"completed={len(sizes)}" not in log or "accepted=2 " not in log:
                raise ValueError(
                    "expected one physical tunnel and complete stream reuse: " + log
                )
        if not tap.done.wait(5):
            raise TimeoutError("capture did not close")
        return shapes(tap)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--before", required=True)
    p.add_argument("--after", default=".build")
    p.add_argument("--out", required=True)
    p.add_argument("--samples", type=int, default=12)
    a = p.parse_args()
    if os.environ.get("VEIL_ISOLATED_NETNS") != "1" or os.readlink(
        "/proc/self/ns/net"
    ) == os.readlink("/proc/1/ns/net"):
        p.error("requires a private network namespace")
    if a.samples < 2:
        p.error("at least two samples required")
    out = pathlib.Path(a.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    f = fixture(out / "fixture")
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.minimum_version = tls.maximum_version = ssl.TLSVersion.TLSv1_3
    tls.load_cert_chain(f["cert"], f["key"])
    tls.set_ecdh_curve("X25519")
    tls.set_alpn_protocols(["http/1.1"])
    website = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Website)
    website.socket = tls.wrap_socket(website.socket, server_side=True)
    thread = threading.Thread(target=website.serve_forever, daemon=True)
    thread.start()
    context = ssl.create_default_context(cafile=f["cert"])
    context.minimum_version = context.maximum_version = ssl.TLSVersion.TLSv1_3
    context.set_alpn_protocols(["http/1.1"])
    before, after = pathlib.Path(a.before).resolve(), pathlib.Path(a.after).resolve()
    rows = []
    try:
        for sample in range(a.samples):
            for workload, sizes in (
                ("fresh", [64]),
                ("reuse", [64, 4096, 32768, 64, 4096, 32768]),
            ):
                for group, binaries in (
                    ("https", None),
                    ("before", before),
                    ("after", after),
                ):
                    row = trial(
                        f,
                        group,
                        binaries,
                        website.server_address,
                        context,
                        sizes,
                        out / f"{sample}-{workload}-{group}",
                    )
                    row.update(
                        sample=sample,
                        workload=workload,
                        group=group,
                        body_bytes=sum(sizes),
                    )
                    rows.append(row)
            print(f"{sample + 1}/{a.samples}", flush=True)
    finally:
        website.shutdown()
        website.server_close()
        thread.join()
    summary = {}
    for workload in ("fresh", "reuse"):
        summary[workload] = {}
        for group in ("https", "before", "after"):
            selected = [
                r for r in rows if r["workload"] == workload and r["group"] == group
            ]
            summary[workload][group] = {
                "median_wire_bytes": statistics.median(
                    sum(r["wire_bytes"].values()) for r in selected
                ),
                "median_bursts": statistics.median(len(r["bursts"]) for r in selected),
                "application_record_lengths": {
                    d: dict(
                        collections.Counter(
                            str(n)
                            for r in selected
                            for t, n in r["records"][d]
                            if t == 23
                        )
                    )
                    for d in ("up", "down")
                },
            }
    metadata = {
        "scope": "owned loopback; Python/OpenSSL HTTPS reference; encrypted handshake records included in type 23; no browser matching or censorship classification",
        "reuse": "HTTPS reference reuses one TLS connection; Veil reuses one outer connection for six independent inner TLS connections; 20ms harness gaps are not protocol delays",
        "binaries": {
            str(b): hashlib.sha256(b.read_bytes()).hexdigest()
            for root in (before, after)
            for b in (root / "veil-batch", root / "veil-openssl")
        },
    }
    (out / "result.json").write_text(
        json.dumps(
            {"metadata": metadata, "summary": summary, "samples": rows}, indent=2
        )
        + "\n"
    )
    print(
        json.dumps(
            {
                k: {
                    g: {m: v for m, v in r.items() if m != "application_record_lengths"}
                    for g, r in groups.items()
                }
                for k, groups in summary.items()
            },
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
