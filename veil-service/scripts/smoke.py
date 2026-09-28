#!/usr/bin/env python3
"""Exercise real veild/veilctl processes without installing a service."""

import base64
import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--bin-dir", type=Path, default=ROOT / ".build")
BIN = parser.parse_args().bin_dir.resolve()
if os.environ.get("VEIL_ISOLATED_NETNS") != "1":
    raise SystemExit("requires a private network namespace with VEIL_ISOLATED_NETNS=1")
# Hosted runners can hide root-owned /proc entries from their ordinary user.
# The launcher can capture its own namespace before unshare and pass it in.
parent_netns = os.environ.get("VEIL_PARENT_NETNS") or os.readlink("/proc/1/ns/net")
if os.readlink("/proc/self/ns/net") == parent_netns:
    raise SystemExit("requires a private network namespace with VEIL_ISOLATED_NETNS=1")

with tempfile.TemporaryDirectory(prefix="veil-smoke-") as temp:
    folder = Path(temp)
    state, control = folder / "state", folder / "run/control.sock"
    command = [
        str(BIN / "veild"),
        "-state-dir",
        str(state),
        "-socket",
        str(control),
    ]
    children = []
    log = open(folder / "daemon.log", "w+")

    def launch(autostart=False):
        child = subprocess.Popen(
            command + (["-autostart"] if autostart else []), stdout=log, stderr=log
        )
        children.append(child)
        for _ in range(100):
            if child.poll() is not None:
                raise RuntimeError("daemon exited during startup")
            try:
                ctl("status")
                return child
            except (OSError, subprocess.CalledProcessError):
                time.sleep(0.02)
        raise TimeoutError("daemon did not start")

    def ctl(action, config=None):
        args = [str(BIN / "veilctl"), "-socket", str(control)]
        if config is not None:
            args += ["-config", "-"]
        r = subprocess.run(
            args + [action],
            input=json.dumps(config) if config else "",
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
            timeout=10,
        )
        result = json.loads(r.stdout)
        assert "error" not in result, result
        return result["status"]

    try:
        process = launch()
        cfg = dict(
            role="client",
            listen="127.0.0.1:0",
            server="127.0.0.1:9",
            secret=base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip("="),
            tls=dict(mode="tls", server_name="cover.test"),
        )
        assert ctl("validate", cfg)["state"] == "stopped"
        assert not (state / "config.json").exists()
        assert ctl("save", cfg)["state"] == "stopped"
        status = ctl("start")
        address = status["listen"].split(":")
        with socket.create_connection(
            (address[0], int(address[1])), timeout=3
        ) as active:
            active.sendall(b"\x05")
            cfg["max_connections"] = 65
            saved = ctl("save", cfg)
            assert saved["restart_required"] and saved["listen"] == status["listen"]
            active.sendall(b"\x01\x00")
            assert active.recv(2) == b"\x05\x00", "save interrupted active connection"
            assert ctl("start")["restart_required"]
            changed = ctl("restart")
            assert not changed["restart_required"]
            assert active.recv(1) == b"", "restart left old connection open"
        duplicate = subprocess.run(
            command,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=5,
        )
        assert duplicate.returncode != 0 and "another daemon owns" in duplicate.stderr
        assert ctl("status")["state"] == "running"
        process.kill()
        process.wait(timeout=5)
        process = launch(autostart=True)
        assert ctl("status")["state"] == "running", "crash recovery/autostart failed"
        assert ctl("stop")["state"] == "stopped"
        process.terminate()
        assert process.wait(timeout=5) == 0
        assert not control.exists(), "control socket survived normal shutdown"

        with socket.socket() as busy:
            busy.bind(("127.0.0.1", 0))
            busy.listen()
            cfg["listen"] = f"127.0.0.1:{busy.getsockname()[1]}"
            (state / "config.json").write_text(json.dumps(cfg))
            process = launch(autostart=True)
            failed = ctl("status")
            assert failed["state"] == "stopped" and failed.get("error")
            cfg["listen"] = "127.0.0.1:0"
            ctl("save", cfg)
            assert ctl("start")["state"] == "running"
            process.terminate()
            assert process.wait(timeout=5) == 0
        print(
            json.dumps(
                {
                    "result": "passed",
                    "checks": [
                        "real CLI control",
                        "save preserves connection",
                        "explicit restart closes old stream",
                        "exclusive state lock",
                        "SIGKILL socket recovery",
                        "autostart",
                        "SIGTERM cleanup",
                        "failed autostart remains controllable",
                    ],
                }
            )
        )
    except Exception:
        log.flush()
        log.seek(0)
        print(log.read())
        raise
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
        log.close()
