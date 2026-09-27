#!/usr/bin/env python3
"""Offline audit of split_http.py metadata. No capture payloads are required."""

import argparse
import json
import pathlib
import statistics


def median(values):
    return round(statistics.median(values), 4)


def length_matches(truth, events, direction, window):
    # Match chronologically and consume each outer record at most once. Parallel
    # handshakes may have identical lengths; reusing a match inflates the count.
    used, lags = set(), []
    for inner in sorted(truth, key=lambda e: e["time"]):
        for i, outer in enumerate(events):
            lag = (inner["time"] - outer["time"]) * (1 if direction == "up" else -1)
            if (
                i not in used
                and outer["direction"] == direction
                and outer["type"] == 23
                and outer["bytes"] == inner["bytes"] + 31
                and 0 <= lag < window
            ):
                used.add(i)
                lags.append(lag)
                break
    return lags


def leakage(rows, delay):
    # This deliberately uses the inner trace as ground truth. It is an audit of
    # length preservation, NOT a passive classifier or a detection-rate estimate.
    result = []
    for group in ("shared", "split"):
        counts = dict(
            client_hellos=0, hello_plus_31=0, server_flights=0, flight_plus_31=0
        )
        lags = []
        for row in (r for r in rows if r["group"] == group):
            hellos, flights = [], []
            for c in {e["connection"] for e in row["inner"]}:
                inner = [e for e in row["inner"] if e["connection"] == c]
                hellos.append(
                    next(e for e in inner if e["direction"] == "up" and e["type"] == 22)
                )
                first_down = next(
                    i for i, e in enumerate(inner) if e["direction"] == "down"
                )
                flight = []
                for e in inner[first_down:]:
                    if e["direction"] == "up":
                        break
                    flight.append(e)
                flights.append(
                    dict(time=flight[-1]["time"], bytes=sum(e["bytes"] for e in flight))
                )
            hello_lags = length_matches(hellos, row["events"], "up", 10 + 2 * delay)
            flight_lags = length_matches(flights, row["events"], "down", 10 + 2 * delay)
            counts["client_hellos"] += len(hellos)
            counts["hello_plus_31"] += len(hello_lags)
            counts["server_flights"] += len(flights)
            counts["flight_plus_31"] += len(flight_lags)
            lags.extend(hello_lags)
        result.append(
            dict(group=group, **counts, hello_lag_ms=median(lags) if lags else None)
        )
    return result


def shapes(folder):
    rows = json.loads((folder / "samples.json").read_text())
    meta = json.loads((folder / "metadata.json").read_text())
    summary = []
    for protocol in ("h2", "h1-close"):
        for parallel in (False, True):
            for group in ("https", "veil", "shared", "split"):
                subset = [
                    r
                    for r in rows
                    if (r["protocol"], r["parallel"], r["group"])
                    == (protocol, parallel, group)
                ]
                summary.append(
                    dict(
                        protocol=protocol,
                        parallel=parallel,
                        group=group,
                        samples=len(subset),
                        turns=median([r["merged"]["turns"] for r in subset]),
                        ttfb_ms=median(
                            [r["browser"]["navigation"]["ttfb_ms"] for r in subset]
                        ),
                        connections=median([len(r["separate"]) for r in subset]),
                    )
                )
    return dict(
        folder=str(folder),
        summary=summary,
        length_audit=leakage(rows, meta["args"]["delay_ms"]),
    )


def perf(folder):
    rows = json.loads((folder / "samples.json").read_text())
    result = []
    for mode in ("U", "D", "E", "S", "C"):
        for group in ("veil", "shared", "split"):
            subset = [r for r in rows if r["mode"] == mode and r["group"] == group]
            baseline = {
                r["pair"]: r for r in rows if r["mode"] == mode and r["group"] == "veil"
            }
            result.append(
                dict(
                    group=group,
                    mode=mode,
                    samples=len(subset),
                    seconds=median([r["seconds"] for r in subset]),
                    cpu_ms=median([r["cpu_ms"] for r in subset]),
                    cpu_efficiency_vs_veil=median(
                        [
                            r["efficiency"] / baseline[r["pair"]]["efficiency"]
                            for r in subset
                        ]
                    ),
                    wall_rate_vs_veil=median(
                        [baseline[r["pair"]]["seconds"] / r["seconds"] for r in subset]
                    ),
                )
            )
    return dict(folder=str(folder), summary=result)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("shapes", type=pathlib.Path, nargs="+")
    p.add_argument("--perf", type=pathlib.Path)
    args = p.parse_args()
    report = dict(shapes=[shapes(f) for f in args.shapes])
    if args.perf:
        report["perf"] = perf(args.perf)
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
