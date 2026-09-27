#!/usr/bin/env python3
"""Run meaningful protocol and TLS regressions against each selected backend."""

import argparse
import json
import subprocess
import time

from build import ENV, ROOT, generate

p = argparse.ArgumentParser()
p.add_argument("mode", choices=["native", "batch", "openssl"])
p.add_argument("--race", action="store_true")
p.add_argument("--stdlib", action="store_true")
a = p.parse_args()
flags = generate(a.mode)
commands = [
    [
        "go",
        "test",
        *flags,
        *(["-race"] if a.race else []),
        "-count=1",
        "-timeout=120s",
        "./...",
    ]
]
if a.mode == "native":
    commands.append(
        [
            "go",
            "test",
            *flags,
            *(["-race"] if a.race else []),
            "-count=1",
            "-timeout=120s",
            "github.com/metacubex/utls",
            "-run",
            "TestVeilPadding",
        ]
    )
else:
    commands.append(
        [
            "go",
            "test",
            *flags,
            *(["-race"] if a.race else []),
            "-count=1",
            "-timeout=120s",
            "github.com/metacubex/utls",
            "-run",
            "TestVeil|TestDynamicRecordSizing|Test.*Close|Test(Client)?KeyUpdate|TestFailedWrite",
        ]
    )
if a.stdlib:
    commands.append(["go", "test", *flags, "-short", "-timeout=180s", "crypto/tls"])
results = []
for cmd in commands:
    testenv = ENV.copy()
    # uTLS fixed handshake transcripts predate Go 1.26 custom-random changes.
    if "github.com/metacubex/utls" in cmd:
        testenv["GODEBUG"] = "cryptocustomrand=1"
    start = time.monotonic()
    r = subprocess.run(
        cmd,
        cwd=ROOT,
        env=testenv,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    print(r.stdout, flush=True)
    results.append(
        {
            "command": cmd,
            "GODEBUG": testenv.get("GODEBUG", ""),
            "seconds": round(time.monotonic() - start, 3),
            "returncode": r.returncode,
            "output": r.stdout,
        }
    )
    if r.returncode:
        break
folder = ROOT / ".build/validation"
folder.mkdir(exist_ok=True)
(
    folder
    / (a.mode + ("-race" if a.race else "") + ("-stdlib" if a.stdlib else "") + ".json")
).write_text(json.dumps(results, indent=2) + "\n")
raise SystemExit(any(r["returncode"] for r in results))
