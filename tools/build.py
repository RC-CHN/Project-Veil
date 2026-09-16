#!/usr/bin/env python3
"""Build independent Linux/Windows artifacts without overwriting older runs."""
import argparse
import hashlib
import json
import subprocess
from common import ROOT, go_binary, environment, source_manifest, version, new_run, command


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--go")
    parser.add_argument("--target", action="append", choices=["linux/amd64", "windows/amd64"])
    args = parser.parse_args()
    go = go_binary(args.go)
    targets = args.target or ["linux/amd64", "windows/amd64"]
    run = new_run("builds")
    source = source_manifest()
    (run / "source-manifest.json").write_text(json.dumps(source, indent=2)+"\n")
    report = {"version": version(), "source_sha256": source["sha256"],
              "go": subprocess.check_output([go, "version"], text=True).strip(),
              "commands": [], "artifacts": []}
    for target in targets:
        system, arch = target.split("/")
        folder = run / f"{system}-{arch}"
        folder.mkdir()
        for name in ("veild", "veilctl"):
            artifact = folder / (name + (".exe" if system == "windows" else ""))
            cmd = [go, "build", "-trimpath", "-buildvcs=false", "-o", str(artifact), f"./cmd/{name}"]
            entry = command(cmd, ROOT/"node", environment(GOOS=system, GOARCH=arch, CGO_ENABLED="0"),
                            run / f"{system}-{arch}-{name}.log")
            report["commands"].append(entry)
            if entry["exit_code"]:
                report["passed"] = False
                (run/"build.json").write_text(json.dumps(report, indent=2)+"\n")
                raise SystemExit(entry["exit_code"])
            report["artifacts"].append({"path": str(artifact.relative_to(run)),
                                        "sha256": hashlib.sha256(artifact.read_bytes()).hexdigest(),
                                        "bytes": artifact.stat().st_size})
    report["passed"] = True
    (run/"build.json").write_text(json.dumps(report, indent=2)+"\n")
    print(run.relative_to(ROOT))

if __name__ == "__main__":
    main()
