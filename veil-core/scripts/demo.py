#!/usr/bin/env python3
"""Self-contained owned-loopback HTTPS through Veil SOCKS5 + REALITY demo."""

import json
import socket
import ssl

from build import ROOT
from fixtures import configurations, fixture, port, spawn, stop, wait_port


def read(c, n):
    b = b""
    while len(b) < n:
        p = c.recv(n - len(b))
        if not p:
            raise EOFError("truncated SOCKS reply")
        b += p
    return b


if __name__ == "__main__":
    f = fixture(ROOT / ".build/demo")
    procs = []
    logs = []

    def launch(cmd):
        log = open(f["folder"] / f"process-{len(procs)}.log", "w")
        logs.append(log)
        p = spawn(cmd, stdout=log, stderr=log)
        procs.append(p)
        return p

    try:
        coverp, sp, cp = [port() for _ in range(3)]
        cover = launch(
            [
                "openssl",
                "s_server",
                "-accept",
                f"127.0.0.1:{coverp}",
                "-cert",
                f["cert"],
                "-key",
                f["key"],
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
        wait_port(coverp, cover)
        sc, cc = configurations(f, "veil-opt-reality", sp, cp, coverp)
        server = launch(sc)
        wait_port(sp, server)
        client = launch(cc)
        wait_port(cp, client)
        with socket.create_connection(("127.0.0.1", cp), timeout=5) as raw:
            raw.sendall(b"\x05\x01\x00")
            assert read(raw, 2) == b"\x05\x00"
            raw.sendall(b"\x05\x01\x00\x01\x7f\x00\x00\x01" + coverp.to_bytes(2, "big"))
            assert read(raw, 10)[1] == 0
            context = ssl.create_default_context(cafile=f["cert"])
            context.minimum_version = ssl.TLSVersion.TLSv1_3
            with context.wrap_socket(raw, server_hostname="cover.test") as c:
                c.sendall(
                    b"GET / HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\n\r\n"
                )
                data = b""
                while True:
                    p = c.recv(65536)
                    if not p:
                        break
                    data += p
                status = data.split(b"\r\n")[0].decode()
                assert "200" in status
                print(
                    json.dumps(
                        {
                            "result": "pass",
                            "path": "HTTPS -> SOCKS5 -> Veil/REALITY -> owned HTTPS target",
                            "status": status,
                            "response_bytes": len(data),
                            "certificate_verification": True,
                        },
                        indent=2,
                    )
                )
    finally:
        for p in reversed(procs):
            stop(p)
        for log in logs:
            log.close()
