#!/usr/bin/env python3
"""Bundle the OPNsense plugin and static Go binaries for native make package."""

import argparse
import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--plugins", type=Path, required=True, help="official OPNsense plugins checkout"
    )
    parser.add_argument("--version", required=True)
    parser.add_argument("--output-dir", type=Path, default=ROOT / ".build/opnsense")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9][A-Za-z0-9.+]{0,63}", args.version):
        parser.error(
            "version must start with a digit and contain letters, digits, . or +"
        )
    if not (args.plugins / "Mk/plugins.mk").is_file():
        parser.error("official plugins checkout must contain Mk/plugins.mk")
    output = args.output_dir.resolve()
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".opnsense-", dir=output) as temp:
        folder = Path(temp)
        for name in ("Mk", "Scripts", "Templates"):
            shutil.copytree(args.plugins / name, folder / name)
        port = folder / "net/veil"
        shutil.copytree(ROOT / "platform/opnsense", port)
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts/build.py"),
                "batch",
                "--version",
                args.version,
                "--output-dir",
                str(port / "src/bin"),
            ],
            env=os.environ | {"GOOS": "freebsd", "GOARCH": "amd64", "CGO_ENABLED": "0"},
            check=True,
        )
        rc = port / "src/etc/rc.d/veil"
        rc.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / "platform/freebsd/veil", rc)
        for path in (
            rc,
            port / "src/opnsense/scripts/veil/control.php",
            port / "src/etc/rc.syshook.d/start/91-veil",
        ):
            path.chmod(0o755)
        license_dir = port / "src/share/veil"
        license_dir.mkdir(parents=True)
        for name in ("LICENSE", "THIRD_PARTY.md"):
            shutil.copyfile(ROOT.parent / name, license_dir / name)
        revision = subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True
        ).strip()
        (folder / "build.sh").write_text(
            '#!/bin/sh\nset -eu\ncd "$(dirname "$0")"\n'
            '[ "$(uname -s)" = FreeBSD ] || { echo "Run inside OPNsense" >&2; exit 1; }\n'
            "abi=$(/usr/local/sbin/opnsense-version -v | cut -d. -f1,2)\n"
            f'make -C net/veil PLUGIN_VERSION={args.version} PLUGIN_HASH={revision} PLUGIN_ABI="$abi" PLUGIN_ARCH=amd64 package\n'
        )
        (folder / "build.sh").chmod(0o755)
        artifact = output / f"veil-opnsense-build-{args.version}.tar.gz"
        with tarfile.open(artifact, "w:gz") as archive:
            for path in sorted(folder.rglob("*")):
                if not path.is_file():
                    continue
                info = archive.gettarinfo(
                    str(path), arcname=str(path.relative_to(folder))
                )
                info.uid = info.gid = 0
                info.uname, info.gname = "root", "wheel"
                info.mode = 0o755 if path.stat().st_mode & 0o111 else 0o644
                with path.open("rb") as data:
                    archive.addfile(info, data)
        digest = hashlib.sha256(artifact.read_bytes()).hexdigest()
        artifact.with_suffix(artifact.suffix + ".sha256").write_text(
            f"{digest}  {artifact.name}\n"
        )
        print(artifact)


if __name__ == "__main__":
    main()
