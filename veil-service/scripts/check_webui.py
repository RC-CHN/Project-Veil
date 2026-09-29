#!/usr/bin/env python3
"""Shared UI syntax and Chinese translation coverage for every platform build."""

import ast
import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]


def check():
    common = ROOT / "webui"
    opnsense = ROOT / "platform/opnsense/src/opnsense"
    labels = set()
    for path in (common / "connections.js", opnsense / "www/js/veil/page.js"):
        subprocess.run(["node", "--check", str(path)], check=True)
        labels.update(ast.literal_eval(s) for s in re.findall(
            r"(?:_|tr)\(\s*('(?:[^'\\]|\\.)*')\s*,?\s*\)", path.read_text(encoding="utf-8")))
    labels.update(re.findall(r'data-i18n="([^"]+)"',
        (opnsense / "mvc/app/views/OPNsense/Veil/index.volt").read_text(encoding="utf-8")))
    source = (common / "i18n.js").read_text(encoding="utf-8")
    translations = json.loads(source[source.index("{"):].rstrip().removesuffix(";"))
    missing = [label for label in sorted(labels) if not translations["zh"].get(label)]
    if missing:
        raise ValueError(f"Shared Chinese labels missing: {missing}")
    subprocess.run(["node", "--check", str(common / "i18n.js")], check=True)
    print(f"Shared UI syntax and {len(labels)} bilingual labels passed.")


if __name__ == "__main__":
    check()
