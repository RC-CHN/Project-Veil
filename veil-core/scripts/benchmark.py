#!/usr/bin/env python3
"""Randomized, CPU-affined full-proxy tests with warmup outside perf counters.
Only loopback endpoints are created. Raw results and perf CSVs are retained.
"""

import argparse
import hashlib
import itertools
import json
import os
import pathlib
import random
import select
import shutil
import statistics
import subprocess

from build import ROOT
from fixtures import configurations, fixture, port, spawn, stop, wait_port


def line(proc, timeout=90):
    ready, _, _ = select.select([proc.stdout], [], [], timeout)
    if not ready:
        raise TimeoutError("benchmark output timeout")
    raw = proc.stdout.readline()
    if not raw:
        raise RuntimeError("benchmark exited early")
    return json.loads(raw)


def cpu(pid):
    # /proc counters are secondary; primary server CPU time is perf task-clock.
    fields = pathlib.Path(f"/proc/{pid}/stat").read_text().split(") ", 1)[1].split()
    return (int(fields[11]) + int(fields[12])) / os.sysconf("SC_CLK_TCK")


def rss(pid):
    return int(pathlib.Path(f"/proc/{pid}/statm").read_text().split()[1]) * os.sysconf(
        "SC_PAGE_SIZE"
    )


class Perf:
    def __init__(self, pid, folder):
        self.ctl = folder / "perf.ctl"
        self.ack = folder / "perf.ack"
        self.csv = folder / "perf.csv"
        os.mkfifo(self.ctl)
        os.mkfifo(self.ack)
        self.cf = os.open(self.ctl, os.O_RDWR | os.O_NONBLOCK)
        self.af = os.open(self.ack, os.O_RDWR | os.O_NONBLOCK)
        self.log = open(folder / "perf.log", "w")
        self.p = subprocess.Popen(
            [
                "sudo",
                "-n",
                "perf",
                "stat",
                "-p",
                str(pid),
                "--delay=-1",
                "--control=fifo:" + str(self.ctl) + "," + str(self.ack),
                "-x,",
                "-e",
                "task-clock,cycles,instructions",
                "-o",
                str(self.csv),
            ],
            stdout=self.log,
            stderr=self.log,
        )

    def command(self, s):
        os.write(self.cf, (s + "\n").encode())
        if not select.select([self.af], [], [], 10)[0]:
            raise RuntimeError("perf control timeout")
        if b"ack" not in os.read(self.af, 4096):
            raise RuntimeError("perf did not acknowledge")

    def close(self):
        if self.p.poll() is None:
            subprocess.run(
                ["sudo", "-n", "kill", "-INT", str(self.p.pid)],
                check=True,
                stdout=subprocess.DEVNULL,
            )
            self.p.wait(timeout=10)
        os.close(self.cf)
        os.close(self.af)
        self.ctl.unlink()
        self.ack.unlink()
        self.log.close()

    def values(self):
        result = {}
        for row in self.csv.read_text().splitlines():
            f = row.split(",")
            if len(f) > 2:
                try:
                    result[f[2]] = float(f[0])
                except ValueError:
                    pass
        if result.get("task-clock", 0) <= 0:
            raise RuntimeError("no task-clock measurement: " + self.csv.read_text())
        return result


def trial(f, variant, mode, connections, size, rounds, index):
    folder = f["folder"] / f"{index:03d}-{variant}-{mode}{connections}"
    folder.mkdir()
    coverp, backendp, sp, cp = [port() for _ in range(4)]
    procs = []
    logs = []
    perf = None

    def launch(cmd, cores, gomax):
        log = open(folder / f"process-{len(procs)}.log", "w")
        logs.append(log)
        p = spawn(cmd, cores, gomax, stdout=log, stderr=log)
        procs.append(p)
        return p

    try:
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
            "16,17",
            2,
        )
        wait_port(coverp, cover)
        backend = launch(
            [
                ROOT / ".build/benchpeer",
                "-role",
                "backend",
                "-listen",
                f"127.0.0.1:{backendp}",
            ],
            "16,17",
            2,
        )
        wait_port(backendp, backend)
        sc, cc = configurations(f, variant, sp, cp, coverp)
        server = launch(sc, "14", 1)
        wait_port(sp, server)
        client = launch(cc, "22,23,24,25", 4)
        wait_port(cp, client)
        errlog = open(folder / "load.log", "w")
        logs.append(errlog)
        load = spawn(
            [
                ROOT / ".build/benchpeer",
                "-socks",
                f"127.0.0.1:{cp}",
                "-target",
                f"127.0.0.1:{backendp}",
                "-mode",
                mode,
                "-connections",
                str(connections),
                "-bytes",
                str(size),
                "-rounds",
                str(rounds),
            ],
            "18,19,20,21",
            4,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=errlog,
            text=True,
        )
        procs.append(load)
        if not line(load).get("ready"):
            raise RuntimeError("load not ready")
        perf = Perf(server.pid, folder)
        perf.command("enable")
        before = [cpu(server.pid), cpu(client.pid)]
        load.stdin.write("go\n")
        load.stdin.flush()
        measured = line(load)
        perf.command("disable")
        after = [cpu(server.pid), cpu(client.pid)]
        measured.update(
            server_rss_bytes=rss(server.pid), client_rss_bytes=rss(client.pid)
        )
        perf.close()
        values = perf.values()
        perf = None
        load.wait(timeout=5)
        seconds = values["task-clock"] / 1000
        measured.update(
            variant=variant,
            index=index,
            server_cpu_seconds=seconds,
            server_process_cpu_seconds=after[0] - before[0],
            client_process_cpu_seconds=after[1] - before[1],
            cycles=values.get("cycles"),
            instructions=values.get("instructions"),
            gib_per_cpu_second=measured["bytes"] / 2**30 / seconds,
            gib_per_second=measured["bytes"] / 2**30 / measured["seconds"],
            server_cpu_utilization=seconds / measured["seconds"],
        )
        return measured
    finally:
        if perf is not None:
            perf.close()
        for p in reversed(procs):
            stop(p)
        for log in logs:
            log.close()


