"""Shared build/check helpers. All paths derive from the formal repository root."""
from pathlib import Path
import datetime
import hashlib
import json
import os
import re
import shutil
import subprocess
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]

def go_binary(value=None):
    candidate = value or os.environ.get("VEIL_GO") or shutil.which("go")
    if not candidate:
        raise SystemExit("Go 1.26.7 required; set VEIL_GO or add go to PATH")
    return candidate

def environment(**extra):
    env = dict(os.environ)
    env.update(GOWORK="off")
    env.update(extra)
    return env

def source_manifest():
    paths = [ROOT / "go.work", ROOT / "Makefile"]
    paths += list((ROOT / "tools").glob("*.py"))
    for directory in ("core", "node"):
        paths += [p for p in (ROOT / directory).rglob("*")
                  if p.is_file() and (p.suffix in (".go", ".mod", ".sum")
                                     or "testdata" in p.parts)]
    files = [{"path": str(p.relative_to(ROOT)).replace(os.sep, "/"),
              "sha256": hashlib.sha256(p.read_bytes()).hexdigest()}
             for p in sorted(paths)]
    digest = hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest()
    return {"sha256": digest, "files": files}

def version():
    match = re.search(r'^const Version\s*=\s*"([^"]+)"',
                      (ROOT / "core/options.go").read_text(), re.M)
    if not match:
        raise SystemExit("core version constant not found")
    return match.group(1)

def new_run(kind):
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    directory = ROOT / "out" / kind / (stamp + "-" + uuid.uuid4().hex[:8])
    directory.mkdir(parents=True, exist_ok=False)
    return directory

def command(argv, cwd, env, logfile):
    start = time.monotonic()
    try:
        result = subprocess.run(argv, cwd=cwd, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=300)
        code, output = result.returncode, result.stdout
    except subprocess.TimeoutExpired as exc:
        code, output = 124, exc.stdout or ""
        if isinstance(output, bytes):
            output = output.decode(errors="replace")
        output += "\nCommand exceeded 300 seconds.\n"
    logfile.write_text(output)
    if code:
        print(output, end="")
    return {"command": argv, "cwd": str(Path(cwd).relative_to(ROOT)),
            "exit_code": code, "seconds": round(time.monotonic()-start, 3),
            "log": logfile.name}
