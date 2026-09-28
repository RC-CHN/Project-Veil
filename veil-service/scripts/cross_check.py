#!/usr/bin/env python3
"""Compile platform-neutral APIs and the Linux/FreeBSD local daemon adapters."""

import argparse
import json
import subprocess

from build import ROOT, core_build, prepare

targets = [
    ("linux", "amd64", {}, ["./cmd/veild", "./cmd/veilctl", "./cmd/veil-rpc"]),
    ("linux", "arm64", {}, ["./cmd/veild", "./cmd/veilctl", "./cmd/veil-rpc"]),
    ("linux", "arm", {"GOARM": "7"}, ["./cmd/veild", "./cmd/veilctl", "./cmd/veil-rpc"]),
    ("linux", "mips", {"GOMIPS": "softfloat"}, ["./cmd/veild", "./cmd/veilctl", "./cmd/veil-rpc"]),
    ("linux", "mipsle", {"GOMIPS": "softfloat"}, ["./cmd/veild", "./cmd/veilctl", "./cmd/veil-rpc"]),
    ("freebsd", "amd64", {}, ["./cmd/veild", "./cmd/veilctl"]),
    ("windows", "amd64", {}, ["veil/service", "./control"]),
    ("windows", "arm64", {}, ["veil/service", "./control"]),
    ("android", "arm64", {}, ["veil/service", "./control"]),
]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument(
    "--target", choices=[f"{goos}/{arch}" for goos, arch, _, _ in targets]
)
selected = parser.parse_args().target
flags = prepare("batch")
out = ROOT / ".build/cross"
out.mkdir(exist_ok=True)
results = []
for goos, arch, extra, packages in targets:
    if selected and f"{goos}/{arch}" != selected:
        continue
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
result_file = selected.replace("/", "-") if selected else "results"
(out / f"{result_file}.json").write_text(json.dumps(results, indent=2) + "\n")
raise SystemExit(any(x["returncode"] for x in results))
