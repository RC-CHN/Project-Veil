#!/usr/bin/env python3
"""Same Chrome workload, direct, proxied, and on unauthenticated REALITY fallback.

Private network namespace required. No user profile, external website or deployed
proxy is used. Records describe userspace forwarding, not TCP packet boundaries.
"""

import argparse
import base64
import contextlib
import hashlib
import json
import os
import pathlib
import random
import re
import shutil
import signal
import socket
import statistics
import subprocess
import threading
import time
from types import SimpleNamespace

from build import ROOT
from fixtures import configurations, fixture, port, spawn, stop, wait_port
from traffic_shapes import shapes


class CaptureGroup:
    """Capture every physical connection, including browser preconnects."""

    def __init__(self, dest, delay=0):
        self.dest, self.delay = dest, delay
        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen()
        self.listener.settimeout(0.1)
        self.port = self.listener.getsockname()[1]
        self.stopped = threading.Event()
        self.connections, self.workers = [], []
        self.errors = []
        self.acceptor = threading.Thread(target=self.accept, daemon=True)
        self.acceptor.start()

    def accept(self):
        while not self.stopped.is_set():
            try:
                local, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            try:
                remote = socket.create_connection(self.dest, timeout=3)
            except OSError as e:
                local.close()
                self.errors.append(str(e))
                return
            remote.settimeout(None)
            for c in (local, remote):
                c.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            item = SimpleNamespace(
                wire={"up": bytearray(), "down": bytearray()},
                events=[],
                ends={},
                sockets=(local, remote),
            )
            self.connections.append(item)
            for src, dst, direction in ((local, remote, "up"), (remote, local, "down")):
                worker = threading.Thread(
                    target=self.pump, args=(item, src, dst, direction), daemon=True
                )
                self.workers.append(worker)
                worker.start()

    def pump(self, item, src, dst, direction):
        outcome = "eof"
        try:
            while True:
                b = src.recv(131072)
                if not b:
                    break
                if len(item.wire[direction]) + len(b) > 4 << 20:
                    raise ValueError("fixture capture limit")
                item.wire[direction].extend(b)
                item.events.append(
                    {"direction": direction, "bytes": len(b), "time": time.monotonic()}
                )
                if self.delay:
                    time.sleep(self.delay)
                dst.sendall(b)
        except OSError as e:
            outcome = "reset" if isinstance(e, ConnectionResetError) else "socket-error"
        except ValueError as e:
            self.errors.append(str(e))
            outcome = "capture-error"
        finally:
            item.ends[direction] = outcome
            try:
                dst.shutdown(socket.SHUT_WR)
            except OSError:
                pass

    def close(self):
        if self.stopped.is_set():
            return
        self.stopped.set()
        self.listener.close()
        self.acceptor.join(timeout=4)
        for worker in self.workers:
            worker.join(timeout=1)
        for item in self.connections:
            for c in item.sockets:
                c.close()
        for worker in self.workers:
            worker.join(timeout=4)
        if self.acceptor.is_alive() or any(w.is_alive() for w in self.workers):
            raise RuntimeError("capture did not stop")

    def result(self):
        if self.errors:
            raise RuntimeError("capture failed: " + "; ".join(self.errors))
        return [
            shapes(item) | {"ends": item.ends}
            for item in self.connections
            if any(item.wire.values())
        ]


