#!/usr/bin/env python3
"""Build a self-contained Linux/systemd archive without installing on the host."""

import argparse
import gzip
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

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parent
ARCHES = {"amd64": {}, "arm64": {}, "armv7": {"GOARM": "7"}}


def package(version, arch, output):
    env = (
        os.environ
        | {
            "GOOS": "linux",
            "GOARCH": "arm" if arch == "armv7" else arch,
            "CGO_ENABLED": "0",
        }
        | ARCHES[arch]
    )
    output.mkdir(parents=True, exist_ok=True)
    name = f"veil-{version}-linux-{arch}"
    with tempfile.TemporaryDirectory(prefix=".package-", dir=output) as temp:
        stage = Path(temp) / name
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts/build.py"),
                "batch",
                "--output-dir",
                str(stage / "bin"),
                "--version",
                version,
            ],
            env=env,
            check=True,
        )
        copies = {
            ROOT / "platform/systemd/veil-system": "bin/veil-system",
            ROOT / "platform/systemd/veil@.service": "lib/systemd/system/veil@.service",
            ROOT / "platform/systemd/veil.conf": "lib/sysusers.d/veil.conf",
            ROOT / "platform/systemd/install.sh": "install.sh",
            ROOT / "platform/systemd/README.md": "share/veil/README.md",
            REPO / "LICENSE": "share/veil/LICENSE",
            REPO / "THIRD_PARTY.md": "share/veil/THIRD_PARTY.md",
        }
        for source, relative in copies.items():
            target = stage / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, target)
        commit = subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=REPO, text=True
        ).strip()
        dirty = bool(
            subprocess.check_output(
                ["git", "status", "--porcelain"], cwd=REPO, text=True
            ).strip()
        )
        manifest = {
            "version": version,
            "commit": commit,
            "dirty": dirty,
            "target": f"linux/{arch}",
            "backend": "batch",
            "cgo": False,
            "go": subprocess.check_output(
                ["go", "version"], env=env, text=True
            ).strip(),
            "sha256": {
                str(p.relative_to(stage)): hashlib.sha256(p.read_bytes()).hexdigest()
                for p in sorted(stage.rglob("*"))
                if p.is_file()
            },
        }
        (stage / "share/veil/manifest.json").write_text(
            json.dumps(manifest, indent=2) + "\n"
        )
        epoch = int(
            os.environ.get("SOURCE_DATE_EPOCH")
            or subprocess.check_output(
                ["git", "show", "-s", "--format=%ct", "HEAD"], cwd=REPO, text=True
            ).strip()
        )

        def normalize(info):
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            info.mtime = epoch
            info.mode = (
                0o755
                if info.isdir()
                or "/bin/" in info.name
                or info.name.endswith("/install.sh")
                else 0o644
            )
            return info

        archive = output / f"{name}.tar.gz"
        pending = Path(temp) / archive.name
        with pending.open("wb") as raw:
            with gzip.GzipFile(
                fileobj=raw, filename="", mode="wb", mtime=epoch
            ) as zipped:
                with tarfile.open(
                    fileobj=zipped, mode="w", format=tarfile.PAX_FORMAT
                ) as tar:
                    tar.add(stage, arcname=name, filter=normalize)
        pending.replace(archive)
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        archive.with_name(archive.name + ".sha256").write_text(
            f"{digest}  {archive.name}\n"
        )
        print(archive)
    return archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    native = {"x86_64": "amd64", "aarch64": "arm64", "armv7l": "armv7"}.get(
        platform.machine()
    )
    parser.add_argument("--arch", choices=ARCHES, default=native)
    parser.add_argument("--output-dir", type=Path, default=ROOT / ".build/releases")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}", args.version):
        parser.error("invalid version: use 1–64 ASCII letters, digits, dots, +, _ or -")
    if args.arch is None:
        parser.error("--arch is required on this build host")
    package(args.version, args.arch, args.output_dir.resolve())


if __name__ == "__main__":
    main()
