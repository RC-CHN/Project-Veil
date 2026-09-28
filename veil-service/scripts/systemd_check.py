#!/usr/bin/env python3
"""Install/lifecycle checks, exclusively inside a disposable systemd container."""

import base64
import json
import os
from pathlib import Path
import pwd
import socket
import subprocess
import sys
import time

if (
    os.environ.get("VEIL_SYSTEMD_TEST") != "1"
    or not Path("/.dockerenv").exists()
    or Path("/proc/1/comm").read_text().strip() != "systemd"
):
    sys.exit("requires a disposable systemd container and VEIL_SYSTEMD_TEST=1")

release = Path(sys.argv[1]).resolve()


def run(*args, ok=True):
    result = subprocess.run(args, text=True, capture_output=True, timeout=20)
    if ok and result.returncode:
        raise RuntimeError(f"{args}: {result.stdout}{result.stderr}")
    if not ok and result.returncode == 0:
        raise RuntimeError(f"unexpected success: {args}")
    return result.stdout


def status(instance="check"):
    return json.loads(run("veil-system", instance, "status"))["status"]


def ready(instance="check"):
    for _ in range(100):
        try:
            return status(instance)
        except (RuntimeError, KeyError):
            time.sleep(0.05)
    raise TimeoutError("daemon control socket not ready")


def pid():
    return run("systemctl", "show", "-p", "MainPID", "--value", "veil@check.service")


run("sh", str(release / "install.sh"), "install")
run("systemd-analyze", "verify", "/usr/local/lib/systemd/system/veil@.service")
run("veil-system", "check", "enable")
assert ready()["state"] == "stopped"
run("veil-system", "second", "enable")
assert ready("second")["state"] == "stopped"
cfg = {
    "role": "client",
    "listen": "127.0.0.1:0",
    "server": "127.0.0.1:9",
    "secret": base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip("="),
    "tls": {"mode": "tls", "server_name": "cover.test"},
}
config = Path("/tmp/test-config.json")
config.write_text(json.dumps(cfg))
run("veil-system", "check", "validate", str(config))
run("veil-system", "check", "import", str(config))
run("veil-system", "check", "start")
current = status()
assert current["state"] == "running"
assert not status("second").get("saved_revision")
owner = pwd.getpwnam("veil").pw_uid
for path, mode in (
    ("/var/lib/veil-check", 0o700),
    ("/run/veil-check", 0o700),
    ("/run/veil-check/control.sock", 0o600),
    ("/var/lib/veil-check/config.json", 0o600),
):
    st = Path(path).stat()
    assert st.st_uid == owner and st.st_mode & 0o777 == mode, path
run("runuser", "-u", "nobody", "--", "veil-system", "check", "status", ok=False)
host, port = current["listen"].rsplit(":", 1)
with socket.create_connection((host, int(port)), timeout=3) as active:
    active.sendall(b"\x05")
    cfg["max_connections"] = 65
    config.write_text(json.dumps(cfg))
    before = pid()
    run("veil-system", "check", "import", str(config))
    assert status()["restart_required"]
    run("sh", str(release / "install.sh"), "install")
    assert pid() == before, "upgrade restarted the daemon"
    active.sendall(b"\x01\x00")
    assert active.recv(2) == b"\x05\x00", "save/upgrade interrupted a connection"
    run("veil-system", "check", "restart")
    assert active.recv(1) == b"", "explicit restart left old connection open"
assert not status()["restart_required"]
run("veil-system", "check", "stop")
assert status()["state"] == "stopped"
run("veil-system", "check", "daemon-restart")
assert ready()["state"] == "running"
run("veil-system", "check", "daemon-status")
logs = run("veil-system", "check", "logs")
assert "control listening" in logs and cfg["secret"] not in logs
saved = Path("/var/lib/veil-check/config.json").read_bytes()
run("sh", str(release / "install.sh"), "uninstall", ok=False)
run("veil-system", "check", "disable")
run("systemctl", "stop", "veil@second.service")
run("sh", str(release / "install.sh"), "uninstall", ok=False)
run("veil-system", "second", "disable")
run("sh", "/usr/local/share/veil/uninstall.sh", "uninstall")
assert not Path("/usr/local/bin/veild").exists()
assert Path("/var/lib/veil-check/config.json").read_bytes() == saved
run("sh", str(release / "install.sh"), "install")
run("veil-system", "check", "enable")
assert ready()["state"] == "running", "reinstall did not recover saved config"
run("veil-system", "check", "disable")
run("sh", "/usr/local/share/veil/uninstall.sh", "uninstall")
print(
    json.dumps(
        {
            "result": "passed",
            "checks": [
                "real systemd install",
                "unit validation",
                "two isolated instances",
                "private file ownership",
                "unprivileged access denied",
                "save/upgrade preserve active connection",
                "explicit restart",
                "daemon restart/autostart",
                "journal diagnostics",
                "active/enabled uninstall guard",
                "config-preserving uninstall/reinstall",
            ],
        }
    )
)
