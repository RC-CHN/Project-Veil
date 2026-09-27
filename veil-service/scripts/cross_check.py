#!/usr/bin/env python3
"""Compile platform-neutral APIs and the Linux/FreeBSD local daemon adapters."""

import json
import subprocess

from build import ROOT, core_build, prepare

flags = prepare("batch")
out = ROOT / ".build/cross"
out.mkdir(exist_ok=True)
results = []
for goos, arch, extra, packages in [
    ("linux", "amd64", {}, ["./cmd/veild", "./cmd/veilctl"]),
    ("linux", "arm64", {}, ["./cmd/veild", "./cmd/veilctl"]),
    ("linux", "arm", {"GOARM": "7"}, ["./cmd/veild", "./cmd/veilctl"]),
    ("linux", "mips", {"GOMIPS": "softfloat"}, ["./cmd/veild", "./cmd/veilctl"]),
    ("linux", "mipsle", {"GOMIPS": "softfloat"}, ["./cmd/veild", "./cmd/veilctl"]),
    ("freebsd", "amd64", {}, ["./cmd/veild", "./cmd/veilctl"]),
    ("windows", "amd64", {}, ["veil/service", "./control"]),
    ("windows", "arm64", {}, ["veil/service", "./control"]),
    ("android", "arm64", {}, ["veil/service", "./control"]),
]:
    env = core_build.ENV | {"GOOS": goos, "GOARCH": arch, "CGO_ENABLED": "0"} | extra
    r = subprocess.run(
        ["go", "build", *flags, *packages],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    row = dict(
        goos=goos,
        goarch=arch,
        options=extra,
        packages=packages,
        returncode=r.returncode,
        output=r.stdout,
    )
    results.append(row)
    print(json.dumps(row), flush=True)
(out / "results.json").write_text(json.dumps(results, indent=2) + "\n")
raise SystemExit(any(x["returncode"] for x in results))
