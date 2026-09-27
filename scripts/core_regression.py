#!/usr/bin/env python3
"""Isolated old/new v0.1 interop and paired CPU/performance regression.

Requires a private network namespace, Linux perf and owned loopback fixtures.
Both proxies get one CPU and GOMAXPROCS=1; no deployed paths are touched.
"""

import argparse
import hashlib
import json
import os
import pathlib
import random
import re
import statistics
import subprocess

from benchmark import Perf, line, rss
from fixtures import configurations, fixture, port, spawn, stop, wait_port


def trial(f, binaries, peer, mode, size, rounds, folder, cores, padding):
    folder.mkdir()
    procs, logs, counters = [], [], []

    def launch(cmd, cpu, **kw):
        log = open(folder / f"process-{len(procs)}.log", "w")
        logs.append(log)
        proc = spawn(cmd, str(cpu), 1, stderr=log, **({"stdout": log} | kw))
        procs.append(proc)
        return proc

    try:
        coverp, bp, sp, cp = [port() for _ in range(4)]
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
            ],
            cores[2],
        )
        wait_port(coverp, cover)
        backend = launch(
            [peer, "-role", "backend", "-listen", f"127.0.0.1:{bp}"], cores[2]
        )
        wait_port(bp, backend)
        sc, cc = configurations(f, "veil-opt-reality", sp, cp, coverp)
        sc[0], cc[0] = binaries
        for name in ("client", "server"):
            path = f["folder"] / (name + ".json")
            cfg = json.loads(path.read_text())
            cfg["tls"]["record_padding"] = padding
            if name == "client":
                cfg["tls"]["fingerprint"] = "chrome149"
            path.write_text(json.dumps(cfg))
        server = launch(sc, cores[0])
        wait_port(sp, server)
        client = launch(cc, cores[1])
        wait_port(cp, client)
        load = launch(
            [
                peer,
                "-socks",
                f"127.0.0.1:{cp}",
                "-target",
                f"127.0.0.1:{bp}",
                "-mode",
                mode,
                "-connections",
                "1",
                "-bytes",
                str(size),
                "-rounds",
                str(rounds),
            ],
            cores[3],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            text=True,
        )
        if not line(load).get("ready"):
            raise RuntimeError("load not ready")
        for name, proc in (("server", server), ("client", client)):
            dest = folder / name
            dest.mkdir()
            counter = Perf(proc.pid, dest)
            counters.append(counter)
            counter.command("enable")
        load.stdin.write("go\n")
        load.stdin.flush()
        row = line(load)
        for counter in counters:
            counter.command("disable")
        row["server_rss_bytes"], row["client_rss_bytes"] = (
            rss(server.pid),
            rss(client.pid),
        )
        for name in ("server", "client"):
            counter = counters.pop(0)
            counter.close()
            row[name + "_perf"] = counter.values()
        load.stdin.close()
        if load.wait(timeout=5):
            raise RuntimeError("load failed")
        stop(client)
        stop(server)
        for log in logs:
            log.flush()
        for name, index in (("server", 2), ("client", 3)):
            match = re.search(
                r"stopped accepted=(\d+) rejected=(\d+) completed=(\d+) failed=(\d+)",
                (folder / f"process-{index}.log").read_text(),
            )
            if not match:
                raise RuntimeError("missing completion counters")
            row[name + "_stats"] = dict(
                zip(
                    ("accepted", "rejected", "completed", "failed"),
                    map(int, match.groups()),
                )
            )
        if mode == "S":
            for name in ("server", "client"):
                stats = row[name + "_stats"]
                if (
                    stats["completed"] != rounds + 16
                    or stats["rejected"]
                    or stats["failed"] != 1
                ):
                    raise RuntimeError("short-stream behavior changed: " + str(row))
        row["throughput"] = row.get("requests", row["bytes"]) / row["seconds"]
        row["cpu_ms"] = sum(
            row[n + "_perf"]["task-clock"] for n in ("client", "server")
        )
        row["work_per_cpu_ms"] = row.get("requests", row["bytes"]) / row["cpu_ms"]
        return row
    finally:
        for counter in counters:
            counter.close()
        for proc in reversed(procs):
            stop(proc)
        for log in logs:
            log.close()


