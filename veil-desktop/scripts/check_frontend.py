#!/usr/bin/env python3
"""Fast syntax and translation checks; native GUI acceptance is run separately."""

import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]
for script in ("app.js", "i18n.js"):
    subprocess.run(["node", "--check", str(ROOT / "frontend" / script)], check=True)
html = (ROOT / "frontend/index.html").read_text(encoding="utf-8")
app = (ROOT / "frontend/app.js").read_text(encoding="utf-8")
labels = set(re.findall(r'data-i18n="([A-Za-z]+)"', html))
labels.update(key for _, key in re.findall(r'''\bt\((["'])([A-Za-z]+)\1\)''', app))

subprocess.run(
    [
        "node",
        "-e",
        """
const fs = require('fs'), vm = require('vm'), assert = require('assert/strict');
const context = { window: {} };
vm.runInNewContext(fs.readFileSync(process.argv[1], 'utf8'), context);
const { en, zh } = context.window.messages;
assert.deepEqual(Object.keys(en).sort(), Object.keys(zh).sort());
for (const key of JSON.parse(process.argv[2])) {
  assert.ok(en[key] && zh[key], 'Missing translation: ' + key);
}
console.log('Desktop English/Chinese labels and JS syntax passed.');
""",
        str(ROOT / "frontend/i18n.js"),
        json.dumps(sorted(labels)),
    ],
    check=True,
)

subprocess.run(["python3", str(ROOT.parent / "veil-service/scripts/check_webui.py")], check=True)