def summarize(rows):
    groups = {}
    for r in rows:
        groups.setdefault((r["variant"], r["mode"], r["connections"]), []).append(r)
    results = []
    for (variant, mode, connections), rs in sorted(groups.items()):
        row = {
            "variant": variant,
            "mode": mode,
            "connections": connections,
            "repetitions": len(rs),
        }
        for key in [
            "gib_per_cpu_second",
            "gib_per_second",
            "server_cpu_utilization",
            "server_rss_bytes",
            "client_rss_bytes",
            "p50_us",
            "p99_us",
        ]:
            if key in rs[0]:
                row[key] = statistics.median(r[key] for r in rs)
        results.append(row)
    return results


def metadata(arguments):
    metadata = {
        "arguments": arguments,
        "cpu": subprocess.check_output(["lscpu"], text=True),
        "go": subprocess.check_output(["go", "version"], text=True)
        if shutil.which("go")
        else "not in test PATH; binaries were built separately",
        "openssl": subprocess.check_output(["openssl", "version"], text=True),
        "cpu_affinity": {
            "server": "14",
            "backend_cover": "16,17",
            "load": "18-21",
            "client": "22-25",
        },
        "binary_sha256": {
            str(p.relative_to(ROOT / ".build")): hashlib.sha256(
                p.read_bytes()
            ).hexdigest()
            for directory in (ROOT / ".build", ROOT / ".build/previous")
            for p in directory.glob("*")
            if p.is_file()
            and p.name
            in [
                "veil-native",
                "veil-batch",
                "veil-openssl",
                "singbox-native",
                "singbox-batch",
                "singbox-openssl",
                "benchpeer",
            ]
        },
    }
    metadata["source_sha256"] = {
        str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
        for parent in ["cmd", "core", "inbound", "internal", "patches", "scripts"]
        for path in (ROOT / parent).rglob("*")
        if path.is_file() and "__pycache__" not in path.parts
    }
    return metadata


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--out", default=".build/bench-pilot")
    p.add_argument(
        "--variants",
        default="anytls-native-tls,anytls-native-reality,anytls-opt-reality,veil-native-reality,veil-opt-reality",
    )
    p.add_argument("--modes", default="U,D")
    p.add_argument("--connections", default="1,8")
    p.add_argument("--reps", type=int, default=3)
    p.add_argument("--gib", type=float, default=1)
    p.add_argument("--rounds", type=int, default=10000)
    a = p.parse_args()
    folder = (ROOT / a.out).resolve()
    folder.mkdir(parents=True, exist_ok=True)
    if (folder / "raw.jsonl").exists():
        raise SystemExit(
            "use a new output directory; existing results are never overwritten"
        )
    f = fixture(folder)
    jobs = list(
        itertools.product(
            a.variants.split(","),
            a.modes.split(","),
            map(int, a.connections.split(",")),
            range(a.reps),
        )
    )
    random.Random(20260927).shuffle(jobs)
    (folder / "metadata.json").write_text(
        json.dumps(metadata(vars(a)), indent=2) + "\n"
    )
    rows = []
    for i, (variant, mode, n, _) in enumerate(jobs):
        result = trial(f, variant, mode, n, int(a.gib * 2**30), a.rounds, i)
        rows.append(result)
        with open(folder / "raw.jsonl", "a") as file:
            file.write(json.dumps(result) + "\n")
        (folder / "summary.json").write_text(
            json.dumps(summarize(rows), indent=2) + "\n"
        )
        print(
            f"{i + 1}/{len(jobs)} {variant} {mode}{n}: {result['gib_per_cpu_second']:.3f} GiB/CPU-s, {result['gib_per_second']:.3f} GiB/s",
            flush=True,
        )
