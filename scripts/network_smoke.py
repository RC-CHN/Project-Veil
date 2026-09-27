#!/usr/bin/env python3
"""Low-traffic two-host TCP checks; run server/client roles over SSH.

The coordinator supplies temporary binaries, keys and config.json. Each role
owns and stops its subprocesses; server exits when the SSH input is closed.
No bandwidth saturation or host network configuration is performed.
"""

import argparse
import concurrent.futures
import hashlib
import json
import os
import pathlib
import signal
import socket
import statistics
import subprocess
import sys
import threading
import time


def read(c, n):
    data = bytearray()
    while len(data) < n:
        part = c.recv(n - len(data))
        if not part:
            raise EOFError("truncated response")
        data.extend(part)
    return bytes(data)


def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_port(p, proc, host="127.0.0.1"):
    for _ in range(300):
        if proc.poll() is not None:
            raise RuntimeError("proxy exited")
        try:
            with socket.create_connection((host, p), 0.1):
                return
        except OSError:
            time.sleep(0.01)
    raise TimeoutError("listener startup")


def stop(proc):
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


def target(listener):
    def handle(c):
        with c:
            c.settimeout(15)
            try:
                mode = read(c, 1)
                if mode == b"H":
                    data = bytearray()
                    while True:
                        part = c.recv(16384)
                        if not part:
                            break
                        data.extend(part)
                        if len(data) > 256 * 1024:
                            raise ValueError("test payload limit")
                    c.sendall(data)
                elif mode == b"E":
                    while True:
                        c.sendall(read(c, 64))
            except (OSError, EOFError):
                pass

    while True:
        try:
            c, _ = listener.accept()
        except OSError:
            return
        threading.Thread(target=handle, args=(c,), daemon=True).start()


def socks(p, target_port):
    c = socket.create_connection(("127.0.0.1", p), 5)
    c.settimeout(10)
    try:
        c.sendall(b"\x05\x01\x00")
        assert read(c, 2) == b"\x05\x00"
        c.sendall(b"\x05\x01\x00\x01\x7f\x00\x00\x01" + target_port.to_bytes(2, "big"))
        reply = read(c, 10)
        if reply[1] != 0:
            raise ConnectionError(f"SOCKS status {reply[1]}")
        return c
    except BaseException:
        c.close()
        raise


