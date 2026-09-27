#!/usr/bin/env python3
"""Run inside a disposable network namespace. Never alter host qdiscs.
Example: sudo unshare -n -- sh -c 'ip link set lo up && exec runuser -u USER -- env VEIL_ISOLATED_NETNS=1 python3 scripts/delayed_benchmark.py'
Only the proxy transport's TCP port is delayed: 10 ms each way, about 20 ms RTT.
"""

import itertools
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
        "10ms",
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
    folder = ROOT / ".build/bench-v0-delay20"
    folder.mkdir(parents=True, exist_ok=True)
    if (folder / "raw.jsonl").exists():
        raise SystemExit("existing results; choose a fresh output directory")
    f = fixture(folder)
    bench.configurations = configure
    variants = ["anytls-native-tls", "anytls-opt-reality", "veil-opt-reality"]
    jobs = (
        list(itertools.product(variants, ["U", "D"], [1, 8], range(3)))
        + list(itertools.product(variants, ["E"], [1, 8], range(3)))
        + list(itertools.product(variants, ["C"], [1], range(5)))
    )
    random.Random(20260927).shuffle(jobs)
    rows = []
    (folder / "metadata.json").write_text(
        json.dumps(
            {
                "scope": "isolated netns; only proxy TCP port; 10 ms netem per direction; no bandwidth/loss limit",
                "bulk_gib": 0.5,
                "echo_rounds": 100,
                "cold_rounds": 1,
                "netns": os.readlink("/proc/self/ns/net"),
                "repetitions": {"bulk": 3, "echo": 3, "cold": 5},
            },
            indent=2,
        )
        + "\n"
    )
    for i, (variant, mode, n, _) in enumerate(jobs):
        row = bench.trial(f, variant, mode, n, 1 << 29, 1 if mode == "C" else 100, i)
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
