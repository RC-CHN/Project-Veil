"""Exercise installation boundaries without touching the current user's setup."""

from contextlib import redirect_stdout
import io
import os
from pathlib import Path
import tempfile
import unittest

from install import FILES, APP_ID, desktop_entry, manage


@unittest.skipUnless(os.name == "posix", "Linux user installer")
class InstallTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.source = self.base / "source"
        self.source.mkdir()
        for name in FILES:
            (self.source / name).write_text(name)
        self.prefix = self.base / 'space $cash "quote" `tick` \\slash'
        self.target = self.prefix / "lib/veil-desktop"
        self.launcher = self.prefix / f"share/applications/{APP_ID}.desktop"

    def run_action(self, action):
        with redirect_stdout(io.StringIO()):
            manage(action, self.source, self.prefix)

    def test_upgrade_and_uninstall_preserve_profile(self):
        profile = self.base / "config/Veil/desktop/profile.json"
        profile.parent.mkdir(parents=True)
        profile.write_text("saved profile")
        self.run_action("install")
        self.assertEqual(self.launcher.read_text(), desktop_entry(self.target))
        self.assertEqual((self.target / "veil-desktop").stat().st_mode & 0o777, 0o755)
        (self.source / "veil-desktop").write_text("updated binary")
        self.run_action("install")
        self.assertEqual((self.target / "veil-desktop").read_text(), "updated binary")
        self.run_action("uninstall")
        self.run_action("uninstall")
        self.assertFalse(self.target.exists())
        self.assertFalse(self.launcher.exists())
        self.assertEqual(profile.read_text(), "saved profile")

    def test_refuse_symlink_parent(self):
        self.prefix.mkdir()
        outside = self.base / "outside"
        outside.mkdir()
        (self.prefix / "lib").symlink_to(outside, target_is_directory=True)
        with self.assertRaises(ValueError):
            self.run_action("install")
        self.assertEqual(list(outside.iterdir()), [])

    def test_reject_percent_in_launcher_path(self):
        self.prefix = self.base / "%field"
        with self.assertRaisesRegex(ValueError, "without %"):
            self.run_action("install")
        self.assertFalse(self.prefix.exists())

    def test_refuse_unmanaged_files(self):
        self.run_action("install")
        extra = self.target / "other-app"
        extra.write_text("keep")
        for action in ("install", "uninstall"):
            with self.assertRaises(ValueError):
                self.run_action(action)
        self.assertEqual(extra.read_text(), "keep")

    def test_missing_source_leaves_installation_intact(self):
        self.run_action("install")
        (self.source / "veil-desktop").write_text("replacement")
        (self.source / "LICENSE").unlink()
        with self.assertRaises(OSError):
            self.run_action("install")
        self.assertEqual((self.target / "veil-desktop").read_text(), "veil-desktop")


if __name__ == "__main__":
    unittest.main()
