#!/usr/bin/env python3
"""Build Veil and LuCI packages with ipkg-build or apk-tools 3 mkpkg."""

import argparse
import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PLATFORM = ROOT / "platform/openwrt"


def target(architecture):
    if architecture == "x86_64":
        return {"GOARCH": "amd64"}
    if architecture.startswith("aarch64_"):
        return {"GOARCH": "arm64"}
    if architecture.startswith("arm_"):
        return {"GOARCH": "arm", "GOARM": "5"}
    if architecture.startswith("mipsel_"):
        return {"GOARCH": "mipsle", "GOMIPS": "softfloat"}
    if architecture.startswith("mips_"):
        return {"GOARCH": "mips", "GOMIPS": "softfloat"}
    raise ValueError("unsupported package architecture")


def copy(source, root, relative, executable=False):
    out = root / relative
    out.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, out)
    out.chmod(0o755 if executable else 0o644)


def write(root, relative, text, executable=False):
    out = root / relative
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(text)
    out.chmod(0o755 if executable else 0o644)


def main():
    os.umask(0o022)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sdk", type=Path)
    parser.add_argument("--format", choices=("ipk", "apk"), default="ipk")
    parser.add_argument("--apk-tool", type=Path)
    parser.add_argument("--po2lmo", type=Path, required=True)
    parser.add_argument("--architecture", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output-dir", type=Path, default=ROOT / ".build/openwrt")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9][A-Za-z0-9.+-]{0,63}", args.version):
        parser.error(
            "version must start with a digit and contain only letters, digits, ., + or -"
        )
    if not re.fullmatch(r"[a-z0-9_]+", args.architecture):
        parser.error("invalid OpenWrt package architecture")
    try:
        platform_env = target(args.architecture)
    except ValueError as error:
        parser.error(str(error))
    ipkg = args.sdk.resolve() / "scripts/ipkg-build" if args.sdk else None
    compiler = args.po2lmo.resolve()
    if not compiler.is_file():
        parser.error("the compiled po2lmo tool is required")
    if args.format == "ipk" and (ipkg is None or not ipkg.is_file()):
        parser.error("IPK builds require SDK scripts/ipkg-build")
    if args.format == "apk" and (args.apk_tool is None or not args.apk_tool.is_file()):
        parser.error("APK builds require --apk-tool with mkpkg support")
    if args.format == "apk" and os.geteuid() != 0:
        parser.error("run APK builds under fakeroot to encode root file ownership")
    output = args.output_dir.resolve()
    output.mkdir(parents=True, exist_ok=True)
    env = os.environ | {"GOOS": "linux", "CGO_ENABLED": "0"} | platform_env
    package_env = os.environ | {
        "TAR": "tar --owner=0 --group=0",
        "SOURCE_DATE_EPOCH": os.environ.get("SOURCE_DATE_EPOCH")
        or subprocess.check_output(
            ["git", "show", "-s", "--format=%ct", "HEAD"], cwd=ROOT, text=True
        ).strip(),
    }
    with tempfile.TemporaryDirectory(prefix=".openwrt-", dir=output) as temp:
        folder = Path(temp)
        binaries = folder / "bin"
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts/build.py"),
                "batch",
                "--version",
                args.version,
                "--output-dir",
                str(binaries),
                "--program",
                "veild",
                "--program",
                "veilctl",
                "--program",
                "veil-rpc",
            ],
            env=env,
            check=True,
        )
        engine = folder / "veil"
        copy(binaries / "veild", engine, "usr/sbin/veild", True)
        copy(binaries / "veilctl", engine, "usr/bin/veilctl", True)
        copy(binaries / "veil-rpc", engine, "usr/libexec/rpcd/veil", True)
        copy(PLATFORM / "veil", engine, "etc/init.d/veil", True)
        copy(ROOT.parent / "LICENSE", engine, "usr/share/veil/LICENSE")
        copy(ROOT.parent / "THIRD_PARTY.md", engine, "usr/share/veil/THIRD_PARTY.md")
        write(engine, "lib/upgrade/keep.d/veil", "/etc/veil/\n")
        write(
            engine,
            "CONTROL/prerm",
            '#!/bin/sh\n[ -n "$IPKG_INSTROOT" ] && exit 0\n/etc/init.d/veil stop\nif [ "$1" = remove ]; then /etc/init.d/veil disable; fi\n',
            True,
        )
        app = folder / "luci-app-veil"
        for source_dir, prefix in (("root", ""), ("htdocs", "www")):
            base = PLATFORM / "luci-app-veil" / source_dir
            for source in base.rglob("*"):
                if source.is_file():
                    copy(source, app, str(Path(prefix) / source.relative_to(base)))
        translation = app / "usr/lib/lua/luci/i18n/veil.zh-cn.lmo"
        translation.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run(
            [
                str(compiler),
                str(PLATFORM / "luci-app-veil/po/zh_Hans/veil.po"),
                str(translation),
            ],
            check=True,
        )
        write(
            app,
            "CONTROL/postinst",
            '#!/bin/sh\n[ -n "$IPKG_INSTROOT" ] && exit 0\nuci set luci.languages.zh_cn="简体中文"\nuci commit luci\n/etc/init.d/rpcd reload\nrm -f /tmp/luci-indexcache*\n',
            True,
        )
        for root, name, arch, depends, description in (
            (
                engine,
                "veil",
                args.architecture,
                "ca-bundle, rpcd",
                "Veil SOCKS5 and HTTP proxy with local control",
            ),
            (
                app,
                "luci-app-veil",
                "all",
                "veil, luci-base",
                "Veil configuration and status for LuCI (English and Chinese)",
            ),
        ):
            write(
                root,
                "CONTROL/control",
                f"Package: {name}\nVersion: {args.version}\nArchitecture: {arch}\nMaintainer: Project Veil\nSection: net\nLicense: GPL-3.0-or-later\nInstalled-Size: 0\nDepends: {depends}\nDescription: {description}\n",
            )
            if args.format == "ipk":
                subprocess.run(
                    ["sh", str(ipkg), str(root), str(output)], env=package_env, check=True
                )
                artifact = output / f"{name}_{args.version}_{arch}.ipk"
            else:
                artifact = output / f"{name}-{args.version}.apk"
                hooks = folder / (name + "-hooks")
                (root / "CONTROL").rename(hooks)
                if name == "veil":
                    write(hooks, "pre-upgrade", '#!/bin/sh\n/etc/init.d/veil stop\n', True)
                    write(hooks, "pre-deinstall", '#!/bin/sh\n/etc/init.d/veil stop\n/etc/init.d/veil disable\n', True)
                    scripts = [(k, hooks / k) for k in ("pre-upgrade", "pre-deinstall")]
                else:
                    scripts = [(k, hooks / "postinst") for k in ("post-install", "post-upgrade")]
                for entry in [root, *root.rglob("*")]:
                    os.chown(entry, 0, 0)
                    if entry.is_dir():
                        entry.chmod(0o755)
                command = [str(args.apk_tool.resolve()), "mkpkg", "--files", str(root), "--output", str(artifact)]
                for key, value in {
                    "name": name, "version": args.version, "arch": "noarch" if arch == "all" else arch,
                    "depends": depends.replace(",", ""), "description": description,
                    "license": "GPL-3.0-or-later", "url": "https://github.com/RC-CHN/Project-Veil",
                }.items():
                    command += ["--info", f"{key}:{value}"]
                for kind, path in scripts:
                    command += ["--script", f"{kind}:{path}"]
                subprocess.run(command, env=package_env, check=True)
            digest = hashlib.sha256(artifact.read_bytes()).hexdigest()
            artifact.with_name(artifact.name + ".sha256").write_text(
                f"{digest}  {artifact.name}\n"
            )
            print(artifact)


if __name__ == "__main__":
    main()