def client_checks(p, target_port, refused_port, proc):
    payload = hashlib.shake_256(b"Veil network integrity").digest(128 * 1024)
    opens = []
    for _ in range(3):
        start = time.monotonic()
        with socks(p, target_port) as c:
            opens.append((time.monotonic() - start) * 1000)
            c.sendall(b"H" + payload)
            c.shutdown(socket.SHUT_WR)
            assert read(c, len(payload)) == payload
            assert c.recv(1) == b""
        time.sleep(0.05)  # allow FIN/DONE to return the connection to the pool

    def echoes(_):
        timings = []
        with socks(p, target_port) as c:
            c.sendall(b"E")
            for i in range(32):
                data = hashlib.sha512(str(i).encode()).digest()
                start = time.monotonic()
                c.sendall(data)
                assert read(c, 64) == data
                timings.append((time.monotonic() - start) * 1000)
            c.shutdown(socket.SHUT_WR)
            assert c.recv(1) == b""
        return timings

    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        timings = sorted(t for row in pool.map(echoes, range(4)) for t in row)
    try:
        with socks(p, refused_port):
            raise AssertionError("unreachable target reported success")
    except ConnectionError:
        pass
    with socks(p, target_port) as c:
        c.sendall(b"E")
        start = time.monotonic()
        stop(proc)
        try:
            assert c.recv(1) == b""
            close_outcome = "eof"
        except ConnectionResetError:
            close_outcome = "reset"
        cancelled = (time.monotonic() - start) * 1000
    return {
        "result": "pass",
        "cold_open_ms": opens[0],
        "reused_open_ms": opens[1:],
        "half_close_rounds": 3,
        "verified_payload_bytes": len(payload) * 3,
        "echo_connections": 4,
        "echo_requests": len(timings),
        "echo_p50_ms": statistics.median(timings),
        "echo_p99_ms": timings[int(len(timings) * 0.99)],
        "active_cancel_ms": cancelled,
        "active_cancel_outcome": close_outcome,
        "target_failure_rejected": True,
    }


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("role", choices=["server", "client"])
    p.add_argument("--binary", required=True)
    p.add_argument("--mode", choices=["tls", "reality"], default="reality")
    p.add_argument("--server-ip", required=True)
    p.add_argument("--server-port", type=int)
    p.add_argument("--target-port", type=int)
    p.add_argument("--refused-port", type=int)
    a = p.parse_args()
    base = pathlib.Path(__file__).resolve().parent

    def terminate(signum, frame):
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, terminate)
    pidfile = base / (a.role + ".pid")
    pidfile.write_text(str(os.getpid()))
    keys = json.loads((base / "config.json").read_text())
    procs = []
    logs = []
    listener = None

    def launch(cmd):
        log = open(base / f"{a.role}-{a.mode}-{len(procs)}.log", "w")
        logs.append(log)
        proc = subprocess.Popen(
            list(map(str, cmd)),
            stdout=log,
            stderr=log,
            stdin=subprocess.DEVNULL,
            env=os.environ | {"GOMAXPROCS": "2"},
        )
        procs.append(proc)
        return proc

    try:
        tls = {"mode": a.mode, "server_name": "cover.test"}
        if a.role == "server":
            listener = socket.socket()
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            threading.Thread(target=target, args=(listener,), daemon=True).start()
            cp, sp = port(), port()
            if a.mode == "reality":
                cover = launch(
                    [
                        "openssl",
                        "s_server",
                        "-accept",
                        f"127.0.0.1:{cp}",
                        "-cert",
                        base / "cert.pem",
                        "-key",
                        base / "key.pem",
                        "-tls1_3",
                        "-ciphersuites",
                        "TLS_AES_128_GCM_SHA256",
                        "-groups",
                        "X25519",
                        "-www",
                        "-alpn",
                        "http/1.1",
                    ]
                )
                wait_port(cp, cover)
                tls.update(
                    reality_private_key=keys["reality_private_key"],
                    short_id=keys["short_id"],
                    cover_address=f"127.0.0.1:{cp}",
                )
            else:
                tls.update(
                    certificate=str(base / "cert.pem"),
                    private_key_file=str(base / "key.pem"),
                )
            cfg = {
                "role": "server",
                "listen": f"{a.server_ip}:{sp}",
                "secret": keys["secret"],
                "tls": tls,
            }
        else:
            sp = port()
            if a.mode == "reality":
                tls.update(
                    reality_public_key=keys["reality_public_key"],
                    short_id=keys["short_id"],
                )
            else:
                tls["ca_file"] = str(base / "cert.pem")
            cfg = {
                "role": "client",
                "listen": f"127.0.0.1:{sp}",
                "server": f"{a.server_ip}:{a.server_port}",
                "secret": keys["secret"],
                "tls": tls,
            }
        config = base / f"{a.role}.json"
        config.write_text(json.dumps(cfg))
        config.chmod(0o600)
        proc = launch([base / a.binary, "-config", config])
        wait_port(sp, proc, a.server_ip if a.role == "server" else "127.0.0.1")
        if a.role == "server":
            print(
                json.dumps(
                    {
                        "ready": True,
                        "server_port": sp,
                        "target_port": listener.getsockname()[1],
                        "refused_port": port(),
                    }
                ),
                flush=True,
            )
            sys.stdin.readline()
        else:
            print(
                json.dumps(client_checks(sp, a.target_port, a.refused_port, proc)),
                flush=True,
            )
    finally:
        for proc in reversed(procs):
            stop(proc)
        if listener:
            listener.close()
        for log in logs:
            log.close()
        pidfile.unlink(missing_ok=True)
