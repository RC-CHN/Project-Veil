#!/usr/bin/env python3
"""Compare owned REALITY ClientHellos and TLS record sequences, not detectability.
Captures are parsed in memory; results exclude raw session IDs and key shares.
"""

import argparse
import collections
import hashlib
import json

from build import ROOT
from fixtures import fixture
from probe import Capture
from reality_benchmark import trial


def records(data):
    result = []
    while data:
        if len(data) < 5:
            raise ValueError("truncated TLS header")
        n = int.from_bytes(data[3:5], "big")
        if len(data) < 5 + n:
            raise ValueError("truncated TLS record")
        result.append((data[0], data[5 : 5 + n]))
        data = data[5 + n :]
    return result


def grease(v):
    return 0x0A0A if (v & 0x0F0F) == 0x0A0A and v >> 8 == v & 255 else v


def words(data):
    if len(data) % 2:
        raise ValueError("odd uint16 vector")
    return [
        grease(int.from_bytes(data[i : i + 2], "big")) for i in range(0, len(data), 2)
    ]


def hello(data):
    if data[0] != 1 or int.from_bytes(data[1:4], "big") != len(data) - 4:
        raise ValueError("expected one complete ClientHello")
    body = data[4:]
    random = body[2:34]
    sid_len = body[34]
    sid = body[35 : 35 + sid_len]
    p = 35 + sid_len
    n = int.from_bytes(body[p : p + 2], "big")
    ciphers = words(body[p + 2 : p + 2 + n])
    p += 2 + n
    n = body[p]
    compression = list(body[p + 1 : p + 1 + n])
    p += 1 + n
    n = int.from_bytes(body[p : p + 2], "big")
    p += 2
    if p + n != len(body):
        raise ValueError("extension length mismatch")
    extensions, order, lengths = {}, [], {}
    share_hashes = []
    while p < len(body):
        typ = grease(int.from_bytes(body[p : p + 2], "big"))
        n = int.from_bytes(body[p + 2 : p + 4], "big")
        value = body[p + 4 : p + 4 + n]
        if len(value) != n:
            raise ValueError("truncated extension")
        p += 4 + n
        order.append(typ)
        lengths[typ] = n
        if typ == 0x0A0A:
            continue
        if typ == 51:
            shares, offset = [], 2
            while offset < len(value):
                group = grease(int.from_bytes(value[offset : offset + 2], "big"))
                size = int.from_bytes(value[offset + 2 : offset + 4], "big")
                public = value[offset + 4 : offset + 4 + size]
                if len(public) != size:
                    raise ValueError("truncated key share")
                shares.append([group, size])
                if group != 0x0A0A:
                    share_hashes.append(hashlib.sha256(public).hexdigest())
                offset += 4 + size
            extensions[typ] = shares
        elif typ in (10, 13):
            extensions[typ] = words(value[2:])
        elif typ == 43:
            extensions[typ] = words(value[1:])
        elif typ in (21, 65037):
            # Padding and GREASE ECH contain intentionally variable material.
            extensions[typ] = "variable"
        else:
            extensions[typ] = value.hex()
    return {
        "profile": {
            "legacy_version": body[:2].hex(),
            "ciphers": ciphers,
            "session_id_length": sid_len,
            "compression": compression,
            "extension_ids": sorted(order),
            "extensions": extensions,
        },
        "extension_order": order,
        "extension_lengths": lengths,
        "random_sha256": hashlib.sha256(random).hexdigest(),
        "session_id_sha256": hashlib.sha256(sid).hexdigest(),
        "key_share_sha256": share_hashes,
        "bytes": len(data),
    }


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--before", required=True)
    p.add_argument("--after", default=".build")
    p.add_argument("--out", required=True)
    p.add_argument("--samples", type=int, default=12)
    a = p.parse_args()
    if a.samples < 2:
        p.error("at least two samples required")
    folder = ROOT / a.out
    if folder.exists():
        raise SystemExit("use a fresh output directory")
    f = fixture(folder)
    results = {"before": [], "after": []}
    binaries = {"before": ROOT / a.before, "after": ROOT / a.after}
    for repeat in range(a.samples):
        for version in ("before", "after"):
            row = trial(
                f,
                binaries[version],
                "H",
                1,
                folder / f"{repeat:03d}-{version}",
                capture=Capture,
            )
            raw = {d: bytes.fromhex(b) for d, b in row.pop("capture").items()}
            parsed = {d: records(b) for d, b in raw.items()}
            client_hello = hello(parsed["up"][0][1])
            shapes = {d: [[typ, len(b)] for typ, b in rs] for d, rs in parsed.items()}
            results[version].append(
                {
                    "hello": client_hello,
                    "records": shapes,
                    "server_stats": row["server_stats"],
                }
            )
            print(
                f"{repeat + 1}/{a.samples} {version}: downstream tail={shapes['down'][-4:]}",
                flush=True,
            )
    profiles = {
        json.dumps(r["hello"]["profile"], sort_keys=True)
        for rows in results.values()
        for r in rows
    }
    summary = {
        "normalized_clienthello_equal": len(profiles) == 1,
        "scope": "owned loopback; record lengths exclude five-byte TLS headers; no censorship classification test",
    }
    for version, rows in results.items():
        summary[version] = {
            "samples": len(rows),
            "clienthello_bytes": dict(
                collections.Counter(r["hello"]["bytes"] for r in rows)
            ),
            "downstream_tail": dict(
                collections.Counter(json.dumps(r["records"]["down"][-4:]) for r in rows)
            ),
            "fresh_randoms": len({r["hello"]["random_sha256"] for r in rows})
            == len(rows),
            "fresh_session_ids": len({r["hello"]["session_id_sha256"] for r in rows})
            == len(rows),
            "fresh_key_shares": len(
                {tuple(r["hello"]["key_share_sha256"]) for r in rows}
            )
            == len(rows),
        }
    (folder / "result.json").write_text(
        json.dumps({"summary": summary, "samples": results}, indent=2) + "\n"
    )
    print(json.dumps(summary, indent=2))
