#!/usr/bin/env python3
"""Cheap OPNsense interface, translation and permission checks; no VM download."""

from check_webui import check as check_webui

import json
from pathlib import Path
import re
import subprocess
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
PLUGIN = ROOT / "platform/opnsense"


def main():
    app = PLUGIN / "src/opnsense"
    page = app / "www/js/veil/page.js"
    view = app / "mvc/app/views/OPNsense/Veil/index.volt"
    check_webui()
    subprocess.run(["node", "--check", str(page)], check=True)
    subprocess.run(["node", "--check", str(ROOT / "scripts/opnsense_smoke.cjs")], check=True)
    for source in PLUGIN.rglob("*.xml"):
        ET.parse(source)
    acl = ET.parse(app / "mvc/app/models/OPNsense/Veil/ACL/ACL.xml")
    read = {node.text for node in acl.findall("./page-veil-status/patterns/pattern")}
    if read != {"ui/veil/*", "api/veil/service/status", "api/veil/service/connections"}:
        raise ValueError("Status permission must not grant credential reads or mutation")
    for source in [PLUGIN / "src/etc/rc.syshook.d/start/91-veil", *PLUGIN.glob("+*")]:
        subprocess.run(["sh", "-n", str(source)], check=True)
    print("OPNsense syntax and status ACL passed.")


if __name__ == "__main__":
    main()
