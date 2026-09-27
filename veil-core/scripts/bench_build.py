#!/usr/bin/env python3
"""Build untouched and identically TLS-optimized full sing-box baselines."""

import argparse
import shutil
import subprocess

from build import ENV, ROOT, generate

p = argparse.ArgumentParser()
p.add_argument("mode", choices=["native", "batch", "openssl"])
a = p.parse_args()
flags = generate(a.mode)
source = ROOT / ".build/mod/github.com/sagernet/sing-box@v1.15.0-alpha.9"
if not source.exists():
    subprocess.run(
        ["go", "mod", "download", "github.com/sagernet/sing-box@v1.15.0-alpha.9"],
        cwd=ROOT,
        env=ENV,
        check=True,
    )
work = ROOT / ".build/sing-box-src"
if not work.exists():
    shutil.copytree(source, work)
mod = ROOT / ".build" / ("singbox-" + a.mode + ".mod")
content = (source / "go.mod").read_text()
if a.mode != "native":
    content += (
        "\nreplace github.com/metacubex/utls => "
        + str(ROOT / ".build" / a.mode / "utls")
        + "\n"
    )
mod.write_text(content)
shutil.copyfile(source / "go.sum", mod.with_suffix(".sum"))
flags = [v for v in flags if not v.startswith("-modfile=")]
subprocess.run(
    [
        "go",
        "build",
        *flags,
        "-mod=mod",
        "-modfile=" + str(mod),
        "-trimpath",
        "-o",
        str(ROOT / ".build" / ("singbox-" + a.mode)),
        "./cmd/sing-box",
    ],
    cwd=work,
    env=ENV,
    check=True,
)
