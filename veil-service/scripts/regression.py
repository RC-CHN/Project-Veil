#!/usr/bin/env python3
"""Compare standalone CLI and managed runtime on an isolated loopback network.

Uses identical TLS backends and the core's warmup, perf counters, interop checks
and randomized paired workloads. The wrapper execs veild, so perf sees its PID.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import random
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
CORE = ROOT.parent / "veil-core"
sys.path.insert(0, str(CORE / "scripts"))
from core_regression import comparison, trial  # noqa: E402
from fixtures import fixture  # noqa: E402


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--cli", default=str(CORE / ".build/veil-batch"))
    p.add_argument("--daemon", default=str(ROOT / ".build/veild"))
    p.add_argument("--peer", default=str(CORE / ".build/benchpeer"))
    p.add_argument("--out", required=True)
    p.add_argument("--reps", type=int, default=5)
    p.add_argument("--bytes", type=int, default=1 << 30)
    p.add_argument("--rounds", type=int, default=10000)
    p.add_argument("--modes", default="U,D,E,S")
    p.add_argument("--cores", default="14,22,17,18")
    a = p.parse_args()
    if os.environ.get("VEIL_ISOLATED_NETNS") != "1" or os.readlink(
        "/proc/self/ns/net"
    ) == os.readlink("/proc/1/ns/net"):
        p.error("requires a private network namespace with VEIL_ISOLATED_NETNS=1")
    cores = list(map(int, a.cores.split(",")))
    if (
        len(cores) != 4
        or len(set(cores)) != 4
        or not set(cores) <= os.sched_getaffinity(0)
    ):
        p.error("select four available distinct CPUs")
    if min(a.reps, a.bytes, a.rounds) < 1:
        p.error("workload values must be positive")
    out = Path(a.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    cli, daemon, peer = [Path(x).resolve() for x in (a.cli, a.daemon, a.peer)]
    metadata = {
        "args": vars(a),
        "gomaxprocs": 1,
        "cores": cores,
        "binaries": {
            str(x): hashlib.sha256(x.read_bytes()).hexdigest()
            for x in (cli, daemon, peer)
        },
    }
    (out / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    f = fixture(out / "fixture")
    rows = []
    modes = a.modes.split(",")
    if not set(modes) <= {"U", "D", "E", "S"}:
        p.error("unknown workload")
    with tempfile.TemporaryDirectory(prefix="veil-managed-") as temp:
        wrapper = Path(temp) / "wrapper"
        wrapper.write_text(
            "#!/usr/bin/env python3\n"
            "import os, pathlib, shutil, sys\n"
            "assert len(sys.argv)==3 and sys.argv[1]=='-config'\n"
            "cfg=pathlib.Path(sys.argv[2])\n"
            f"state=pathlib.Path({temp!r}) / cfg.stem\n"
            "state.mkdir(mode=0o700,exist_ok=True)\n"
            "shutil.copyfile(cfg,state/'config.json')\n"
            "(state/'config.json').chmod(0o600)\n"
            f"os.execv({str(daemon)!r}, [{str(daemon)!r}, '-state-dir', str(state),"
            " '-socket', str(state/'control.sock'), '-autostart'])\n"
        )
        wrapper.chmod(0o700)

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
                True,
            )
            row.update(group=group, pair=pair)
            rows.append(row)
            (out / "results.json").write_text(json.dumps(rows, indent=2) + "\n")
            print(
                f"{group} {mode} pair={pair}: {row['work_per_cpu_ms']:.2f} work/CPU-ms",
                flush=True,
            )

        for name, pair in (
            ("cli-client-managed-server", (wrapper, cli)),
            ("managed-client-cli-server", (cli, wrapper)),
        ):
            run(name, pair, "S", -1, 100, 0)
        rng = random.Random(149)
        for pair in range(a.reps):
            for mode in modes:
                groups = [("before", (cli, cli)), ("after", (wrapper, wrapper))]
                rng.shuffle(groups)
                for group, binaries in groups:
                    run(group, binaries, mode, pair, a.rounds, a.bytes)
    summary = comparison(rows, modes)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
