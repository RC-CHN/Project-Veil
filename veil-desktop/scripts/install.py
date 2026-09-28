#!/usr/bin/env python3
"""Install or remove Veil Desktop for the current Linux user."""

import argparse
import os
from pathlib import Path
import shutil
import tempfile

FILES = (
    "veil-desktop",
    "veil.svg",
    "README.md",
    "LICENSE",
    "THIRD_PARTY.md",
    "manifest.json",
    "install.py",
)
APP_ID = "net.projectveil.Veil"
MARKER = "X-Veil-Managed=true\n"


def safe_path(path):
    if not path.is_absolute() or any(ord(c) < 32 for c in str(path)):
        raise ValueError(
            "installation paths must be absolute and contain no control characters"
        )
    if "%" in str(path):
        raise ValueError("desktop launchers require an installation path without %")
    for part in (path, *path.parents):
        if part.is_symlink():
            raise ValueError(f"refusing symbolic link: {part}")


def desktop_entry(target):
    # Desktop Entry string escaping is applied after Exec argument escaping.
    command = str(target / "veil-desktop").replace("%", "%%")
    for char in ("\\", '"', "`", "$"):
        command = command.replace(char, "\\" + char)
    command = ('"' + command + '"').replace("\\", "\\\\")
    icon = str(target / "veil.svg").replace("\\", "\\\\")
    return (
        "[Desktop Entry]\nType=Application\nName=Veil\n"
        "Comment=Manage your Veil proxy\nComment[zh_CN]=管理 Veil 代理\n"
        f"Exec={command}\nIcon={icon}\nTerminal=false\n"
        f"Categories=Network;\nStartupWMClass=Veil-desktop\n{MARKER}"
    )


def atomic_write(path, data, mode):
    fd, name = tempfile.mkstemp(prefix=".veil-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as out:
            out.write(data)
            os.fchmod(out.fileno(), mode)
        os.replace(name, path)
    finally:
        Path(name).unlink(missing_ok=True)


def manage(action, source, prefix):
    prefix = Path(os.path.abspath(prefix))
    target = prefix / "lib/veil-desktop"
    launcher = prefix / f"share/applications/{APP_ID}.desktop"
    for path in (target, launcher):
        safe_path(path)
    if target.exists():
        if not target.is_dir() or not (target / "manifest.json").is_file():
            raise ValueError(f"refusing unmanaged installation: {target}")
        for child in target.iterdir():
            if child.name not in FILES or child.is_symlink() or not child.is_file():
                raise ValueError(f"unexpected installation file: {child}")
    if launcher.exists() and MARKER not in launcher.read_text():
        raise ValueError(f"refusing unmanaged launcher: {launcher}")
    if action == "uninstall":
        launcher.unlink(missing_ok=True)
        if target.exists():
            shutil.rmtree(target)
        print("Veil Desktop removed. Saved profiles are preserved.")
        return
    # Read every input before replacing anything, including when run from the install directory.
    contents = {name: (source / name).read_bytes() for name in FILES}
    target.mkdir(parents=True, exist_ok=True)
    launcher.parent.mkdir(parents=True, exist_ok=True)
    for name, data in contents.items():
        atomic_write(target / name, data, 0o755 if name == "veil-desktop" else 0o644)
    atomic_write(launcher, desktop_entry(target).encode(), 0o644)
    print(f"Installed Veil Desktop: {target / 'veil-desktop'}")
    print("Open Veil from your applications menu.")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("action", choices=("install", "uninstall"))
    p.add_argument("--prefix", type=Path, default=Path.home() / ".local")
    a = p.parse_args()
    try:
        if os.geteuid() == 0:
            raise ValueError("run as your desktop user, without sudo")
        manage(a.action, Path(__file__).resolve().parent, a.prefix)
    except (ValueError, OSError) as e:
        p.exit(1, f"{e}\n")


if __name__ == "__main__":
    main()
