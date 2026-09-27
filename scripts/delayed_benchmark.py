#!/usr/bin/env python3
"""Run inside a disposable network namespace. Never alter host qdiscs.
Example: sudo unshare -n -- sh -c 'ip link set lo up && exec runuser -u USER -- env VEIL_ISOLATED_NETNS=1 python3 scripts/delayed_benchmark.py'
Only the proxy transport's TCP port is delayed: 10 ms each way, about 20 ms RTT.
"""

import argparse
import json
import os
import random
import subprocess

import benchmark as bench
from build import ROOT
from fixtures import configurations, fixture


def tc(*args):
    subprocess.run(["sudo", "-n", "tc", *map(str, args)], check=True)


def configure(f, variant, sp, cp, cover):
    # A marker is set by the outer namespace launcher, and checked against PID 1
    # network namespace as an additional guard against accidental host mutation.
    if (
        os.environ.get("VEIL_ISOLATED_NETNS") != "1"
        or os.readlink("/proc/self/ns/net")
        == subprocess.check_output(
            ["sudo", "-n", "readlink", "/proc/1/ns/net"], text=True
        ).strip()
    ):
        raise RuntimeError("requires a separate network namespace")
    subprocess.run(
        ["sudo", "-n", "tc", "qdisc", "del", "dev", "lo", "root"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
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
        f"{a.delay_ms}ms",
        "limit",
        "10000",
    )
    if a.loss_percent:
        tc(
            "qdisc",
            "change",
            "dev",
            "lo",
            "parent",
            "1:3",
            "handle",
            "30:",
            "netem",
            "delay",
            f"{a.delay_ms}ms",
            "loss",
            "random",
            f"{a.loss_percent}%",
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
            sp,
            "0xffff",
            "flowid",
            "1:3",
        )
    return configurations(f, variant, sp, cp, cover)


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--out", default=".build/bench-delay20")
    p.add_argument(
        "--variants", default="anytls-native-tls,anytls-opt-reality,veil-opt-reality"
    )
    p.add_argument("--modes", default="U,D,E,C")
    p.add_argument("--connections", default="1,8")
    p.add_argument("--reps", type=int, default=3)
    p.add_argument("--cold-reps", type=int, default=5)
    p.add_argument("--gib", type=float, default=0.5)
    p.add_argument("--rounds", type=int, default=100)
    p.add_argument("--delay-ms", type=float, default=10)
    p.add_argument("--loss-percent", type=float, default=0)
    a = p.parse_args()
    if a.delay_ms < 0 or not 0 <= a.loss_percent <= 100:
        p.error("invalid delay or loss")
    folder = ROOT / a.out
    folder.mkdir(parents=True, exist_ok=True)
    if (folder / "raw.jsonl").exists():
        raise SystemExit("existing results; choose a fresh output directory")
    f = fixture(folder)
    bench.configurations = configure
    jobs = [
        (v, mode, n, repeat)
        for v in a.variants.split(",")
        for mode in a.modes.split(",")
        for n in ([1] if mode == "C" else map(int, a.connections.split(",")))
        for repeat in range(a.cold_reps if mode == "C" else a.reps)
    ]
    random.Random(20260927).shuffle(jobs)
    rows = []
    info = bench.metadata(vars(a))
    info.update(
        scope="isolated netns, proxy TCP port only",
        netns=os.readlink("/proc/self/ns/net"),
    )
    (folder / "metadata.json").write_text(json.dumps(info, indent=2) + "\n")

    def retransmissions():
        lines = open("/proc/net/snmp").read().splitlines()
        for i, line in enumerate(lines):
            if line.startswith("Tcp:"):
                return int(
                    dict(zip(line.split()[1:], lines[i + 1].split()[1:]))["RetransSegs"]
                )
        raise RuntimeError("missing TCP counters")

    for i, (variant, mode, n, _) in enumerate(jobs):
        before = retransmissions()
        row = bench.trial(
            f, variant, mode, n, int(a.gib * 2**30), 1 if mode == "C" else a.rounds, i
        )
        row["tcp_retransmissions_with_warmup"] = retransmissions() - before
        row["qdisc"] = json.loads(
            subprocess.check_output(
                ["sudo", "-n", "tc", "-s", "-j", "qdisc", "show", "dev", "lo"],
                text=True,
            )
        )
        rows.append(row)
        with open(folder / "raw.jsonl", "a") as out:
            out.write(json.dumps(row) + "\n")
        (folder / "summary.json").write_text(
            json.dumps(bench.summarize(rows), indent=2) + "\n"
        )
        print(
            f"{i + 1}/{len(jobs)} {variant} {mode}{n}: wall={row['seconds']:.3f}s p99_us={row.get('p99_us')}",
            flush=True,
        )
