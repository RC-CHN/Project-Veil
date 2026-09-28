#!/usr/bin/env python3
"""Build native Linux tar or portable Windows zip releases for Veil Desktop."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parent


def package(version, target, arch, output):
    output.mkdir(parents=True, exist_ok=True)
    name = f"veil-desktop-{version}-{target}-{arch}"
    env = os.environ | {"GOOS": target, "GOARCH": arch}
    if target == "windows":
        env["CGO_ENABLED"] = "0"
    elif platform.system() != "Linux":
        raise ValueError("Linux desktop releases require a native Linux build host")
    with tempfile.TemporaryDirectory(prefix=".desktop-", dir=output) as temp:
        stage = Path(temp) / name
        binary = stage / ("veil-desktop.exe" if target == "windows" else "veil-desktop")
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts/build.py"),
                "--version",
                version,
                "--output",
                str(binary),
            ],
            env=env,
            check=True,
        )
        for source in (ROOT / "README.md", REPO / "LICENSE", REPO / "THIRD_PARTY.md"):
            shutil.copyfile(source, stage / source.name)
        if target == "linux":
            shutil.copyfile(ROOT / "scripts/install.py", stage / "install.py")
            shutil.copyfile(ROOT / "assets/veil.svg", stage / "veil.svg")
        manifest = {
            "version": version,
            "target": f"{target}/{arch}",
            "backend": "batch",
            "commit": subprocess.check_output(
                ["git", "rev-parse", "HEAD"], cwd=REPO, text=True
            ).strip(),
            "dirty": bool(
                subprocess.check_output(
                    ["git", "status", "--porcelain"], cwd=REPO, text=True
                ).strip()
            ),
            "go": subprocess.check_output(
                ["go", "version"], env=env, text=True
            ).strip(),
            "sha256": {
                p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                for p in sorted(stage.iterdir())
            },
        }
        (stage / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        suffix = ".zip" if target == "windows" else ".tar.gz"
        archive = output / (name + suffix)
        pending = Path(temp) / archive.name
        if target == "windows":
            with zipfile.ZipFile(pending, "w", zipfile.ZIP_DEFLATED) as out:
                for p in sorted(stage.iterdir()):
                    out.write(p, f"{name}/{p.name}")
        else:
            with tarfile.open(pending, "w:gz") as out:
                out.add(stage, arcname=name)
        pending.replace(archive)
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        archive.with_name(archive.name + ".sha256").write_text(
            f"{digest}  {archive.name}\n"
        )
        print(archive)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--version", required=True)
    p.add_argument(
        "--target", choices=("linux", "windows"), default=platform.system().lower()
    )
    p.add_argument(
        "--arch",
        choices=("amd64", "arm64"),
        default={
            "x86_64": "amd64",
            "AMD64": "amd64",
            "aarch64": "arm64",
            "ARM64": "arm64",
        }.get(platform.machine()),
    )
    p.add_argument("--output-dir", type=Path, default=ROOT / ".build/releases")
    a = p.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}", a.version):
        p.error("invalid version")
    if a.arch is None or a.target not in ("linux", "windows"):
        p.error("specify a supported target and architecture")
    package(a.version, a.target, a.arch, a.output_dir.resolve())


if __name__ == "__main__":
    main()
