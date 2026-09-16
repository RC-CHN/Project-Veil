#!/usr/bin/env python3
"""Test the new modules; Windows mode only cross-builds, never claims execution."""
import argparse
import json
from common import ROOT, go_binary, environment, source_manifest, new_run, command


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--go")
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--windows", action="store_true")
    parser.add_argument("--windows-only", action="store_true")
    args = parser.parse_args()
    go = go_binary(args.go)
    run = new_run("checks")
    source = source_manifest()
    (run/"source-manifest.json").write_text(json.dumps(source, indent=2)+"\n")
    report = {"source_sha256": source["sha256"], "commands": []}
    if not args.windows_only:
        for module in ("core", "node"):
            argv = [go, "test", "-count=1", "-timeout=120s"]
            if args.race:
                argv.append("-race")
            argv.append("./...")
            env = environment()
            if args.race:
                env["CGO_ENABLED"] = "1"
            report["commands"].append(command(argv, ROOT/module, env, run/f"{module}-tests.log"))
    if args.windows or args.windows_only:
        for module in ("core", "node"):
            report["commands"].append(command([go, "build", "./..."], ROOT/module,
                environment(GOOS="windows", GOARCH="amd64", CGO_ENABLED="0"), run/f"{module}-windows-build.log"))
    report["passed"] = all(c["exit_code"] == 0 for c in report["commands"])
    (run/"checks.json").write_text(json.dumps(report, indent=2)+"\n")
    print(run.relative_to(ROOT))
    raise SystemExit(0 if report["passed"] else 1)

if __name__ == "__main__":
    main()
