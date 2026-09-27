#!/usr/bin/env python3
"""Before/after cold-tunnel and short-stream CPU tests on owned loopback peers.
Server and client have one fixed CPU each. Both perf counters exclude warmup.
"""

import argparse
import hashlib
import itertools
import json
import random
import re
import subprocess

from benchmark import Perf, line, metadata
from build import ROOT
from fixtures import configurations, fixture, port, spawn, stop, wait_port


def trial(
    f,
    binaries,
    mode,
    rounds,
    folder,
    optimized=True,
    capture=None,
    client_tls=None,
    server_tls=None,
):
    folder.mkdir(parents=True)
    procs, logs, counters = [], [], []

    def launch(cmd, cores, **kw):
        log = open(folder / f"process-{len(procs)}.log", "w")
        logs.append(log)
        p = spawn(cmd, cores, 1, stderr=log, **({"stdout": log} | kw))
        procs.append(p)
        return p

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
            "16",
        )
        wait_port(coverp, cover)
        backend = launch(
            [
                ROOT / ".build/benchpeer",
                "-role",
                "backend",
                "-listen",
                f"127.0.0.1:{bp}",
            ],
            "17",
        )
        wait_port(bp, backend)
        variant = "veil-opt-reality" if optimized else "veil-native-reality"
        sc, cc = configurations(f, variant, sp, cp, coverp)
        for name, overrides in (("client", client_tls), ("server", server_tls)):
            if overrides:
                path = f["folder"] / (name + ".json")
                cfg = json.loads(path.read_text())
                cfg["tls"].update(overrides)
                path.write_text(json.dumps(cfg))
        sc[0], cc[0] = binaries / sc[0].name, binaries / cc[0].name
        server = launch(sc, "14")
        wait_port(sp, server)
        if capture is not None:
            tap = capture(("127.0.0.1", sp))
            cfg = json.loads((f["folder"] / "client.json").read_text())
            cfg["server"] = f"127.0.0.1:{tap.port}"
            (f["folder"] / "client.json").write_text(json.dumps(cfg))
        else:
            tap = None
        if mode == "H":
            cmd = [
                binaries
                / ("handshakebench-batch" if optimized else "handshakebench-native"),
                "-config",
                f["folder"] / "client.json",
                "-target",
                f"127.0.0.1:{bp}",
                "-rounds",
                str(rounds),
                "-warmup",
                "0" if tap else "16",
            ]
            load = launch(
                cmd, "22", stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True
            )
            client = load
        else:
            client = launch(cc, "22")
            wait_port(cp, client)
            load = launch(
                [
                    ROOT / ".build/benchpeer",
                    "-socks",
                    f"127.0.0.1:{cp}",
                    "-target",
                    f"127.0.0.1:{bp}",
                    "-mode",
                    mode,
                    "-connections",
                    "1",
                    "-rounds",
                    str(rounds),
                ],
                "18",
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                text=True,
            )
        if not line(load).get("ready"):
            raise RuntimeError("load not ready")
        if tap is None:
            for name, pid in (("server", server.pid), ("client", client.pid)):
                d = folder / name
                d.mkdir()
                counters.append(Perf(pid, d))
            for counter in counters:
                counter.command("enable")
        load.stdin.write("go\n")
        load.stdin.flush()
        row = line(load)
        for counter in counters:
            counter.command("disable")
        for name in ("server", "client") if counters else ():
            counter = counters.pop(0)
            counter.close()
            values = counter.values()
            row[name + "_perf"] = values
            row[name + "_cpu_us_per_request"] = (
                values["task-clock"] * 1000 / row["requests"]
            )
        load.stdin.close()
        load.wait(timeout=5)
        if load.returncode:
            raise RuntimeError("load failed")
        if tap:
            tap.done.wait(timeout=5)
            tap.close()
            row["capture"] = {d: bytes(b).hex() for d, b in tap.wire.items()}
        stop(client)
        stop(server)
        for log in logs:
            log.flush()
        # Physical tunnel count exposes pool misses in the short SOCKS workload.
        server_log = (folder / "process-2.log").read_text()
        match = re.search(
            r"stopped accepted=(\d+) rejected=(\d+) completed=(\d+) failed=(\d+)",
            server_log,
        )
        if not match:
            raise RuntimeError("missing server completion counters")
        row["server_stats"] = dict(
            zip(
                ("accepted", "rejected", "completed", "failed"),
                map(int, match.groups()),
            )
        )
        stats = row["server_stats"]
        if (
            stats["completed"] != rounds + (0 if tap else 16)
            or stats["rejected"]
            or stats["failed"] != 1
        ):
            # The single expected failure is the TCP-only listener-ready probe.
            raise RuntimeError("unexpected server counters: " + str(stats))
        row["requests_per_second"] = row["requests"] / row["seconds"]
        return row
    finally:
        for counter in counters:
            counter.close()
        for p in reversed(procs):
            stop(p)
        if capture is not None and "tap" in locals() and tap:
            tap.close()
        for log in logs:
            log.close()


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument(
        "--before",
        required=True,
        help="saved binary directory including handshakebench",
    )
    p.add_argument("--after", default=".build")
    p.add_argument("--out", required=True)
    p.add_argument("--modes", default="H,S")
    p.add_argument("--reps", type=int, default=7)
    p.add_argument("--rounds", type=int, default=3000)
    p.add_argument("--native", action="store_true")
    p.add_argument(
        "--fingerprint", help="candidate client template; baseline unchanged"
    )
    p.add_argument(
        "--record-padding", action="store_true", help="pad candidate peers only"
    )
    a = p.parse_args()
    client_tls, server_tls = {}, {}
    if a.fingerprint:
        client_tls["fingerprint"] = a.fingerprint
    if a.record_padding:
        client_tls["record_padding"] = server_tls["record_padding"] = True
    if (
        a.reps < 1
        or a.rounds < 1
        or any(m not in ("H", "S") for m in a.modes.split(","))
    ):
        p.error("invalid workload")
    folder = ROOT / a.out
    if folder.exists():
        raise SystemExit("use a fresh output directory")
    f = fixture(folder)
    binaries = {"before": ROOT / a.before, "after": ROOT / a.after}
    info = metadata(vars(a))
    info["cpu_affinity"] = {
        "server": 14,
        "client": 22,
        "cover": 16,
        "backend": 17,
        "SOCKS_load": 18,
    }
    info["comparison_binary_sha256"] = {
        version: {
            path.name: hashlib.sha256(path.read_bytes()).hexdigest()
            for pattern in ("veil-*", "handshakebench-*")
            for path in directory.glob(pattern)
            if path.is_file()
        }
        for version, directory in binaries.items()
    }
    (folder / "metadata.json").write_text(json.dumps(info, indent=2) + "\n")
    # Adjacent A/B pairs limit bias from host frequency/load drift. Alternate
    # the order within each randomized workload block.
    blocks = list(itertools.product(a.modes.split(","), range(a.reps)))
    rng = random.Random(20260928)
    rng.shuffle(blocks)
    jobs = []
    for pair, (mode, repeat) in enumerate(blocks):
        versions = list(binaries)
        rng.shuffle(versions)
        jobs.extend((v, mode, repeat, pair) for v in versions)
    for index, (version, mode, repeat, pair) in enumerate(jobs):
        row = trial(
            f,
            binaries[version],
            mode,
            a.rounds,
            folder / f"{index:03d}-{version}-{mode}",
            not a.native,
            client_tls=client_tls if version == "after" else None,
            server_tls=server_tls if version == "after" else None,
        )
        row.update(version=version, repetition=repeat, pair=pair)
        with (folder / "raw.jsonl").open("a") as out:
            out.write(json.dumps(row) + "\n")
        print(
            f"{index + 1}/{len(jobs)} {version} {mode}: server={row['server_cpu_us_per_request']:.2f} client={row['client_cpu_us_per_request']:.2f} CPU-us/op p99={row['p99_us']:.1f}us",
            flush=True,
        )
