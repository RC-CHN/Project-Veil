#!/usr/bin/env python3
"""Build the Wails desktop with Veil's pinned TLS backend, without a JS toolchain."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parent
spec = importlib.util.spec_from_file_location(
    "veil_build", REPO / "veil-core/scripts/build.py"
)
core = importlib.util.module_from_spec(spec)
spec.loader.exec_module(core)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--version", default="dev")
    p.add_argument("--check", action="store_true")
    p.add_argument("--race", action="store_true")
    p.add_argument("--debug", action="store_true")
    p.add_argument("--output", type=Path)
    a = p.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}", a.version):
        p.error("invalid version")
    flags = core.generate("batch")
    out = ROOT / ".build"
    out.mkdir(exist_ok=True)
    mod = (ROOT / "go.mod").read_text()
    for name in ("veil-core", "veil-service"):
        mod = mod.replace("../" + name, json.dumps((REPO / name).as_posix()))
    fork = REPO / "veil-core/.build/batch/utls"
    mod += f"\nreplace github.com/metacubex/utls => {json.dumps(fork.as_posix())}\n"
    (out / "build.mod").write_text(mod)
    shutil.copyfile(ROOT / "go.sum", out / "build.sum")
    tags = "with_utls,webkit2_41,production" + (",debug" if a.debug else "")
    flags = [f for f in flags if not f.startswith(("-modfile=", "-tags="))]
    flags += ["-tags=" + tags, "-modfile=" + str(out / "build.mod")]

    def run(args):
        subprocess.run(["go", *args], cwd=ROOT, env=core.ENV, check=True)

    if a.check:
        run(
            [
                "test",
                *flags,
                *(["-race"] if a.race else []),
                "-count=1",
                "-timeout=120s",
                "./...",
            ]
        )
        run(["vet", *flags, "./..."])
    else:
        target = os.environ.get("GOOS", "")
        suffix = (
            ".exe" if target == "windows" or (not target and os.name == "nt") else ""
        )
        output = a.output or out / ("veil-desktop" + suffix)
        output.parent.mkdir(parents=True, exist_ok=True)
        ldflags = "-X main.version=" + a.version
        if suffix:
            ldflags += " -H=windowsgui"
        run(
            [
                "build",
                *flags,
                "-trimpath",
                "-ldflags=" + ldflags,
                "-o",
                str(output.resolve()),
                ".",
            ]
        )


if __name__ == "__main__":
    main()
