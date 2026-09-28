#!/usr/bin/env python3
"""Exercise real Windows CLI processes and named pipes in temporary user state."""

import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--bin-dir", type=Path, required=True)
    a = p.parse_args()
    if os.name != "nt":
        p.error("run this acceptance test on Windows")
    daemon, ctl = [
        str((a.bin_dir / name).resolve()) for name in ("veild.exe", "veilctl.exe")
    ]
    with tempfile.TemporaryDirectory(prefix="veil-cli-") as temp:
        root = Path(temp)
        pipe = r"\\.\pipe\veil-test-" + uuid.uuid4().hex
        profile = root / "import.json"
        profile.write_text(
            json.dumps(
                dict(
                    role="client",
                    inbound="mixed",
                    listen="127.0.0.1:0",
                    server="127.0.0.1:9",
                    secret=base64.urlsafe_b64encode(bytes([7] * 32))
                    .decode()
                    .rstrip("="),
                    tls=dict(mode="tls", server_name="test.example"),
                )
            )
        )

        def call(action, *flags, success=True):
            result = subprocess.run(
                [ctl, "-socket", pipe, *flags, action],
                capture_output=True,
                text=True,
                timeout=20,
            )
            if success and result.returncode:
                raise AssertionError(result.stderr)
            return result

        def start():
            proc = subprocess.Popen(
                [daemon, "-state-dir", str(root / "state"), "-socket", pipe],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            try:
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline:
                    if proc.poll() is not None:
                        raise AssertionError("daemon exited before ready")
                    status = call("status", success=False)
                    if status.returncode == 0:
                        return proc, json.loads(status.stdout)["status"]
                    time.sleep(0.1)
                raise AssertionError("daemon never became ready")
            except BaseException:
                proc.terminate()
                proc.wait(timeout=10)
                raise

        proc, state = start()
        try:
            assert state["state"] == "stopped"
            call("validate", "-config", str(profile))
            saved = json.loads(call("save", "-config", str(profile)).stdout)
            revision = saved["revision"]
            duplicate = subprocess.run(
                [daemon, "-state-dir", str(root / "state"), "-socket", pipe + "-other"],
                capture_output=True,
                timeout=5,
            )
            assert duplicate.returncode != 0, (
                "state lock did not exclude duplicate daemon"
            )
            running = json.loads(call("start", "-if-revision", revision).stdout)
            assert running["status"]["state"] == "running"
            assert (
                call("restart", "-if-revision", "stale", success=False).returncode != 0
            )
            assert "config" not in running and "secret" not in call("status").stdout
            config = json.loads(call("config").stdout)
            assert config["config"]["inbound"] == "mixed"
            assert json.loads(call("stop").stdout)["status"]["state"] == "stopped"
        finally:
            proc.terminate()
            proc.wait(timeout=10)
        proc, restored = start()
        try:
            assert (
                restored["saved_revision"] == revision
                and restored["state"] == "stopped"
            )
        finally:
            proc.terminate()
            proc.wait(timeout=10)
    print(
        "Windows real CLI lifecycle, privacy, revision guard, lock and recovery passed."
    )


if __name__ == "__main__":
    main()
