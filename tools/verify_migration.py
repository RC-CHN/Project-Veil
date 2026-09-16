#!/usr/bin/env python3
"""Read-only checks of imported source preservation and frozen key binaries."""
from pathlib import Path
import hashlib
import json
from common import ROOT


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()

def main():
    manifest = json.loads((ROOT/"docs/migration/initial-import.json").read_text())
    failures = []
    for item in manifest["files"]:
        src = ROOT/"references/veil"/item["source"]
        if not src.is_file() or digest(src) != item["source_sha256"]:
            failures.append(str(src.relative_to(ROOT)))
        if not (ROOT/item["target"]).is_file():
            failures.append("missing migrated target: " + item["target"])
    frozen = {
        "veil-v4": "efdbcbbf8d6259977e7c363e54f2b1880a859ed5cf46e7ceb59943f73137b5c7",
        "veil-generation-runtime": "8b0a1e0d971b3524cab52a1a50d60a1d87c234998c63553edf2316e6b0e941ec",
        "sing-box-veil-tun-v7-dns2": "7b1d38e0fac222b335cd964fe05f23951895df449eed7ae2a7c118f1bff3a44b",
    }
    for name, expected in frozen.items():
        path = ROOT/"references/veil/bin"/name
        if not path.is_file() or digest(path) != expected:
            failures.append(str(path.relative_to(ROOT)))
    print(json.dumps({"source_files": len(manifest["files"]), "frozen_binaries": len(frozen),
                      "passed": not failures, "failures": failures}, indent=2))
    raise SystemExit(bool(failures))

if __name__ == "__main__":
    main()