def trial(f, browser, spki, group, binaries, website, parallel, folder, delay):
    folder.mkdir()
    with contextlib.ExitStack() as cleanup:
        cleanup.callback(shutil.rmtree, folder / "profile", ignore_errors=True)
        procs = []

        def launch(command):
            log = cleanup.enter_context(open(folder / f"process-{len(procs)}.log", "w"))
            p = spawn(command, stdout=log, stderr=log)
            cleanup.callback(stop, p)
            procs.append(p)
            return p

        proxy = []
        if group == "https":
            tap = CaptureGroup(("127.0.0.1", website), delay)
            cleanup.callback(tap.close)
            endpoint = tap.port
        else:
            sp, cp = port(), port()
            sc, cc = configurations(f, "veil-opt-reality", sp, cp, website)
            for role in ("client", "server"):
                path = f["folder"] / (role + ".json")
                cfg = json.loads(path.read_text())
                cfg["tls"].update(server_name="localhost", record_padding=True)
                if role == "client":
                    cfg["tls"]["fingerprint"] = "chrome149"
                path.write_text(json.dumps(cfg))
            sc[0], cc[0] = binaries / "veil-openssl", binaries / "veil-batch"
            server = launch(sc)
            wait_port(sp, server)
            tap = CaptureGroup(("127.0.0.1", sp), delay)
            cleanup.callback(tap.close)
            if group == "fallback":
                endpoint = tap.port
            else:
                cfg = json.loads((f["folder"] / "client.json").read_text())
                cfg["server"] = f"127.0.0.1:{tap.port}"
                (f["folder"] / "client.json").write_text(json.dumps(cfg))
                client = launch(cc)
                wait_port(cp, client)
                endpoint = website
                proxy = ["--proxy", f"127.0.0.1:{cp}"]
        command = [
            "node",
            ROOT / "scripts/browser_peer.mjs",
            "--role",
            "browser",
            "--browser",
            browser,
            "--profile",
            folder / "profile",
            "--log",
            folder / "browser.log",
            "--spki",
            spki,
            "--url",
            f"https://localhost:{endpoint}/run?parallel={int(parallel)}",
            *proxy,
        ]
        # Chrome has subprocesses. Kill this owned process group on every exit,
        # including a stuck CDP driver, before removing its temporary profile.
        driver = subprocess.Popen(
            list(map(str, command)),
            stdout=subprocess.PIPE,
            text=True,
            start_new_session=True,
        )
        try:
            output, _ = driver.communicate(timeout=35)
            if driver.returncode:
                raise RuntimeError(f"browser driver exited {driver.returncode}")
            result = json.loads(output)
        finally:
            try:
                os.killpg(driver.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            driver.wait()
        for proc in reversed(procs):
            stop(proc)
        tap.close()
        captured = tap.result()
        if not captured:
            raise RuntimeError("browser traffic bypassed capture")
        stats = {}
        if group != "https":
            # Chrome may reset open keep-alive sockets on process exit. The
            # page verifies every body; completion counters need not be all FIN.
            roles = ("server",) if group == "fallback" else ("server", "client")
            for index, role in enumerate(roles):
                log = (folder / f"process-{index}.log").read_text()
                match = re.search(
                    r"stopped accepted=(\d+) rejected=(\d+) completed=(\d+) failed=(\d+)",
                    log,
                )
                if not match:
                    raise RuntimeError("missing proxy completion counters")
                stats[role] = dict(
                    zip(
                        ("accepted", "rejected", "completed", "failed"),
                        map(int, match.groups()),
                    )
                )
        return {"browser": result, "connections": captured, "proxy_stats": stats}


def summarize(rows):
    """Descriptive features, not a fitted or validated traffic classifier."""
    result = []
    for protocol, parallel, group in sorted(
        {(r["protocol"], r["parallel"], r["group"]) for r in rows}
    ):
        samples = [
            r
            for r in rows
            if (r["protocol"], r["parallel"], r["group"]) == (protocol, parallel, group)
        ]
        features = []
        for row in samples:
            connections = row["connections"]
            # Go/uTLS's MSS estimate produces 1186-byte steps with TLS 1.3
            # AEAD. This counts one specific pattern, not all TLS fingerprints.
            ramp_pairs = sum(
                a[0] == b[0] == 23
                and 4096 < a[1] < b[1] <= 16401
                and b[1] - a[1] == 1186
                for c in connections
                for records in c["records"].values()
                for a, b in zip(records, records[1:])
            )
            features.append(
                {
                    "connections": len(connections),
                    "bursts": sum(len(c["bursts"]) for c in connections),
                    "records": sum(
                        len(rs) for c in connections for rs in c["records"].values()
                    ),
                    "wire_bytes": sum(
                        sum(c["wire_bytes"].values()) for c in connections
                    ),
                    "ramp_pairs": ramp_pairs,
                }
            )
        result.append(
            {
                "protocol": protocol,
                "parallel": parallel,
                "group": group,
                "samples": len(samples),
                "samples_with_ramp": sum(f["ramp_pairs"] > 0 for f in features),
                "median": {
                    k: statistics.median(f[k] for f in features) for k in features[0]
                },
            }
        )
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--browser", required=True, type=pathlib.Path)
    p.add_argument("--before", required=True, type=pathlib.Path)
    p.add_argument("--after", type=pathlib.Path)
    p.add_argument("--out", required=True, type=pathlib.Path)
    p.add_argument("--samples", type=int, default=6)
    p.add_argument("--delay-ms", type=float, default=0)
    a = p.parse_args()
    if os.environ.get("VEIL_ISOLATED_NETNS") != "1" or os.readlink(
        "/proc/self/ns/net"
    ) == os.readlink("/proc/1/ns/net"):
        p.error("requires a private network namespace")
    if a.samples < 1 or not 0 <= a.delay_ms <= 100:
        p.error("invalid sample count or forwarding delay")
    a.out = a.out.resolve()
    a.out.mkdir(parents=True, exist_ok=False)
    f = fixture(a.out / "fixture")
    der = subprocess.check_output(
        ["openssl", "x509", "-in", f["cert"], "-pubkey", "-noout"]
    )
    der = subprocess.check_output(
        ["openssl", "pkey", "-pubin", "-outform", "DER"], input=der
    )
    spki = base64.b64encode(hashlib.sha256(der).digest()).decode()
    groups = {"https": None, "before": a.before.resolve()}
    if a.after:
        groups["after"] = a.after.resolve()
    groups["fallback"] = (a.after or a.before).resolve()
    rows, rng = [], random.Random(149)
    for protocol in ("h2", "h1-close"):
        wp = port()
        with open(a.out / f"website-{protocol}.log", "w") as log:
            server = spawn(
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
                    str(wp),
                    "--protocol",
                    protocol,
                ],
                stdout=log,
                stderr=log,
            )
            try:
                wait_port(wp, server)
                for sample in range(a.samples):
                    for parallel in (False, True):
                        order = list(groups)
                        rng.shuffle(order)
                        for group in order:
                            folder = (
                                a.out / f"{protocol}-{sample}-{int(parallel)}-{group}"
                            )
                            row = trial(
                                f,
                                a.browser,
                                spki,
                                group,
                                groups[group],
                                wp,
                                parallel,
                                folder,
                                a.delay_ms / 1000,
                            )
                            row.update(
                                protocol=protocol,
                                sample=sample,
                                parallel=parallel,
                                group=group,
                            )
                            expected = "h2" if protocol == "h2" else "http/1.1"
                            if any(
                                r["protocol"] != expected
                                for r in row["browser"]["responses"]
                            ):
                                raise RuntimeError(
                                    "browser negotiated the wrong HTTP protocol"
                                )
                            rows.append(row)
                            (a.out / "samples.json").write_text(
                                json.dumps(rows, indent=2) + "\n"
                            )
                            print(
                                protocol,
                                sample,
                                parallel,
                                group,
                                "connections",
                                len(row["connections"]),
                                "bursts",
                                [len(c["bursts"]) for c in row["connections"]],
                                flush=True,
                            )
            finally:
                stop(server)
    metadata = {
        "scope": __doc__,
        "args": {
            k: str(v) if isinstance(v, pathlib.Path) else v for k, v in vars(a).items()
        },
        "browser_version": subprocess.check_output(
            [str(a.browser), "--version"], text=True
        ).strip(),
        "node": subprocess.check_output(["node", "--version"], text=True).strip(),
        "kernel": os.uname().release,
        "scripts": {
            p.name: hashlib.sha256(p.read_bytes()).hexdigest()
            for p in (pathlib.Path(__file__), ROOT / "scripts/browser_peer.mjs")
        },
        "binaries": {
            str(b): hashlib.sha256(b.read_bytes()).hexdigest()
            for d in groups.values()
            if d
            for b in (d / "veil-batch", d / "veil-openssl")
        },
    }
    (a.out / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    (a.out / "summary.json").write_text(json.dumps(summarize(rows), indent=2) + "\n")


if __name__ == "__main__":
    main()
