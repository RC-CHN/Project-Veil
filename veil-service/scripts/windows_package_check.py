#!/usr/bin/env python3
"""Verify a Windows CLI archive; optionally run its actual programs on Windows."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import zipfile

FILES = {"veild.exe", "veilctl.exe", "README.md", "LICENSE", "THIRD_PARTY.md"}
MACHINES = {"windows/amd64": 0x8664, "windows/arm64": 0xAA64}


def verify(archive):
    digest = archive.with_name(archive.name + ".sha256").read_text().split()[0]
    assert hashlib.sha256(archive.read_bytes()).hexdigest() == digest, (
        "archive digest mismatch"
    )
    with zipfile.ZipFile(archive) as package:
        names = [f"{archive.stem}/{name}" for name in FILES | {"manifest.json"}]
        assert sorted(package.namelist()) == sorted(names), "unexpected package layout"
        contents = {Path(name).name: package.read(name) for name in names}
    manifest = json.loads(contents.pop("manifest.json"))
    assert manifest["target"] in MACHINES, "unexpected target"
    assert manifest["backend"] == "batch" and manifest["cgo"] is False
    assert set(manifest["sha256"]) == FILES, "incomplete file manifest"
    assert (
        archive.stem
        == f"veil-{manifest['version']}-{manifest['target'].replace('/', '-')}"
    )
    for name, data in contents.items():
        assert hashlib.sha256(data).hexdigest() == manifest["sha256"][name], name
    for name in ("veild.exe", "veilctl.exe"):
        data = contents[name]
        assert data[:2] == b"MZ", f"{name}: not a Windows executable"
        offset = struct.unpack_from("<I", data, 0x3C)[0]
        assert data[offset : offset + 4] == b"PE\0\0", f"{name}: missing PE header"
        assert (
            struct.unpack_from("<H", data, offset + 4)[0]
            == MACHINES[manifest["target"]]
        ), f"{name}: wrong architecture"
    return manifest, contents


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument(
        "--run", action="store_true", help="run packaged CLI lifecycle tests on Windows"
    )
    args = parser.parse_args()
    if args.run and os.name != "nt":
        parser.error("--run requires native Windows")
    manifest, contents = verify(args.archive.resolve())
    print(
        "Windows CLI archive integrity, layout and PE architecture verified.",
        flush=True,
    )
    if args.run:
        with tempfile.TemporaryDirectory(prefix="veil cli package ") as folder:
            root = Path(folder)
            for name, data in contents.items():
                (root / name).write_bytes(data)
            for name in ("veild.exe", "veilctl.exe"):
                version = subprocess.check_output(
                    [root / name, "-version"], text=True, timeout=10
                )
                assert (
                    version.strip()
                    == f"Veil {manifest['version']} ({manifest['target']})"
                ), version
            subprocess.run(
                [
                    sys.executable,
                    str(Path(__file__).with_name("windows_smoke.py")),
                    "--bin-dir",
                    str(root),
                ],
                check=True,
                timeout=120,
            )


if __name__ == "__main__":
    main()
