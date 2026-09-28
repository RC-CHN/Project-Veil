"""Verify the complete tag payload before creating and publishing a release."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tarfile
import zipfile


def payload(version):
    files = [
        f"veil-{version}-{target}-{arch}.{suffix}"
        for target, arch, suffix in (
            ("linux", "amd64", "tar.gz"),
            ("linux", "arm64", "tar.gz"),
            ("linux", "armv7", "tar.gz"),
            ("windows", "amd64", "zip"),
            ("windows", "arm64", "zip"),
        )
    ]
    files += [
        f"veil-desktop-{version}-linux-amd64.tar.gz",
        f"veil-desktop-{version}-windows-amd64.zip",
        f"veil_{version}-1_x86_64.ipk",
        f"luci-app-veil_{version}-1_all.ipk",
        f"veil-{version}-r1.apk",
        f"luci-app-veil-{version}-r1.apk",
        f"veil-opnsense-build-{version}.tar.gz",
    ]
    return sorted(files)


def verify_manifest(raw, version, commit):
    manifest = json.loads(raw)
    if (
        manifest["version"] != version
        or manifest["commit"] != commit
        or manifest["dirty"] is not False
    ):
        raise ValueError("package manifest does not match the clean release commit")


def verify(folder, version, commit):
    names = payload(version)
    expected = set(names) | {name + ".sha256" for name in names}
    if {p.name for p in folder.iterdir()} != expected:
        raise ValueError(
            "release payload is missing files or contains unexpected files"
        )
    lines = []
    for name in names:
        path = folder / name
        with path.open("rb") as source:
            digest = hashlib.file_digest(source, "sha256").hexdigest()
        line = f"{digest}  {name}\n"
        if (folder / (name + ".sha256")).read_text() != line:
            raise ValueError(f"checksum mismatch: {name}")
        lines.append(line)
        if name.endswith(".zip"):
            with zipfile.ZipFile(path) as archive:
                manifests = [
                    n for n in archive.namelist() if n.endswith("/manifest.json")
                ]
                if len(manifests) != 1:
                    raise ValueError(f"missing or duplicate manifest: {name}")
                verify_manifest(archive.read(manifests[0]), version, commit)
        elif name.endswith(".tar.gz") and "opnsense" not in name:
            with tarfile.open(path) as archive:
                manifests = [m for m in archive if m.name.endswith("/manifest.json")]
                if len(manifests) != 1:
                    raise ValueError(f"missing or duplicate manifest: {name}")
                verify_manifest(
                    archive.extractfile(manifests[0]).read(), version, commit
                )
    (folder / "SHA256SUMS").write_text("".join(lines))
    return names + [name + ".sha256" for name in names] + ["SHA256SUMS"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("folder", type=Path)
    parser.add_argument("--publish", action="store_true")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", args.version):
        parser.error("expected a stable MAJOR.MINOR.PATCH version")
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    assets = verify(args.folder, args.version, commit)
    print(f"Verified {len(assets)} release assets for {commit}")
    if args.publish:
        tag = "v" + args.version
        # gh creates a draft, uploads every asset, then publishes it. An existing
        # release causes an error: reruns cannot overwrite a published payload.
        subprocess.run(
            [
                "gh",
                "release",
                "create",
                tag,
                "--verify-tag",
                "--title",
                f"Veil {tag}",
                "--notes-file",
                f".github/releases/{tag}.md",
                *[str(args.folder / name) for name in assets],
            ],
            check=True,
        )


if __name__ == "__main__":
    main()
