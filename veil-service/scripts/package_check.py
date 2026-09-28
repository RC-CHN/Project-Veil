#!/usr/bin/env python3
"""Check a real release archive in temporary roots; never install on the host."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

ARCHIVE = Path(sys.argv.pop(1)).resolve()


class PackageTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="veil-package-check-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        with tarfile.open(ARCHIVE) as archive:
            archive.extractall(self.root, filter="data")
        self.package = self.root / ARCHIVE.name.removesuffix(".tar.gz")
        self.stage = self.root / "stage with spaces"
        self.stage.mkdir()
        self.commands = self.root / "commands"
        self.commands.mkdir()
        self.log = self.root / "commands.jsonl"
        fake = '#!/usr/bin/env python3\nimport json, os, sys\nwith open(os.environ["COMMAND_LOG"], "a") as f: f.write(json.dumps(sys.argv) + "\\n")\nsys.exit(int(os.environ.get("COMMAND_EXIT", "0")))\n'
        for name in ("systemctl", "systemd-sysusers", "journalctl"):
            path = self.commands / name
            path.write_text(fake)
            path.chmod(0o755)
        self.fake = fake
        self.env = os.environ | {
            "PATH": str(self.commands) + os.pathsep + os.environ["PATH"],
            "COMMAND_LOG": str(self.log),
        }

    def install(self, action="install", ok=True, script=None):
        result = subprocess.run(
            [
                "sh",
                str(script or self.package / "install.sh"),
                action,
                "--destdir",
                str(self.stage),
            ],
            env=self.env,
            text=True,
            capture_output=True,
        )
        if ok:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.log.exists(), "staging invoked host management")
        return result

    def test_archive_and_installed_identity(self):
        checksum = ARCHIVE.with_name(ARCHIVE.name + ".sha256").read_text().split()[0]
        self.assertEqual(checksum, hashlib.sha256(ARCHIVE.read_bytes()).hexdigest())
        manifest = json.loads((self.package / "share/veil/manifest.json").read_text())
        for path, digest in manifest["sha256"].items():
            self.assertEqual(
                hashlib.sha256((self.package / path).read_bytes()).hexdigest(), digest
            )
        self.install()
        for name in ("veild", "veilctl"):
            binary = self.stage / "usr/local/bin" / name
            version = subprocess.check_output([binary, "-version"], text=True)
            self.assertIn(manifest["version"], version)
            self.assertEqual(binary.stat().st_mode & 0o777, 0o755)

    def test_upgrade_and_uninstall_preserve_config(self):
        self.install()
        state = self.stage / "var/lib/veil-home"
        state.mkdir(parents=True, mode=0o700)
        cfg = state / "config.json"
        cfg.write_text('{"secret":"preserve-me"}')
        cfg.chmod(0o600)
        self.install()
        uninstall = self.stage / "usr/local/share/veil/uninstall.sh"
        self.install("uninstall", script=uninstall)
        self.assertEqual(cfg.read_text(), '{"secret":"preserve-me"}')
        self.assertEqual(cfg.stat().st_mode & 0o777, 0o600)
        self.assertFalse(
            any(p.is_file() for p in (self.stage / "usr/local").rglob("*"))
        )
        self.install("uninstall")

    def test_refuses_redirected_destination(self):
        outside = self.root / "outside"
        outside.mkdir()
        (self.stage / "usr").symlink_to(outside)
        self.install(ok=False)
        self.assertEqual(list(outside.iterdir()), [])

    def test_refuses_redirected_binary(self):
        self.install()
        outside = self.root / "untouched"
        outside.write_text("untouched")
        binary = self.stage / "usr/local/bin/veild"
        binary.unlink()
        binary.symlink_to(outside)
        self.install(ok=False)
        self.install("uninstall", ok=False)
        self.assertEqual(outside.read_text(), "untouched")

    def test_missing_payload_is_rejected_before_install(self):
        (self.package / "bin/veilctl").unlink()
        self.install(ok=False)
        self.assertEqual(list(self.stage.iterdir()), [])

    def test_platform_commands(self):
        wrapper = self.commands / "veil-system"
        shutil.copyfile(self.package / "bin/veil-system", wrapper)
        wrapper.chmod(0o755)
        ctl = self.commands / "veilctl"
        ctl.write_text(self.fake)
        ctl.chmod(0o755)

        def call(instance, *args, success=True, env=None):
            self.log.unlink(missing_ok=True)
            result = subprocess.run(
                [wrapper, instance, *args], env=env or self.env, capture_output=True
            )
            self.assertEqual(result.returncode == 0, success, result.stderr)
            return (
                [json.loads(line) for line in self.log.read_text().splitlines()]
                if self.log.exists()
                else []
            )

        for invalid in ("../home", "home.service/evil", "x;y", "a" * 49, ""):
            self.assertEqual(call(invalid, "enable", success=False), [])
        self.assertEqual(call("home", "enable", "extra", success=False), [])
        self.assertEqual(call("home", "logs", "--unit=other", success=False), [])
        self.assertEqual(
            call("home", "enable")[0][1:], ["enable", "--now", "veil@home.service"]
        )
        self.assertEqual(
            call("home", "disable")[0][1:], ["disable", "--now", "veil@home.service"]
        )
        self.assertEqual(
            call("home", "logs", "--follow")[0][1:],
            ["--unit", "veil@home.service", "--lines", "100", "--no-pager", "--follow"],
        )
        imported = call("home", "import", "config with spaces.json")
        self.assertEqual(len(imported), 1, "import must not restart an instance")
        self.assertEqual(
            imported[0][1:],
            [
                "-socket",
                "/run/veil-home/control.sock",
                "-config",
                "config with spaces.json",
                "save",
            ],
        )
        for action in ("start", "stop", "restart", "status"):
            self.assertEqual(
                call("home", action)[0][1:],
                ["-socket", "/run/veil-home/control.sock", action],
            )
        self.assertEqual(
            call("home", "daemon-restart")[0][1:], ["restart", "veil@home.service"]
        )
        failed = call(
            "home", "import", "-", success=False, env=self.env | {"COMMAND_EXIT": "1"}
        )
        self.assertEqual(len(failed), 1)


if __name__ == "__main__":
    unittest.main()
