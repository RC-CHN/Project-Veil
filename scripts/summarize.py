#!/usr/bin/env python3
"""Compare trial medians, with independent exploratory bootstrap intervals."""

import argparse
import collections
import json
import pathlib
import random
import statistics

p = argparse.ArgumentParser()
p.add_argument("raw")
p.add_argument("--output")
a = p.parse_args()
rows = [
    json.loads(s) for s in pathlib.Path(a.raw).read_text().splitlines() if s.strip()
]
groups = collections.defaultdict(list)
for r in rows:
    groups[(r["variant"], r["mode"], r["connections"])].append(r)
rng = random.Random(20260927)
out = []
for (variant, mode, n), rs in sorted(groups.items()):
    if variant != "veil-opt-reality":
        continue
    result = {
        "mode": mode,
        "connections": n,
        "repetitions": len(rs),
        "veil_cpu_efficiency": statistics.median(r["gib_per_cpu_second"] for r in rs),
        "veil_wall_gib_s": statistics.median(r["gib_per_second"] for r in rs),
        "veil_server_rss_mib": statistics.median(
            r["server_rss_bytes"] / 2**20 for r in rs
        ),
        "comparisons": {},
    }
    for name in [
        "anytls-native-tls",
        "anytls-native-reality",
        "anytls-opt-reality",
        "veil-native-reality",
    ]:
        base = groups.get((name, mode, n))
        if not base:
            continue
        metric = "gib_per_cpu_second" if mode in ("U", "D") else "p99_us"
        x = [r[metric] for r in rs]
        y = [r[metric] for r in base]
        ratios = sorted(
            statistics.median(rng.choices(x, k=len(x)))
            / statistics.median(rng.choices(y, k=len(y)))
            for _ in range(10000)
        )
        result["comparisons"][name] = {
            "metric": metric,
            "baseline_median": statistics.median(y),
            "candidate_median": statistics.median(x),
            "ratio": statistics.median(x) / statistics.median(y),
            "bootstrap_95_ratio": [ratios[249], ratios[9749]],
            "baseline_wall_gib_s": statistics.median(r["gib_per_second"] for r in base),
        }
    out.append(result)
text = json.dumps(out, indent=2) + "\n"
if a.output:
    pathlib.Path(a.output).write_text(text)
else:
    print(text)
