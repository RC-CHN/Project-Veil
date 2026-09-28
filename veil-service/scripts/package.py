#!/usr/bin/env python3
"""Build Linux/systemd archives or portable Windows CLI ZIPs without installing."""

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
import time
import zipfile

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parent
ARCHES = {"amd64": {}, "arm64": {}, "armv7": {"GOARM": "7"}}


def package(version, arch, output, target="linux"):
    env = (
        os.environ
        | {
            "GOOS": target,
            "GOARCH": "arm" if arch == "armv7" else arch,
            "CGO_ENABLED": "0",
        }
        | ARCHES[arch]
    )
    output.mkdir(parents=True, exist_ok=True)
    name = f"veil-{version}-{target}-{arch}"
    with tempfile.TemporaryDirectory(prefix=".package-", dir=output) as temp:
        stage = Path(temp) / name
        docs = "share/veil/" if target == "linux" else ""
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts/build.py"),
                "batch",
                "--output-dir",
                str(stage / "bin" if target == "linux" else stage),
                "--version",
                version,
            ],
            env=env,
            check=True,
        )
        copies = {
            ROOT
            / "platform"
            / ("systemd" if target == "linux" else "windows")
            / "README.md": docs + "README.md",
            REPO / "LICENSE": docs + "LICENSE",
            REPO / "THIRD_PARTY.md": docs + "THIRD_PARTY.md",
        }
        if target == "linux":
            copies.update(
                {
                    ROOT / "platform/systemd/veil-system": "bin/veil-system",
                    ROOT
                    / "platform/systemd/veil@.service": "lib/systemd/system/veil@.service",
                    ROOT / "platform/systemd/veil.conf": "lib/sysusers.d/veil.conf",
                    ROOT / "platform/systemd/install.sh": "install.sh",
                }
            )
        for source, relative in copies.items():
            destination = stage / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, destination)
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
            "target": f"{target}/{arch}",
            "backend": "batch",
            "cgo": False,
            "go": subprocess.check_output(
                ["go", "version"], env=env, text=True
            ).strip(),
            "sha256": {
                p.relative_to(stage).as_posix(): hashlib.sha256(
                    p.read_bytes()
                ).hexdigest()
                for p in sorted(stage.rglob("*"))
                if p.is_file()
            },
        }
        (stage / docs / "manifest.json").write_text(
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

        archive = output / (name + (".tar.gz" if target == "linux" else ".zip"))
        pending = Path(temp) / archive.name
        if target == "windows":
            with zipfile.ZipFile(pending, "w", zipfile.ZIP_DEFLATED) as zipped:
                for path in sorted(stage.iterdir()):
                    info = zipfile.ZipInfo(
                        f"{name}/{path.name}", time.gmtime(max(epoch, 315532800))[:6]
                    )
                    info.create_system = 3
                    info.external_attr = 0o100644 << 16
                    info.compress_type = zipfile.ZIP_DEFLATED
                    zipped.writestr(info, path.read_bytes())
        else:
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
    parser.add_argument("--target", choices=("linux", "windows"), default="linux")
    native = {
        "x86_64": "amd64",
        "amd64": "amd64",
        "aarch64": "arm64",
        "arm64": "arm64",
        "armv7l": "armv7",
    }.get(platform.machine().lower())
    parser.add_argument("--arch", choices=ARCHES, default=native)
    parser.add_argument("--output-dir", type=Path, default=ROOT / ".build/releases")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}", args.version):
        parser.error("invalid version: use 1–64 ASCII letters, digits, dots, +, _ or -")
    if args.arch is None:
        parser.error("--arch is required on this build host")
    if args.target == "windows" and args.arch == "armv7":
        parser.error("Windows CLI packages support amd64 and arm64")
    package(args.version, args.arch, args.output_dir.resolve(), args.target)


if __name__ == "__main__":
    main()
