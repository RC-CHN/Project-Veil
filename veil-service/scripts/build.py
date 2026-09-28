#!/usr/bin/env python3
"""Build the service with the core's pinned TLS patches and backend selection."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[1]
CORE = ROOT.parent / "veil-core"
spec = importlib.util.spec_from_file_location(
    "veil_core_build", CORE / "scripts/build.py"
)
core_build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(core_build)


def prepare(mode):
    flags = core_build.generate(mode)
    out = ROOT / ".build" / mode
    out.mkdir(parents=True, exist_ok=True)
    mod = (
        (ROOT / "go.mod")
        .read_text()
        .replace("replace veil => ../veil-core", f"replace veil => {CORE}")
    )
    mod += f"\nreplace github.com/metacubex/utls => {CORE / '.build' / mode / 'utls'}\n"
    (out / "build.mod").write_text(mod)
    shutil.copyfile(ROOT / "go.sum", out / "build.sum")
    flags = [f for f in flags if not f.startswith("-modfile=")]
    flags.append("-modfile=" + str(out / "build.mod"))
    (out / "flags.json").write_text(json.dumps(flags) + "\n")
    return flags


def main():
    p = argparse.ArgumentParser()
    p.add_argument(
        "mode", choices=["native", "batch", "openssl"], default="batch", nargs="?"
    )
    p.add_argument("--check", action="store_true")
    p.add_argument("--race", action="store_true")
    p.add_argument("--generate-only", action="store_true")
    p.add_argument("--output-dir", type=Path, default=ROOT / ".build")
    p.add_argument("--version", default="dev")
    p.add_argument("--program", action="append", choices=("veild", "veilctl", "veil-rpc"))
    a = p.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}", a.version):
        p.error("version must be 1–64 ASCII letters, digits, dots, +, _ or -")
    flags = prepare(a.mode)
    if a.generate_only:
        return

    def run(args):
        subprocess.run(args, cwd=ROOT, env=core_build.ENV, check=True)

    if a.check:
        run(
            [
                "go",
                "test",
                *flags,
                *(["-race"] if a.race else []),
                "-count=1",
                "-timeout=120s",
                "./...",
            ]
        )
        run(["go", "vet", *flags, "./..."])
    else:
        a.output_dir.mkdir(parents=True, exist_ok=True)
        for name in a.program or ("veild", "veilctl"):
            suffix = ".exe" if os.environ.get("GOOS") == "windows" else ""
            run(
                [
                    "go",
                    "build",
                    *flags,
                    "-trimpath",
                    "-ldflags=-X veil-service/internal/buildinfo.Version=" + a.version,
                    "-o",
                    str(a.output_dir.resolve() / (name + suffix)),
                    "./cmd/" + name,
                ]
            )


if __name__ == "__main__":
    main()
