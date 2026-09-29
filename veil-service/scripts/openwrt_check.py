#!/usr/bin/env python3
"""Check LuCI syntax, translation coverage and the read-only ACL without an SDK."""

from check_webui import check as check_webui

import ast
import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]
APP = ROOT / "platform/openwrt/luci-app-veil"


def catalog(path):
    entries = {}
    key, value, field = None, "", None
    for line in [*path.read_text().splitlines(), ""]:
        if not line.strip():
            if key:
                if key in entries:
                    raise ValueError(f"duplicate translation: {key}")
                entries[key] = value
            key, value, field = None, "", None
        elif line.startswith("#"):
            continue
        elif line.startswith("msgid "):
            key, field = ast.literal_eval(line[6:]), "key"
        elif line.startswith("msgstr "):
            value, field = ast.literal_eval(line[7:]), "value"
        elif line.startswith('"') and field:
            if field == "key":
                key += ast.literal_eval(line)
            else:
                value += ast.literal_eval(line)
        else:
            raise ValueError(f"unsupported catalog entry: {line}")
    return entries


def main():
    check_webui()
    source = APP / "htdocs/luci-static/resources/view/veil.js"
    subprocess.run(["node", "--check", str(source)], check=True)
    subprocess.run(["node", "--check", str(ROOT / "scripts/luci_smoke.cjs")], check=True)
    labels = {
        ast.literal_eval(match)
        for match in re.findall(r"_\(\s*('(?:[^'\\]|\\.)*')\s*,?\s*\)", source.read_text())
    }
    for relative in ("po/templates/veil.pot", "po/zh_Hans/veil.po"):
        entries = catalog(APP / relative)
        missing = labels - entries.keys()
        if missing:
            raise ValueError(f"{relative}: untranslated labels: {sorted(missing)}")
        if relative.endswith(".po") and any(not entries[label] for label in labels):
            raise ValueError("Chinese translations must not be empty")
    acl = json.loads((APP / "root/usr/share/rpcd/acl.d/luci-app-veil.json").read_text())
    read = acl["luci-app-veil"]["read"]
    if read != {"ubus": {"veil": ["status", "connections"]}}:
        raise ValueError("read-only LuCI must not expose credentials or mutations")
    for source in (APP / "root").rglob("*.json"):
        json.loads(source.read_text())
    print(f"LuCI syntax, {len(labels)} bilingual labels and read-only ACL passed.")


if __name__ == "__main__":
    main()
