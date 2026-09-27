#!/usr/bin/env python3
"""Compile portable CLI/core targets; this is not a device or packaging test."""

import json
import subprocess

from build import ENV, ROOT, generate


def main():
    flags = generate("native")
    folder = ROOT / ".build/cross"
    folder.mkdir(exist_ok=True)
    targets = [
        ("linux", "amd64"),
        ("linux", "arm64"),
        ("linux", "arm"),
        ("linux", "mips"),
        ("linux", "mipsle"),
        ("freebsd", "amd64"),
        ("windows", "amd64"),
        ("windows", "arm64"),
        ("android", "arm64"),
    ]
    rows = []
    for system, arch in targets:
        env = ENV | {
            "GOOS": system,
            "GOARCH": arch,
            "CGO_ENABLED": "0",
            "GOMAXPROCS": "2",
        }
        if arch == "arm":
            env["GOARM"] = "7"
        if arch in ("mips", "mipsle"):
            env["GOMIPS"] = "softfloat"
        cmd = ["go", "build", *flags, "-buildvcs=false", "-trimpath"]
        if system == "android":
            cmd += ["./core", "./inbound"]
        else:
            suffix = ".exe" if system == "windows" else ""
            cmd += ["-o", str(folder / f"veil-{system}-{arch}{suffix}"), "./cmd/veil"]
        result = subprocess.run(cmd, env=env, cwd=ROOT, capture_output=True, text=True)
        row = {
            "os": system,
            "arch": arch,
            "scope": "packages" if system == "android" else "cli",
            "command": cmd,
            "returncode": result.returncode,
            "output": result.stdout + result.stderr,
        }
        rows.append(row)
        print(json.dumps(row), flush=True)
        (folder / "results.json").write_text(json.dumps(rows, indent=2) + "\n")
    raise SystemExit(any(row["returncode"] for row in rows))


if __name__ == "__main__":
    main()
