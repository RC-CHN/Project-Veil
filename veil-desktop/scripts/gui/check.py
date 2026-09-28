#!/usr/bin/env python3
"""Run the packaged desktop and real loopback transfers on an isolated Windows user."""

import argparse
import base64
import contextlib
import http.server
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parent


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--server", required=True, type=Path)
    parser.add_argument("--artifacts", required=True, type=Path)
    args = parser.parse_args()
    if os.name != "nt" or os.environ.get("VEIL_TEST_SYSTEM_PROXY") != "1":
        parser.error(
            "requires Windows and VEIL_TEST_SYSTEM_PROXY=1 on an isolated user"
        )
    import winreg
    import windows

    args.artifacts = args.artifacts.resolve()
    args.artifacts.mkdir(parents=True, exist_ok=True)
    original = windows.snapshot()
    policy = r"Software\Policies\Microsoft\Edge\WebView2\AdditionalBrowserArguments"
    with winreg.CreateKey(winreg.HKEY_CURRENT_USER, policy) as key:
        try:
            old_policy = winreg.QueryValueEx(key, "veil-desktop.exe")
        except FileNotFoundError:
            old_policy = None
    with tempfile.TemporaryDirectory(prefix="veil desktop gui ") as temp:
        root = Path(temp)
        processes, servers = [], []

        def restore_policy():
            with winreg.CreateKey(winreg.HKEY_CURRENT_USER, policy) as key:
                if old_policy is None:
                    with contextlib.suppress(FileNotFoundError):
                        winreg.DeleteValue(key, "veil-desktop.exe")
                else:
                    winreg.SetValueEx(
                        key, "veil-desktop.exe", 0, old_policy[1], old_policy[0]
                    )

        with contextlib.ExitStack() as stack:
            # Restore user settings even if process teardown itself fails.
            stack.callback(restore_policy)
            stack.callback(windows.write, original)
            try:
                zipfile.ZipFile(args.package).extractall(root / "package")
                (binary,) = (root / "package").glob("*/veil-desktop.exe")
                cert, keyfile = root / "cert.pem", root / "key.pem"
                subprocess.run(
                    [
                        "openssl",
                        "req",
                        "-x509",
                        "-newkey",
                        "rsa:2048",
                        "-nodes",
                        "-keyout",
                        str(keyfile),
                        "-out",
                        str(cert),
                        "-days",
                        "1",
                        "-subj",
                        "/CN=localhost",
                        "-addext",
                        "subjectAltName=DNS:localhost,IP:127.0.0.1",
                    ],
                    check=True,
                    capture_output=True,
                )

                class Handler(http.server.BaseHTTPRequestHandler):
                    def log_message(self, *_args):
                        pass

                    def do_GET(self):
                        body = b"Veil Windows GUI acceptance\n"
                        self.send_response(200)
                        self.send_header("Content-Length", str(len(body)))
                        self.end_headers()
                        self.wfile.write(body)

                for secure in (False, True):
                    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
                    if secure:
                        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
                        tls.load_cert_chain(cert, keyfile)
                        server.socket = tls.wrap_socket(server.socket, server_side=True)
                    servers.append(server)
                    threading.Thread(target=server.serve_forever, daemon=True).start()
                secret = (
                    base64.urlsafe_b64encode(secrets.token_bytes(32))
                    .decode()
                    .rstrip("=")
                )
                server_port, proxy_port, cdp_port = port(), port(), port()
                config = dict(
                    role="server",
                    listen=f"127.0.0.1:{server_port}",
                    secret=secret,
                    tls=dict(
                        mode="tls",
                        server_name="localhost",
                        certificate=str(cert),
                        private_key_file=str(keyfile),
                    ),
                )
                (root / "server.json").write_text(json.dumps(config))
                config = dict(
                    role="client",
                    listen=f"127.0.0.1:{proxy_port}",
                    inbound="mixed",
                    server=f"127.0.0.1:{server_port}",
                    secret=secret,
                    tls=dict(mode="tls", server_name="localhost", ca_file=str(cert)),
                )
                (root / "client.json").write_text(json.dumps(config))
                baseline = dict(
                    flags=5,
                    server="",
                    bypass="<local>",
                    pac="http://127.0.0.1:9/veil-test.pac",
                )
                windows.write(baseline)
                with winreg.CreateKey(winreg.HKEY_CURRENT_USER, policy) as key:
                    winreg.SetValueEx(
                        key,
                        "veil-desktop.exe",
                        0,
                        winreg.REG_SZ,
                        f"--remote-debugging-port={cdp_port}",
                    )
                for name, command in [
                    (
                        "server",
                        [
                            str(args.server.resolve()),
                            "-config",
                            str(root / "server.json"),
                        ],
                    ),
                    ("desktop", [str(binary), "-state-dir", str(root / "state")]),
                ]:
                    log = stack.enter_context(
                        (args.artifacts / f"{name}.log").open("wb")
                    )
                    processes.append(subprocess.Popen(command, stdout=log, stderr=log))
                deadline = time.monotonic() + 45
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                while True:
                    if any(p.poll() is not None for p in processes):
                        raise RuntimeError(
                            "desktop or fixture exited before CDP was ready"
                        )
                    try:
                        with opener.open(
                            f"http://127.0.0.1:{cdp_port}/json/version", timeout=1
                        ):
                            break
                    except OSError:
                        if time.monotonic() > deadline:
                            raise TimeoutError("WebView2 CDP did not become ready")
                        time.sleep(0.2)
                fixture = dict(
                    cdp=cdp_port,
                    proxy=proxy_port,
                    http=servers[0].server_port,
                    https=servers[1].server_port,
                    cert=str(cert),
                    config=str(root / "client.json"),
                    python=sys.executable,
                    artifacts=str(args.artifacts),
                    baseline=baseline,
                )
                (root / "fixture.json").write_text(json.dumps(fixture))
                subprocess.run(
                    ["node", str(ROOT / "check.mjs"), str(root / "fixture.json")],
                    check=True,
                    timeout=120,
                )
                assert processes[1].wait(timeout=10) == 0, (
                    "explicit desktop exit failed"
                )
                assert windows.snapshot() == json.loads(
                    (args.artifacts / "cleared-proxy.json").read_text()
                ), "explicit exit failed to restore cleared baseline"
                with socket.socket() as sock:
                    sock.settimeout(1)
                    assert sock.connect_ex(("127.0.0.1", proxy_port)) != 0, (
                        "exit left proxy listening"
                    )
                (args.artifacts / "result.json").write_text(
                    json.dumps(
                        {"passed": True, "explicit_exit": True, "proxy_restored": True}
                    )
                )
                print(
                    "Windows packaged WebView2 GUI, native proxy settings, close-to-tray and four transfer paths passed."
                )
            finally:
                for proc in reversed(processes):
                    if proc.poll() is None:
                        subprocess.run(
                            ["taskkill", "/PID", str(proc.pid), "/T", "/F"],
                            capture_output=True,
                        )
                        proc.wait(timeout=10)
                for server in servers:
                    server.shutdown()
                    server.server_close()


if __name__ == "__main__":
    main()
