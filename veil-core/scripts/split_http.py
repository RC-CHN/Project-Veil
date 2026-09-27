#!/usr/bin/env python3
"""Owned split-HTTPS experiment. No production configuration or remote hosts.

Captures use one monotonic clock for all connections, retaining the information
needed to merge both directions across TCP connections. TLS ciphertext stays in
memory; only lengths, directions and timestamps are saved.
"""

import argparse
import base64
import contextlib
import hashlib
import http.server
import json
import os
import pathlib
import random
import shutil
import signal
import ssl
import statistics
import subprocess
import threading
import time

from benchmark import Perf, line, rss
from browser_shapes import CaptureGroup
from build import ROOT
from fixtures import configurations, fixture, port, save, spawn, stop, wait_port
from reality_fingerprint import records


def netem(p, delay):
    def tc(*args, check=True):
        subprocess.run(
            ["sudo", "-n", "tc", *map(str, args)],
            check=check,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    tc("qdisc", "del", "dev", "lo", "root", check=False)
    if not delay:
        return
    tc("qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio")
    tc(
        "qdisc",
        "add",
        "dev",
        "lo",
        "parent",
        "1:3",
        "handle",
        "30:",
        "netem",
        "delay",
        f"{delay}ms",
        "limit",
        "10000",
    )
    for direction in ("sport", "dport"):
        tc(
            "filter",
            "add",
            "dev",
            "lo",
            "protocol",
            "ip",
            "parent",
            "1:",
            "prio",
            "1",
            "u32",
            "match",
            "ip",
            direction,
            p,
            "0xffff",
            "flowid",
            "1:3",
        )


class Processes:
    def __init__(self, folder, cleanup):
        self.folder, self.cleanup, self.procs = folder, cleanup, []

    def launch(self, cmd, cpu=None, **kw):
        log = self.cleanup.enter_context(
            open(self.folder / f"process-{len(self.procs)}.log", "w")
        )
        p = spawn(
            cmd,
            str(cpu) if cpu is not None else None,
            1,
            stderr=log,
            **({"stdout": log} | kw),
        )
        self.cleanup.callback(stop, p)
        self.procs.append(p)
        return p


def proxy(f, group, ps, tap=False, delay=0):
    sp, cp = port(), port()
    if group == "veil":
        sc, cc = configurations(f, "veil-opt-tls", sp, cp, 0)
        # Same Go AES and TLS batch overlays as the HTTP/2 fixture on both ends.
        sc[0] = ROOT / ".build/veil-batch"
        for role in ("server", "client"):
            path = f["folder"] / (role + ".json")
            cfg = json.loads(path.read_text())
            cfg["tls"]["record_padding"] = True
            save(path, cfg)
    else:
        common = dict(
            Mode=group,
            Secret=f["keys"]["secret"],
            Certificate=f["cert"],
            PrivateKey=f["key"],
            CAFile=f["cert"],
        )
        sc = [
            ROOT / ".build/splithttp",
            "-config",
            save(
                f["folder"] / "server.json",
                common | dict(Role="server", Listen=f"127.0.0.1:{sp}"),
            ),
        ]
        cc = [
            ROOT / ".build/splithttp",
            "-config",
            save(
                f["folder"] / "client.json",
                common
                | dict(
                    Role="client", Listen=f"127.0.0.1:{cp}", Server=f"127.0.0.1:{sp}"
                ),
            ),
        ]
    server = ps.launch(sc, 14)
    wait_port(sp, server)
    netem(sp, delay)
    ps.cleanup.callback(netem, sp, 0)
    capture = None
    if tap:
        capture = CaptureGroup(("127.0.0.1", sp))
        ps.cleanup.callback(capture.close)
        path = f["folder"] / "client.json"
        cfg = json.loads(path.read_text())
        cfg["server" if group == "veil" else "Server"] = f"127.0.0.1:{capture.port}"
        save(path, cfg)
    client = ps.launch(cc, 22)
    wait_port(cp, client)
    return cp, server, client, capture


def trace(tap):
    if tap.errors:
        raise RuntimeError("capture errors: " + repr(tap.errors))
    result = []
    for index, c in enumerate(tap.connections):
        offsets = {d: [] for d in ("up", "down")}
        positions = {d: 0 for d in offsets}
        for e in sorted(c.events, key=lambda x: x["time"]):
            d = e["direction"]
            positions[d] += e["bytes"]
            offsets[d].append((positions[d], e["time"]))
        for direction, data in c.wire.items():
            offset, event = 0, 0
            for typ, payload in records(bytes(data)):
                offset += 5 + len(payload)
                while offsets[direction][event][0] < offset:
                    event += 1
                result.append(
                    dict(
                        connection=index,
                        direction=direction,
                        type=typ,
                        bytes=5 + len(payload),
                        time=offsets[direction][event][1],
                    )
                )
    return sorted(result, key=lambda x: x["time"])


def describe(events):
    # Start at each TLS 1.3 client's first encrypted record (Finished included).
    # Counts include HTTP/2 control traffic; they are not handshake RTT counts.
    finished = {}
    for e in events:
        if e["direction"] == "up" and e["type"] == 23:
            finished.setdefault(e["connection"], e["time"])
    app = [
        e
        for e in events
        if e["type"] == 23 and e["time"] >= finished.get(e["connection"], float("inf"))
    ]
    bursts = []
    for e in app:
        sign = 1 if e["direction"] == "up" else -1
        if bursts and (bursts[-1] > 0) == (sign > 0):
            bursts[-1] += sign * e["bytes"]
        else:
            bursts.append(sign * e["bytes"])
    total = sum(e["bytes"] for e in app)
    up = sum(e["bytes"] for e in app if e["direction"] == "up")
    return dict(
        bursts=bursts,
        turns=max(0, len(bursts) - 1),
        up=up,
        down=total - up,
        duration_ms=(app[-1]["time"] - app[0]["time"]) * 1000 if app else 0,
        directional_share=max(up, total - up) / total if total else 0,
    )


def browser_trial(f, args, group, website, spki, protocol, parallel, seed, folder):
    folder.mkdir()
    with contextlib.ExitStack() as cleanup:
        ps = Processes(folder, cleanup)
        # A second owned tap supplies ground-truth inner TLS timing. It is never
        # supplied to the observer's merged/per-connection feature extraction.
        inner = CaptureGroup(("127.0.0.1", website))
        cleanup.callback(inner.close)
        endpoint = inner.port
        proxy_args = []
        if group == "https":
            netem(inner.port, args.delay_ms)
            cleanup.callback(netem, inner.port, 0)
            outer = CaptureGroup(("127.0.0.1", inner.port))
            cleanup.callback(outer.close)
            endpoint = outer.port
        else:
            cp, _, _, outer = proxy(f, group, ps, True, args.delay_ms)
            proxy_args = ["--proxy", f"127.0.0.1:{cp}"]
        cleanup.callback(shutil.rmtree, folder / "profile", ignore_errors=True)
        cmd = [
            "node",
            ROOT / "scripts/browser_peer.mjs",
            "--role",
            "browser",
            "--browser",
            args.browser,
            "--profile",
            folder / "profile",
            "--log",
            folder / "browser.log",
            "--spki",
            spki,
            "--url",
            f"https://localhost:{endpoint}/run?parallel={int(parallel)}&seed={seed}",
            *proxy_args,
        ]
        driver = subprocess.Popen(
            list(map(str, cmd)),
            stdout=subprocess.PIPE,
            text=True,
            start_new_session=True,
        )
        try:
            data, _ = driver.communicate(timeout=35)
            if driver.returncode:
                raise RuntimeError("browser failed")
            browser = json.loads(data)
        finally:
            try:
                os.killpg(driver.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            driver.wait()
        for p in reversed(ps.procs):
            stop(p)
        outer.close()
        inner.close()
        events, ground_truth = trace(outer), trace(inner)
        if not events or not ground_truth:
            raise RuntimeError("traffic bypassed measured path")
        count = len({e["connection"] for e in events})
        if group in ("shared", "split") and count != (1 if group == "shared" else 2):
            raise RuntimeError(f"unexpected {group} physical connection count: {count}")
        expected = "h2" if protocol == "h2" else "http/1.1"
        if any(r["protocol"] != expected for r in browser["responses"]):
            raise RuntimeError("wrong browser HTTP protocol")
        merged = describe(events)
        separate = [
            describe([e for e in events if e["connection"] == c])
            for c in sorted({e["connection"] for e in events})
        ]
        origin = min(events[0]["time"], ground_truth[0]["time"])
        for e in events + ground_truth:
            e["time"] = round((e["time"] - origin) * 1000, 4)
        return dict(
            group=group,
            protocol=protocol,
            parallel=parallel,
            seed=seed,
            browser=browser,
            merged=merged,
            separate=separate,
            events=events,
            inner=ground_truth,
        )


def shapes(f, args):
    pub = subprocess.check_output(
        ["openssl", "x509", "-in", f["cert"], "-pubkey", "-noout"]
    )
    der = subprocess.check_output(
        ["openssl", "pkey", "-pubin", "-outform", "DER"], input=pub
    )
    spki = base64.b64encode(hashlib.sha256(der).digest()).decode()
    rows, rng = [], random.Random(928)
    for protocol in ("h2", "h1-close"):
        with contextlib.ExitStack() as cleanup:
            folder = args.out / (protocol + "-origin")
            folder.mkdir()
            ps = Processes(folder, cleanup)
            wp = port()
            server = ps.launch(
                [
                    "node",
                    ROOT / "scripts/browser_peer.mjs",
                    "--role",
                    "server",
                    "--cert",
                    f["cert"],
                    "--key",
                    f["key"],
                    "--port",
                    wp,
                    "--protocol",
                    protocol,
                ]
            )
            wait_port(wp, server)
            for seed in range(1, args.samples + 1):
                for parallel in (False, True):
                    order = ["https", "veil", "shared", "split"]
                    rng.shuffle(order)
                    for group in order:
                        folder = args.out / f"{protocol}-{seed}-{int(parallel)}-{group}"
                        row = browser_trial(
                            f, args, group, wp, spki, protocol, parallel, seed, folder
                        )
                        rows.append(row)
                        (args.out / "samples.json").write_text(
                            json.dumps(rows, indent=2) + "\n"
                        )
                        print(
                            folder.name,
                            "connections",
                            len(row["separate"]),
                            "merged turns",
                            row["merged"]["turns"],
                            flush=True,
                        )
    return rows


def performance(f, args):
    rows, rng = [], random.Random(928)
    for rep in range(args.samples):
        for mode in ("U", "D", "E", "S", "C"):
            order = ["veil", "shared", "split"]
            rng.shuffle(order)
            for group in order:
                folder = args.out / f"{rep}-{mode}-{group}"
                folder.mkdir()
                with contextlib.ExitStack() as cleanup:
                    ps = Processes(folder, cleanup)
                    bp = port()
                    backend = ps.launch(
                        [
                            ROOT / ".build/benchpeer",
                            "-role",
                            "backend",
                            "-listen",
                            f"127.0.0.1:{bp}",
                        ],
                        17,
                    )
                    wait_port(bp, backend)
                    cp, server, client, _ = proxy(f, group, ps, False, args.delay_ms)
                    load = ps.launch(
                        [
                            ROOT / ".build/benchpeer",
                            "-socks",
                            f"127.0.0.1:{cp}",
                            "-target",
                            f"127.0.0.1:{bp}",
                            "-mode",
                            mode,
                            "-connections",
                            1,
                            "-bytes",
                            args.bytes,
                            "-rounds",
                            1 if mode == "C" else args.rounds,
                        ],
                        18,
                        stdin=subprocess.PIPE,
                        stdout=subprocess.PIPE,
                        text=True,
                    )
                    if not line(load).get("ready"):
                        raise RuntimeError("load not ready")
                    counters = []
                    try:
                        for name, proc in (("server", server), ("client", client)):
                            d = folder / name
                            d.mkdir()
                            counter = Perf(proc.pid, d)
                            counters.append((name, counter))
                            counter.command("enable")
                        load.stdin.write("go\n")
                        load.stdin.flush()
                        row = line(load)
                        for name, counter in counters:
                            counter.command("disable")
                        row["rss"] = dict(
                            server=rss(server.pid), client=rss(client.pid)
                        )
                    finally:
                        for name, counter in counters:
                            counter.close()
                    for name, counter in counters:
                        row[name + "_perf"] = counter.values()
                    if load.wait(timeout=5):
                        raise RuntimeError("load failed")
                    row.update(
                        group=group,
                        pair=rep,
                        cpu_ms=sum(
                            row[n + "_perf"]["task-clock"] for n in ("server", "client")
                        ),
                    )
                    row["efficiency"] = (
                        row.get("requests", row["bytes"]) / row["cpu_ms"]
                    )
                    rows.append(row)
                    (args.out / "samples.json").write_text(
                        json.dumps(rows, indent=2) + "\n"
                    )
                    print(
                        rep,
                        mode,
                        group,
                        round(row["seconds"], 3),
                        "cpu_ms",
                        row["cpu_ms"],
                        flush=True,
                    )
    return rows


def download(f, args):
    """Integrity/idle regression, not a throughput benchmark of the Python origin."""
    chunk = bytes(range(256)) * 512
    total = 64 << 20
    expected = hashlib.sha256(chunk * (total // len(chunk))).hexdigest()

    class Origin(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_GET(self):
            if self.path == "/redirect":
                self.send_response(302)
                self.send_header("Location", "/file")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            if self.path != "/file":
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Length", str(total))
            self.end_headers()
            for offset in range(0, total, len(chunk)):
                if offset == total // 2:
                    time.sleep(args.pause_seconds)
                self.wfile.write(chunk)

    rows = []
    with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Origin) as origin:
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.minimum_version = tls.maximum_version = ssl.TLSVersion.TLSv1_3
        tls.load_cert_chain(f["cert"], f["key"])
        tls.set_alpn_protocols(["http/1.1"])
        origin.socket = tls.wrap_socket(origin.socket, server_side=True)
        worker = threading.Thread(target=origin.serve_forever, daemon=True)
        worker.start()
        try:
            wp = origin.server_port
            for group in ("https", "veil", "shared", "split"):
                folder = args.out / group
                folder.mkdir()
                with contextlib.ExitStack() as cleanup:
                    proxy_args = []
                    if group == "https":
                        netem(wp, args.delay_ms)
                        cleanup.callback(netem, wp, 0)
                    else:
                        cp, _, _, _ = proxy(
                            f, group, Processes(folder, cleanup), delay=args.delay_ms
                        )
                        proxy_args = ["--proxy", f"socks5://127.0.0.1:{cp}"]
                    body = folder / "body.tmp"
                    cleanup.callback(body.unlink, missing_ok=True)
                    fields = (
                        "size_download",
                        "time_starttransfer",
                        "time_total",
                        "num_redirects",
                        "http_code",
                    )
                    output = subprocess.check_output(
                        [
                            "curl",
                            "--silent",
                            "--show-error",
                            "--fail",
                            "--location",
                            "--http1.1",
                            "--noproxy",
                            "",
                            "--proxy",
                            "",
                            *proxy_args,
                            "--cacert",
                            f["cert"],
                            "--resolve",
                            f"cover.test:{wp}:127.0.0.1",
                            "--max-time",
                            "120",
                            "--output",
                            str(body),
                            "--write-out",
                            "{"
                            + ",".join('"' + k + '":%{' + k + "}" for k in fields)
                            + "}",
                            f"https://cover.test:{wp}/redirect",
                        ],
                        text=True,
                        timeout=125,
                    )
                    row = json.loads(output)
                    with body.open("rb") as inp:
                        digest = hashlib.file_digest(inp, "sha256").hexdigest()
                    if (
                        digest != expected
                        or row["size_download"] != total
                        or row["num_redirects"] != 1
                        or row["http_code"] != 200
                    ):
                        raise RuntimeError("HTTPS download integrity/redirect failure")
                    row.update(
                        group=group, sha256=digest, pause_seconds=args.pause_seconds
                    )
                    rows.append(row)
                    (args.out / "samples.json").write_text(
                        json.dumps(rows, indent=2) + "\n"
                    )
                    print(
                        group, "64 MiB verified, seconds", row["time_total"], flush=True
                    )
        finally:
            origin.shutdown()
            worker.join()
    return rows


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("kind", choices=["shapes", "perf", "download"])
    p.add_argument("--out", required=True, type=pathlib.Path)
    p.add_argument("--browser", type=pathlib.Path)
    p.add_argument("--samples", type=int, default=8)
    p.add_argument("--delay-ms", type=float, default=0)
    p.add_argument("--bytes", type=int, default=1 << 30)
    p.add_argument("--rounds", type=int, default=1000)
    p.add_argument("--pause-seconds", type=float, default=30)
    args = p.parse_args()
    if os.environ.get("VEIL_ISOLATED_NETNS") != "1" or os.readlink(
        "/proc/self/ns/net"
    ) == os.readlink("/proc/1/ns/net"):
        p.error("requires a private network namespace")
    if (
        args.samples < 1
        or not 0 <= args.delay_ms <= 100
        or args.bytes < 1
        or args.rounds < 1
        or not 0 <= args.pause_seconds <= 60
        or (args.kind == "shapes" and args.browser is None)
    ):
        p.error("invalid experiment arguments")
    args.out = args.out.resolve()
    args.out.mkdir(parents=True, exist_ok=False)
    f = fixture(args.out / "fixture")
    metadata = dict(
        args={
            k: str(v) if isinstance(v, pathlib.Path) else v
            for k, v in vars(args).items()
        },
        kernel=os.uname().release,
        go=subprocess.check_output(["go", "version"], text=True).strip(),
        binaries={
            p.name: hashlib.sha256(p.read_bytes()).hexdigest()
            for p in (
                ROOT / ".build/splithttp",
                ROOT / ".build/veil-batch",
                ROOT / ".build/benchpeer",
            )
        },
    )
    (args.out / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    try:
        rows = {"shapes": shapes, "perf": performance, "download": download}[args.kind](
            f, args
        )
        if args.kind == "download":
            return
        summary = []
        for group in ("https", "veil", "shared", "split"):
            for mode in (
                ("h2", "h1-close")
                if args.kind == "shapes"
                else ("U", "D", "E", "S", "C")
            ):
                ss = [
                    r
                    for r in rows
                    if r["group"] == group and r.get("protocol", r.get("mode")) == mode
                ]
                if ss:
                    metrics = (
                        {
                            "merged_turns": [r["merged"]["turns"] for r in ss],
                            "ttfb_ms": [
                                r["browser"]["navigation"]["ttfb_ms"] for r in ss
                            ],
                        }
                        if args.kind == "shapes"
                        else {
                            k: [r[k] for r in ss]
                            for k in ("seconds", "cpu_ms", "efficiency")
                        }
                    )
                    summary.append(
                        dict(
                            group=group,
                            mode=mode,
                            count=len(ss),
                            median={
                                k: statistics.median(v) for k, v in metrics.items()
                            },
                        )
                    )
        (args.out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    finally:
        netem(0, 0)
        shutil.rmtree(f["folder"])


if __name__ == "__main__":
    main()
