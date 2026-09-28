"""Small CI routing/check helpers; builds and tests stay in component scripts."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys

SCOPES = ("core", "service", "interop", "cross", "windows", "desktop")


def scopes(paths):
    selected = set()
    for path in paths:
        if path == "PROTOCOL.md":
            selected.add("interop")
        elif path.endswith(".md") or path == "LICENSE":
            continue
        elif path.startswith("veil-core/"):
            selected.update(SCOPES)
        elif path.startswith("veil-service/"):
            selected.update(("service", "cross", "windows", "desktop"))
        elif path.startswith("veil-desktop/"):
            selected.add("desktop")
        elif path.startswith("interop/rust/"):
            selected.add("interop")
        else:
            # CI/toolchain changes and new components fail open to all checks.
            selected.update(SCOPES)
    return {name: name in selected for name in SCOPES}


def changes():
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    kind = os.environ["GITHUB_EVENT_NAME"]
    if kind == "pull_request":
        base = event["pull_request"]["base"]["sha"]
    else:
        base = event.get("before", "")
    selected = dict.fromkeys(SCOPES, True)
    if (
        kind in ("push", "pull_request")
        and re.fullmatch(r"[0-9a-f]{40}", base)
        and int(base, 16)
    ):
        diff = subprocess.run(
            ["git", "diff", "--no-renames", "--name-only", "-z", base, "HEAD", "--"],
            capture_output=True,
        )
        if diff.returncode == 0:
            selected = scopes(os.fsdecode(p) for p in diff.stdout.split(b"\0") if p)
        else:
            print("Base absent from shallow checkout; running all checks.")
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        for name, enabled in selected.items():
            output.write(f"{name}={str(enabled).lower()}\n")
    print(json.dumps(selected))


def check_gate(needs):
    if needs["changes"]["result"] != "success":
        raise ValueError("change detection failed or was cancelled")
    selected = needs["changes"]["outputs"]
    for name in SCOPES:
        required = selected[name]
        result = needs[name]["result"]
        if required not in ("true", "false"):
            raise ValueError(f"invalid routing output: {name}")
        if result != "success" and not (required == "false" and result == "skipped"):
            raise ValueError(f"{name}: required={required}, result={result}")


def gate():
    needs = json.loads(os.environ["CI_NEEDS"])
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
        summary.write("| Check | Result |\n| --- | --- |\n")
        for name, job in needs.items():
            summary.write(f"| {name} | {job['result']} |\n")
    check_gate(needs)


def vectors(path):
    document = Path("PROTOCOL.md").read_text()
    section = document.split("## 11. 固定测试向量\n", 1)[1]
    expected = json.loads(section.split("```json\n", 1)[1].split("\n```", 1)[0])
    actual = json.loads(Path(path).read_text())
    if actual != expected:
        raise ValueError(
            "Rust vectors differ from the standalone protocol specification"
        )
    print("Protocol vectors match the independent Rust peer.")


if __name__ == "__main__":
    if sys.argv[1:] == ["changes"]:
        changes()
    elif sys.argv[1:] == ["gate"]:
        gate()
    elif len(sys.argv) == 3 and sys.argv[1] == "vectors":
        vectors(sys.argv[2])
    else:
        sys.exit("usage: ci.py changes | gate | vectors JSON_FILE")
