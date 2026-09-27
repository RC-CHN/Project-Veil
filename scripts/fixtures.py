"""Create only local test credentials, configurations, and subprocesses."""

import json
import socket
import subprocess
import time

from build import ENV, ROOT


def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def save(path, obj):
    path.write_text(json.dumps(obj, indent=2) + "\n")
    path.chmod(0o600)
    return path


def fixture(folder, rsa=False):
    folder.mkdir(parents=True, exist_ok=True)
    cert, key = folder / "cert.pem", folder / "key.pem"
    subprocess.run(
        [
            "openssl",
            "req",
            "-x509",
            "-newkey",
            *(["rsa:2048"] if rsa else ["ec", "-pkeyopt", "ec_paramgen_curve:P-256"]),
            "-nodes",
            "-keyout",
            str(key),
            "-out",
            str(cert),
            "-days",
            "1",
            "-subj",
            "/CN=cover.test",
            "-addext",
            "subjectAltName=DNS:cover.test",
        ],
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    key.chmod(0o600)
    keys = json.loads(
        subprocess.check_output([str(ROOT / ".build/veil-native"), "-keygen"])
    )
    return dict(folder=folder, cert=str(cert), key=str(key), keys=keys)


def spawn(cmd, cores=None, procs=1, **kw):
    env = ENV | {"GOMAXPROCS": str(procs)}
    if cores:
        cmd = ["taskset", "-c", cores, *map(str, cmd)]
    return subprocess.Popen(list(map(str, cmd)), env=env, **kw)


def wait_port(p, proc):
    for _ in range(300):
        if proc.poll() is not None:
            raise RuntimeError("process exited: " + str(proc.args))
        try:
            with socket.create_connection(("127.0.0.1", p), 0.05):
                return
        except OSError:
            time.sleep(0.01)
    raise TimeoutError("listener did not start")


def stop(proc):
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


def configurations(f, variant, sp, cp, cover):
    k = f["keys"]
    reality = "reality" in variant
    if variant.startswith("veil"):
        st = {"mode": "reality" if reality else "tls", "server_name": "cover.test"}
        ct = st.copy()
        if reality:
            st.update(
                reality_private_key=k["reality_private_key"],
                short_id=k["short_id"],
                cover_address=f"127.0.0.1:{cover}",
            )
            ct.update(
                reality_public_key=k["reality_public_key"], short_id=k["short_id"]
            )
        else:
            st.update(certificate=f["cert"], private_key_file=f["key"])
            ct.update(ca_file=f["cert"])
        server = {
            "role": "server",
            "listen": f"127.0.0.1:{sp}",
            "secret": k["secret"],
            "tls": st,
            "max_connections": 128,
            "idle_seconds": 60,
        }
        client = {
            "role": "client",
            "listen": f"127.0.0.1:{cp}",
            "server": f"127.0.0.1:{sp}",
            "secret": k["secret"],
            "tls": ct,
            "max_connections": 128,
            "idle_seconds": 60,
        }
        sb = "veil-openssl" if "opt" in variant else "veil-native"
        cb = "veil-batch" if "opt" in variant else "veil-native"
        configflag = "-config"
    else:
        st = {
            "enabled": True,
            "server_name": "cover.test",
            "min_version": "1.3",
            "max_version": "1.3",
        }
        ct = st.copy()
        if reality:
            st["reality"] = {
                "enabled": True,
                "handshake": {"server": "127.0.0.1", "server_port": cover},
                "private_key": k["reality_private_key"],
                "short_id": [k["short_id"]],
                "max_time_difference": "1m",
            }
            ct.update(
                utls={"enabled": True, "fingerprint": "chrome"},
                reality={
                    "enabled": True,
                    "public_key": k["reality_public_key"],
                    "short_id": k["short_id"],
                },
            )
        else:
            st.update(certificate_path=f["cert"], key_path=f["key"])
            ct.update(certificate_path=f["cert"])
        server = {
            "log": {"level": "error"},
            "inbounds": [
                {
                    "type": "anytls",
                    "listen": "127.0.0.1",
                    "listen_port": sp,
                    "users": [{"name": "test", "password": k["secret"]}],
                    "tls": st,
                }
            ],
            "outbounds": [{"type": "direct"}],
        }
        client = {
            "log": {"level": "error"},
            "inbounds": [{"type": "socks", "listen": "127.0.0.1", "listen_port": cp}],
            "outbounds": [
                {
                    "type": "anytls",
                    "server": "127.0.0.1",
                    "server_port": sp,
                    "password": k["secret"],
                    "tls": ct,
                }
            ],
        }
        sb = "singbox-openssl" if "opt" in variant else "singbox-native"
        cb = "singbox-batch" if "opt" in variant else "singbox-native"
        configflag = "-c"
    sf = save(f["folder"] / "server.json", server)
    cf = save(f["folder"] / "client.json", client)
    prefix = [] if variant.startswith("veil") else ["run"]
    binaries = ROOT / ".build"
    if variant.endswith("-previous"):
        binaries /= "previous"
    return [binaries / sb, *prefix, configflag, sf], [
        binaries / cb,
        *prefix,
        configflag,
        cf,
    ]
