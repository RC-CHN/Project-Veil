#!/usr/bin/env python3
"""Exercise released CLI processes over loopback, without root or Docker.

This is Linux process acceptance, not TUN, public-VPS or performance acceptance.
All network endpoints belong to this run. Private material lives in a temporary
0700 directory and is removed even after a failed case; evidence contains no keys.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import socketserver
import struct
import subprocess
import sys
import tempfile
import threading
import time

from common import ROOT, new_run, source_manifest


BLOCK = bytes(range(256)) * 64


def save(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def private_json(path, value):
    # Only called for files within this run's private directory.
    with path.open("w") as stream:
        json.dump(value, stream)
    path.chmod(0o600)


def exact(stream, count):
    result = bytearray()
    while len(result) < count:
        chunk = stream.recv(count - len(result))
        if not chunk:
            raise EOFError("unexpected EOF")
        result.extend(chunk)
    return bytes(result)


def socks_address(host, port):
    try:
        raw = socket.inet_pton(socket.AF_INET, host)
        data = b"\x01" + raw
    except OSError:
        try:
            data = b"\x04" + socket.inet_pton(socket.AF_INET6, host)
        except OSError:
            raw = host.encode("ascii")
            if not 1 <= len(raw) <= 253:
                raise ValueError("domain length")
            data = b"\x03" + bytes([len(raw)]) + raw
    return data + struct.pack("!H", port)


def read_socks_address(stream):
    kind = exact(stream, 1)[0]
    if kind in (1, 4):
        family, length = (socket.AF_INET, 4) if kind == 1 else (socket.AF_INET6, 16)
        host = socket.inet_ntop(family, exact(stream, length))
    elif kind == 3:
        host = exact(stream, exact(stream, 1)[0]).decode("ascii")
    else:
        raise ValueError("SOCKS address type")
    return host, struct.unpack("!H", exact(stream, 2))[0]


def socks_connect(address, host, port, command=1, timeout=30):
    stream = socket.create_connection(address, timeout)
    try:
        stream.sendall(b"\x05\x01\x00")
        if exact(stream, 2) != b"\x05\x00":
            raise ValueError("SOCKS greeting")
        stream.sendall(bytes([5, command, 0]) + socks_address(host, port))
        if exact(stream, 3) != b"\x05\x00\x00":
            raise ValueError("SOCKS request rejected")
        bound = read_socks_address(stream)
        return stream, bound
    except BaseException:
        stream.close()
        raise


class TCPFixture(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = False
    block_on_close = True


class UDPFixture(socketserver.ThreadingUDPServer):
    daemon_threads = False
    block_on_close = True


class Fixtures:
    def __init__(self):
        self.lock = threading.Lock()
        self.witness = {"tcp_connections": 0, "tcp_bytes": 0, "udp_packets": 0,
                        "udp_bytes": 0, "dns_questions": [], "downloads": []}
        self.servers, self.threads = [], []
        owner = self

        class Echo(socketserver.BaseRequestHandler):
            def handle(self):
                count = 0
                self.request.settimeout(20)
                try:
                    while data := self.request.recv(65536):
                        count += len(data)
                        self.request.sendall(data)
                except (OSError, EOFError):
                    pass
                finally:
                    with owner.lock:
                        owner.witness["tcp_connections"] += 1
                        owner.witness["tcp_bytes"] += count

        class Datagram(socketserver.BaseRequestHandler):
            def handle(self):
                data, sock = self.request
                sock.sendto(data, self.client_address)
                with owner.lock:
                    owner.witness["udp_packets"] += 1
                    owner.witness["udp_bytes"] += len(data)

        class Download(socketserver.StreamRequestHandler):
            def handle(self):
                self.request.settimeout(30)
                record = {"requested": 0, "written": 0}
                try:
                    line = self.rfile.readline(64)
                    if not line.endswith(b"\n"):
                        raise ValueError("download request")
                    size = int(line)
                    if not 1 <= size <= 32 << 20:
                        raise ValueError("download size")
                    record["requested"] = size
                    while record["written"] < size:
                        chunk = BLOCK[:min(len(BLOCK), size-record["written"])]
                        self.request.sendall(chunk)
                        record["written"] += len(chunk)
                except (OSError, ValueError) as error:
                    record["error"] = type(error).__name__
                finally:
                    with owner.lock:
                        owner.witness["downloads"].append(record)

        def dns_reply(data):
            if len(data) < 17:
                raise ValueError("short DNS query")
            pos, labels = 12, []
            while data[pos]:
                size = data[pos]
                pos += 1
                if size > 63 or pos+size >= len(data):
                    raise ValueError("DNS label")
                labels.append(data[pos:pos+size].decode("ascii").lower())
                pos += size
            pos += 1
            qtype, qclass = struct.unpack("!HH", data[pos:pos+4])
            end = pos+4
            name = ".".join(labels)
            with owner.lock:
                owner.witness["dns_questions"].append({"name": name, "type": qtype})
            exists = name == "echo.test" and qclass == 1
            answer = b""
            if exists and qtype == 1:
                answer = b"\xc0\x0c" + struct.pack("!HHIH", 1, 1, 30, 4) + socket.inet_aton("127.0.0.1")
            return data[:2] + struct.pack("!HHHHH", 0x8180 if exists else 0x8183, 1, bool(answer), 0, 0) + data[12:end] + answer

        class DNSUDP(socketserver.BaseRequestHandler):
            def handle(self):
                data, sock = self.request
                sock.sendto(dns_reply(data), self.client_address)

        class DNSTCP(socketserver.BaseRequestHandler):
            def handle(self):
                self.request.settimeout(5)
                try:
                    size = struct.unpack("!H", exact(self.request, 2))[0]
                    answer = dns_reply(exact(self.request, size))
                    self.request.sendall(struct.pack("!H", len(answer)) + answer)
                except (OSError, EOFError):
                    pass

        try:
            self.tcp = self.start(TCPFixture(("127.0.0.1", 0), Echo))
            self.udp = self.start(UDPFixture(("127.0.0.1", 0), Datagram))
            self.download = self.start(TCPFixture(("127.0.0.1", 0), Download))
            # Reserve both DNS protocols before starting either server. A free
            # TCP port may already be in use by UDP, so retry only this bind.
            for attempt in range(8):
                dns_tcp = TCPFixture(("127.0.0.1", 0), DNSTCP)
                try:
                    dns_udp = UDPFixture(dns_tcp.server_address, DNSUDP)
                except OSError:
                    dns_tcp.server_close()
                    if attempt == 7:
                        raise
                    continue
                self.dns = self.start(dns_udp)
                self.start(dns_tcp)
                break
        except BaseException:
            self.close()
            raise

    def start(self, server):
        self.servers.append(server)
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.1})
        thread.start()
        self.threads.append(thread)
        return server.server_address

    def close(self):
        for server in self.servers:
            server.shutdown()
        for server in self.servers:
            server.server_close()
        for thread in self.threads:
            thread.join(timeout=5)
            if thread.is_alive():
                raise RuntimeError("fixture thread did not exit")


class Node:
    def __init__(self, binary, config, run, name):
        self.condition = threading.Condition()
        self.records, self.errors = [], []
        self.name = name
        self.stderr = (run / (name + "-stderr.log")).open("w")
        self.stdout = (run / (name + "-status.jsonl")).open("w")
        self.process = subprocess.Popen([str(binary), "--config", str(config), "--status-interval", "200ms"],
                                        stdout=subprocess.PIPE, stderr=self.stderr, text=True)
        self.thread = threading.Thread(target=self.collect)
        self.thread.start()

    def collect(self):
        try:
            for line in self.process.stdout:
                if len(line) > 65536 or len(self.records) >= 20000:
                    raise RuntimeError("status output exceeded acceptance bound")
                self.stdout.write(line)
                self.stdout.flush()
                value = json.loads(line)
                with self.condition:
                    self.records.append(value)
                    self.condition.notify_all()
        except BaseException as error:
            with self.condition:
                self.errors.append(repr(error))
                self.condition.notify_all()
        finally:
            self.process.stdout.close()

    def wait(self, predicate, timeout=12):
        until = time.monotonic()+timeout
        with self.condition:
            while True:
                if self.errors:
                    raise RuntimeError(self.errors)
                for record in reversed(self.records):
                    if predicate(record):
                        return record
                if self.process.poll() is not None:
                    raise RuntimeError(f"{self.name} exited: {self.process.returncode}")
                remaining = until-time.monotonic()
                if remaining <= 0:
                    raise TimeoutError(f"{self.name} status wait")
                self.condition.wait(min(remaining, 0.1))

    def ready(self):
        return self.wait(lambda r: r["phase"] == "ready" and r["LocalReady"])

    def stop(self, kill=False):
        if self.process.poll() is None:
            self.process.send_signal(signal.SIGKILL if kill else signal.SIGTERM)
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)
            raise RuntimeError(f"{self.name} did not stop gracefully")
        finally:
            self.thread.join(timeout=5)
            self.stdout.close()
            self.stderr.close()
        if self.thread.is_alive() or self.errors:
            raise RuntimeError(f"{self.name} collector failed: {self.errors}")
        if not kill:
            if self.process.returncode != 0:
                raise RuntimeError(f"{self.name} exit: {self.process.returncode}")
            last = self.records[-1]
            if last["phase"] != "stopped" or last["LocalReady"] or last["ActiveStreams"] or last["ActiveCarriers"]:
                raise RuntimeError(f"{self.name} did not release resources: {last}")
            if last["CleanupFailures"]:
                raise RuntimeError(f"{self.name} recorded cleanup failures")
        if self.records:
            identity = self.records[0]["run_id"]
            if any(r["run_id"] != identity or r["pid"] != self.process.pid or r["EndToEnd"] != "not_checked" for r in self.records):
                raise RuntimeError(f"{self.name} status identity/scope changed")
        return {"name": self.name, "pid": self.process.pid, "exit_code": self.process.returncode,
                "expected_kill": kill, "run_id": self.records[0]["run_id"] if self.records else None,
                "status_records": len(self.records), "final_status": self.records[-1] if self.records else None}


class Acceptance:
    def __init__(self, build, run, long_seconds):
        self.build, self.run, self.long_seconds = build, run, long_seconds
        self.veild, self.ctl = build/"linux-amd64/veild", build/"linux-amd64/veilctl"
        self.report = {"schema_version": 1, "scope": "linux_loopback_cli_processes", "passed": False,
                       "long_seconds": long_seconds, "cases": [], "commands": [], "processes": [],
                       "public_vps": "not_tested", "tun": "not_tested", "system_service": "not_tested"}
        self.nodes = []
        self.generation = 0

    def command(self, args, success=True, timeout=20):
        result = subprocess.run([str(self.ctl), *map(str, args)], capture_output=True, text=True, timeout=timeout)
        record = {"command": ["veilctl", *map(str, args)], "exit_code": result.returncode,
                  "stdout": result.stdout, "stderr": result.stderr}
        self.report["commands"].append(record)
        if (result.returncode == 0) != success:
            raise RuntimeError(f"unexpected CLI result: {record}")
        return json.loads(result.stdout) if result.stdout.strip() else None

    def case(self, name, action):
        started = time.monotonic()
        record = {"name": name, "passed": False}
        self.report["cases"].append(record)
        try:
            record["detail"] = action()
            record["passed"] = True
        except BaseException as error:
            record["failure"] = repr(error)
            raise
        finally:
            record["seconds"] = round(time.monotonic()-started, 3)
            save(self.run/"acceptance.json", self.report)
            print(json.dumps({"case": name, "passed": record["passed"], "seconds": record["seconds"]}), flush=True)

    def start_pair(self):
        self.generation += 1
        self.server = Node(self.veild, self.server_config, self.run, f"server-{self.generation}")
        self.nodes.append(self.server)
        server = self.server.ready()
        server_address = server["Listen"]
        value = json.loads(self.server_config.read_text())
        value["listen"] = server_address
        private_json(self.server_config, value)
        value = json.loads(self.client_config.read_text())
        value["dial_address"] = server_address
        private_json(self.client_config, value)
        self.client = Node(self.veild, self.client_config, self.run, f"client-{self.generation}")
        self.nodes.append(self.client)
        client = self.client.ready()
        host, port = client["Listen"].rsplit(":", 1)
        self.socks = (host, int(port))
        if client["EndToEnd"] != "not_checked" or server["EndToEnd"] != "not_checked":
            raise AssertionError("local readiness misrepresented as end-to-end health")
        return {"client_run": client["run_id"], "server_run": server["run_id"]}

    def stop_node(self, node, kill=False):
        if node in self.nodes:
            record = node.stop(kill)
            self.nodes.remove(node)
            self.report["processes"].append(record)

    def stop_pair(self):
        self.stop_node(self.client)
        self.stop_node(self.server)

    def probe(self, domain=False, success=True):
        host = "echo.test" if domain else "127.0.0.1"
        result = self.command(["probe", "--socks", f"{self.socks[0]}:{self.socks[1]}",
                               "--tcp-echo", f"{host}:{self.fixtures.tcp[1]}",
                               "--udp-echo", f"{host}:{self.fixtures.udp[1]}", "--timeout", "10s"], success)
        if result["end_to_end"] != ("passed" if success else "failed"):
            raise AssertionError("incorrect diagnostic scope")
        return result

    def download(self, size, passes, cap=None):
        stream, _ = socks_connect(self.socks, "127.0.0.1", self.fixtures.download[1], timeout=90)
        digest, count = hashlib.sha256(), 0
        expected = hashlib.sha256()
        for offset in range(0, size, len(BLOCK)):
            expected.update(BLOCK[:min(len(BLOCK), size-offset)])
        error_name = None
        with stream:
            stream.sendall(f"{size}\n".encode())
            stream.shutdown(socket.SHUT_WR)
            try:
                while data := stream.recv(65536):
                    count += len(data)
                    digest.update(data)
                    if count > size:
                        raise AssertionError("excess download bytes")
            except (ConnectionResetError, BrokenPipeError) as error:
                error_name = type(error).__name__
        complete = count == size and digest.digest() == expected.digest()
        if complete != passes or not passes and count == 0:
            raise AssertionError(f"download completion: {count}/{size}, expected {passes}")
        if cap is not None and count > cap:
            raise AssertionError(f"received {count} beyond configured cap {cap}")
        if cap is not None and count < cap-min(1<<20, cap//2):
            raise AssertionError(f"transfer failed too early to demonstrate budget exhaustion: {count}/{cap}")
        prefix = hashlib.sha256()
        for offset in range(0, count, len(BLOCK)):
            prefix.update(BLOCK[:min(len(BLOCK), count-offset)])
        if prefix.digest() != digest.digest():
            raise AssertionError("partial download content corrupted")
        return {"requested_bytes": size, "received_bytes": count, "complete": complete,
                "sha256": digest.hexdigest(), "expected_sha256": expected.hexdigest(), "socket_error": error_name}

    def echo(self, index):
        stream, _ = socks_connect(self.socks, "127.0.0.1", self.fixtures.tcp[1])
        payload = f"concurrent-{index}:".encode()*2048
        with stream:
            stream.sendall(payload)
            stream.shutdown(socket.SHUT_WR)
            if exact(stream, len(payload)) != payload or stream.recv(1):
                raise AssertionError("concurrent TCP/half-close mismatch")
        return {"index": index, "verified_bytes": len(payload)}

    def long_flow(self):
        stream, _ = socks_connect(self.socks, "echo.test", self.fixtures.tcp[1])
        control, relay = socks_connect(self.socks, "0.0.0.0", 0, command=3)
        udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        udp.settimeout(8)
        udp.connect(relay)
        count, started = 0, time.monotonic()
        with stream, control, udp:
            while True:
                payload = f"long-{count}".encode()
                stream.sendall(payload)
                if exact(stream, len(payload)) != payload:
                    raise AssertionError("long TCP mismatch")
                packet = b"\0\0\0"+socks_address("127.0.0.1", self.fixtures.udp[1])+payload
                udp.send(packet)
                if udp.recv(1024) != packet:
                    raise AssertionError("long UDP mismatch")
                count += 1
                elapsed = time.monotonic()-started
                if elapsed >= self.long_seconds:
                    break
                if count % 30 == 0:
                    print(json.dumps({"long_flow_operations": count, "elapsed_seconds": round(elapsed)}), flush=True)
                time.sleep(min(1, self.long_seconds-elapsed))
            stream.shutdown(socket.SHUT_WR)
            if stream.recv(1):
                raise AssertionError("long TCP EOF")
        return {"seconds": round(time.monotonic()-started, 3), "tcp_operations": count, "udp_operations": count,
                "same_tcp_connection": True, "same_udp_association": True}

    def set_limits(self, client, server):
        for path, limits in [(self.client_config, client), (self.server_config, server)]:
            value = json.loads(path.read_text())
            value["limits"] = limits
            private_json(path, value)
            checked = self.command(["configcheck", "--config", path])
            if checked["limits"] != {"stream_bytes": limits.get("stream_bytes", 8<<20), "carrier_bytes": limits.get("carrier_bytes", 64<<20)}:
                raise AssertionError("configcheck effective limits mismatch")

    def failure_recovery(self):
        stream, _ = socks_connect(self.socks, "127.0.0.1", self.fixtures.tcp[1])
        stream.settimeout(8)
        with stream:
            stream.sendall(b"before-stop")
            if exact(stream, 11) != b"before-stop":
                raise AssertionError("pre-failure stream")
            self.stop_node(self.server, kill=True)
            # Failure must terminate the original stream; no automatic byte replay.
            try:
                data = stream.recv(1)
            except ConnectionResetError:
                data = b""
            if data:
                raise AssertionError("old stream produced unexpected data after server death")
        self.probe(success=False)
        client_pid = self.client.process.pid
        self.generation += 1
        self.server = Node(self.veild, self.server_config, self.run, f"server-recovery-{self.generation}")
        self.nodes.append(self.server)
        self.server.ready()
        result = self.probe()
        if self.client.process.pid != client_pid:
            raise AssertionError("client unexpectedly restarted")
        return {"client_pid_unchanged": True, "old_stream_terminated": True, "new_flow_probe": result}

    def rejected_material(self, other, model=False):
        self.stop_node(self.client)
        original = json.loads(self.client_config.read_text())
        invalid = dict(original)
        if model:
            invalid["model_file"] = str(other/"client/model.json")
        else:
            invalid["certificate"] = str(other/"client/client.pem")
            invalid["private_key"] = str(other/"client/client-key.pem")
        with self.fixtures.lock:
            before = {key: self.fixtures.witness[key] for key in ("tcp_connections", "udp_packets")}
        private_json(self.client_config, invalid)
        try:
            self.generation += 1
            self.client = Node(self.veild, self.client_config, self.run, f"client-rejected-{self.generation}")
            self.nodes.append(self.client)
            ready = self.client.ready()
            host, port = ready["Listen"].rsplit(":", 1)
            self.socks = host, int(port)
            result = self.probe(success=False)
            self.stop_node(self.client)
        finally:
            private_json(self.client_config, original)
        with self.fixtures.lock:
            after = {key: self.fixtures.witness[key] for key in before}
        if after != before:
            raise AssertionError("rejected identity/model reached an echo target")
        self.generation += 1
        self.client = Node(self.veild, self.client_config, self.run, f"client-restored-{self.generation}")
        self.nodes.append(self.client)
        ready = self.client.ready()
        host, port = ready["Listen"].rsplit(":", 1)
        self.socks = host, int(port)
        return {"rejected_probe": result, "target_witness_unchanged": True, "restored_probe": self.probe()}

    def execute(self):
        manifest = json.loads((self.build/"build.json").read_text())
        current = source_manifest()
        if not manifest["passed"] or manifest["source_sha256"] != current["sha256"]:
            raise RuntimeError("build is not bound to the current source; build again")
        artifacts = {item["path"]: item for item in manifest["artifacts"]}
        for path in (self.veild, self.ctl):
            item = artifacts[str(path.relative_to(self.build))]
            if hashlib.sha256(path.read_bytes()).hexdigest() != item["sha256"]:
                raise RuntimeError("binary hash mismatch")
        self.report.update(version=manifest["version"], source_sha256=current["sha256"],
                           artifacts=[artifacts[str(p.relative_to(self.build))] for p in (self.veild, self.ctl)])
        save(self.run/"source-manifest.json", current)
        with tempfile.TemporaryDirectory(prefix="veil-linux-accept-") as temporary:
            private = Path(temporary)
            self.private_path = private
            self.fixtures = Fixtures()
            try:
                pair = private/"pair"
                args = ["prepare", "--out", pair, "--local-test", "--server-address", "127.0.0.1:24443",
                        "--server-name", "owned.test", "--server-listen", "127.0.0.1:0", "--client-listen", "127.0.0.1:0"]
                self.case("private-pair-preparation", lambda: self.command(args))
                self.case("existing-pair-refused", lambda: self.command(args, success=False))
                self.client_config, self.server_config = pair/"client/node.json", pair/"server/node.json"
                for path in pair.rglob("*"):
                    if path.stat().st_mode & 0o077:
                        raise AssertionError("private bundle permissions")
                value = json.loads(self.server_config.read_text())
                value["dns_address"] = f"127.0.0.1:{self.fixtures.dns[1]}"
                value["deny_cidrs"] = ["127.0.0.2/32"]
                private_json(self.server_config, value)
                self.case("client-configcheck", lambda: self.command(["configcheck", "--config", self.client_config]))
                self.case("server-configcheck", lambda: self.command(["configcheck", "--config", self.server_config]))
                self.client_config.chmod(0o644)
                self.case("unsafe-config-permissions-refused", lambda: self.command(["configcheck", "--config", self.client_config], success=False))
                self.client_config.chmod(0o600)
                self.case("default-pair-start", self.start_pair)
                self.case("numeric-tcp-udp-probe", self.probe)
                self.case("remote-domain-tcp-udp-probe", lambda: self.probe(domain=True))
                self.case("policy-denial", lambda: self.command(["probe", "--socks", f"{self.socks[0]}:{self.socks[1]}",
                          "--tcp-echo", f"127.0.0.2:{self.fixtures.tcp[1]}"], success=False))
                def parallel_flows():
                    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                        return list(pool.map(self.echo, range(4)))
                self.case("concurrent-half-close", parallel_flows)
                self.case("default-stream-budget-enforced", lambda: self.download(9<<20, False, 8<<20))
                self.case("after-budget-new-flows", self.probe)
                other = private/"unrelated-pair"
                other_args = list(args)
                other_args[other_args.index("--out")+1] = other
                self.command(other_args)
                self.case("untrusted-client-identity-refused", lambda: self.rejected_material(other))
                self.case("wrong-model-seed-refused", lambda: self.rejected_material(other, model=True))
                self.case("persistent-tcp-and-udp", self.long_flow)
                self.case("server-death-and-new-flow-recovery", self.failure_recovery)
                self.stop_pair()
                larger = {"stream_bytes": 16<<20, "carrier_bytes": 128<<20}
                self.set_limits(larger, {})
                self.case("asymmetric-pair-start", self.start_pair)
                self.case("peer-minimum-stream-budget-enforced", lambda: self.download(9<<20, False, 8<<20))
                self.stop_pair()
                self.set_limits(larger, larger)
                self.case("explicit-larger-budget-start", self.start_pair)
                self.case("twelve-MiB-download-integrity", lambda: self.download(12<<20, True))
                self.case("larger-budget-tcp-udp-probe", self.probe)
                self.stop_pair()
                small_carrier = {"stream_bytes": 16<<20, "carrier_bytes": 1<<20}
                self.set_limits(small_carrier, small_carrier)
                self.case("small-carrier-start", self.start_pair)
                self.case("carrier-budget-enforced", lambda: self.download(2<<20, False, 1<<20))
                self.case("after-carrier-budget-new-flows", self.probe)
                self.stop_pair()
                questions = self.fixtures.witness["dns_questions"]
                if not any(q["name"] == "echo.test" and q["type"] == 1 for q in questions) or not any(q["type"] == 28 for q in questions):
                    raise AssertionError("missing server-side dual-family DNS witness")
                self.report["passed"] = True
            finally:
                failures = []
                for node in list(reversed(self.nodes)):
                    try:
                        self.stop_node(node)
                    except BaseException as error:
                        failures.append(repr(error))
                self.fixtures.close()
                save(self.run/"target-witness.json", self.fixtures.witness)
                if failures:
                    self.report["passed"] = False
                    self.report["cleanup_failures"] = failures


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--build", required=True, type=Path)
    parser.add_argument("--long-seconds", type=int, default=260, help="5..600; below 260 is a pilot only")
    args = parser.parse_args()
    if sys.platform != "linux" or not 5 <= args.long_seconds <= 600:
        parser.error("requires Linux and long-seconds 5..600")
    run = new_run("linux-acceptance")
    acceptance = Acceptance(args.build.resolve(), run, args.long_seconds)
    acceptance.report["pilot"] = args.long_seconds < 260
    print(run.relative_to(ROOT), flush=True)
    try:
        acceptance.execute()
    except BaseException as error:
        acceptance.report["passed"] = False
        acceptance.report["failure"] = repr(error)
    finally:
        acceptance.report["private_material_removed"] = hasattr(acceptance, "private_path") and not acceptance.private_path.exists()
        if not acceptance.report["private_material_removed"]:
            acceptance.report["passed"] = False
        save(run/"acceptance.json", acceptance.report)
    if not acceptance.report["passed"]:
        print(json.dumps(acceptance.report.get("failure", acceptance.report.get("cleanup_failures"))), file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
