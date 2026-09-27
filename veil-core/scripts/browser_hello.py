#!/usr/bin/env python3
"""Capture a browser ClientHello against an owned loopback listener.
Only normalized fields and hashes of ephemeral values are saved.
"""

import argparse
import json
import socket
import subprocess
import tempfile
from pathlib import Path

from build import ROOT
from fixtures import stop
from reality_fingerprint import hello


def read_exact(c, n):
    b = bytearray()
    while len(b) < n:
        part = c.recv(n - len(b))
        if not part:
            raise EOFError("truncated ClientHello")
        b.extend(part)
    return bytes(b)


def capture(browser, folder):
    # A fresh profile prevents session resumption and never opens user data.
    with tempfile.TemporaryDirectory(prefix="browser-", dir=folder) as profile:
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            listener.settimeout(15)
            port = listener.getsockname()[1]
            with open(folder / "browser.log", "w") as log:
                proc = subprocess.Popen(
                    [
                        str(browser),
                        "--headless",
                        "--no-sandbox",
                        "--disable-gpu",
                        "--disable-background-networking",
                        "--disable-component-update",
                        "--no-first-run",
                        "--no-default-browser-check",
                        "--no-proxy-server",
                        "--host-resolver-rules=MAP * 127.0.0.1",
                        f"--user-data-dir={profile}",
                        "--dump-dom",
                        f"https://cover.test:{port}/",
                    ],
                    stdout=log,
                    stderr=log,
                )
                try:
                    with listener.accept()[0] as c:
                        c.settimeout(5)
                        header = read_exact(c, 5)
                        n = int.from_bytes(header[3:5], "big")
                        if header[0] != 22 or n > 16384:
                            raise ValueError("unexpected browser TLS record")
                        return hello(read_exact(c, n))
                finally:
                    stop(proc)


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--browser", required=True, type=Path)
    p.add_argument("--out", required=True)
    p.add_argument("--samples", type=int, default=8)
    args = p.parse_args()
    if args.samples < 1:
        p.error("samples must be positive")
    folder = ROOT / args.out
    folder.mkdir(parents=True, exist_ok=False)
    rows = [capture(args.browser, folder) for _ in range(args.samples)]
    result = {
        "scope": "fresh local headless browser; owned loopback ClientHello only",
        "browser_version": subprocess.check_output(
            [str(args.browser), "--version"], text=True
        ).strip(),
        "samples": rows,
    }
    (folder / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(result["browser_version"])
    print("ClientHello lengths:", [r["bytes"] for r in rows])
    print("groups:", rows[0]["profile"]["extensions"].get("10"))
    print("key shares:", rows[0]["profile"]["extensions"].get("51"))