def comparison(rows, modes):
    result = {}
    for mode in modes:
        pairs = {}
        for row in rows:
            if row["mode"] == mode and row["group"] in ("before", "after"):
                pairs.setdefault(row["pair"], {})[row["group"]] = row
        result[mode] = {}
        for metric in ("throughput", "work_per_cpu_ms", "p50_us", "p99_us"):
            deltas = [
                100 * (p["after"][metric] / p["before"][metric] - 1)
                for p in pairs.values()
                if len(p) == 2 and metric in p["before"]
            ]
            if deltas:
                result[mode][metric] = {
                    "paired_median_percent": statistics.median(deltas),
                    "pairs_percent": deltas,
                }
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument(
        "--before", required=True, help="directory with veil-client and veil-server"
    )
    p.add_argument(
        "--after", default=".build", help="directory with veil-batch and veil-openssl"
    )
    p.add_argument("--peer", default=".build/baseline/benchpeer")
    p.add_argument("--out", required=True)
    p.add_argument("--reps", type=int, default=7)
    p.add_argument("--bytes", type=int, default=1 << 30)
    p.add_argument("--rounds", type=int, default=10000)
    p.add_argument("--modes", default="U,D,E,S")
    p.add_argument(
        "--cores", default="14,22,17,18", help="server,client,backend,load CPU IDs"
    )
    p.add_argument("--no-padding", action="store_true")
    a = p.parse_args()
    if os.environ.get("VEIL_ISOLATED_NETNS") != "1" or os.readlink(
        "/proc/self/ns/net"
    ) == os.readlink("/proc/1/ns/net"):
        p.error("run inside a private network namespace with VEIL_ISOLATED_NETNS=1")
    cores = list(map(int, a.cores.split(",")))
    if (
        len(cores) != 4
        or len(set(cores)) != 4
        or not set(cores) <= os.sched_getaffinity(0)
    ):
        p.error("provide four distinct available CPU IDs")
    modes = a.modes.split(",")
    if (
        not set(modes) <= {"U", "D", "E", "S"}
        or a.reps < 1
        or a.rounds < 1
        or a.bytes < 1
    ):
        p.error("invalid workload")
    out = pathlib.Path(a.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    before, after, peer = (
        pathlib.Path(a.before).resolve(),
        pathlib.Path(a.after).resolve(),
        pathlib.Path(a.peer).resolve(),
    )
    old = (before / "veil-server", before / "veil-client")
    new = (after / "veil-openssl", after / "veil-batch")
    metadata = {
        "kernel": os.uname().release,
        "cpu": subprocess.check_output(["lscpu"], text=True),
        "go": subprocess.check_output(["go", "version"], text=True),
        "openssl": subprocess.check_output(["openssl", "version"], text=True),
        "script_sha256": hashlib.sha256(
            pathlib.Path(__file__).read_bytes()
        ).hexdigest(),
        "cores": cores,
        "gomaxprocs": 1,
        "args": vars(a),
        "binaries": {
            str(path): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in (*old, *new, peer)
        },
    }
    (out / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    f = fixture(out / "fixture")
    rows = []

    def run(group, binaries, mode, pair, rounds, size):
        row = trial(
            f,
            binaries,
            peer,
            mode,
            size,
            rounds,
            out / f"{len(rows):03d}-{group}-{mode}",
            cores,
            not a.no_padding,
        )
        row.update(group=group, pair=pair)
        rows.append(row)
        (out / "results.json").write_text(json.dumps(rows, indent=2) + "\n")
        print(json.dumps(row), flush=True)

    # Half-close, authentication and sequential pool reuse across both versions.
    for group, binaries in (
        ("old-client-new-server", (new[0], old[1])),
        ("new-client-old-server", (old[0], new[1])),
    ):
        run(group, binaries, "S", -1, 100, 0)
    rng = random.Random(149)
    for pair in range(a.reps):
        for mode in modes:
            groups = [("before", old), ("after", new)]
            rng.shuffle(groups)
            for group, binaries in groups:
                run(group, binaries, mode, pair, a.rounds, a.bytes)
    summary = comparison(rows, modes)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
