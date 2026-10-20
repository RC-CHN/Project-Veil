"""WebView2 profile teardown must wait for sharing locks without hiding errors."""

import importlib.util
from pathlib import Path
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "gui_check", Path(__file__).parent / "gui/check.py"
)
gui = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gui)


def locked(code=32):
    error = PermissionError("profile in use")
    error.winerror = code
    return error


class ProfileCleanupTest(unittest.TestCase):
    def setUp(self):
        self.temp = mock.Mock(name="temporary directory")
        self.temp.name = "isolated profile"
        patcher = mock.patch.object(
            gui.tempfile, "TemporaryDirectory", return_value=self.temp
        )
        patcher.start()
        self.addCleanup(patcher.stop)

    @mock.patch.object(gui.time, "sleep")
    def test_transient_lock_preserves_journey_error(self, sleep):
        self.temp.cleanup.side_effect = [locked(), locked(33), None]
        with self.assertRaisesRegex(RuntimeError, "journey failed"):
            with gui.temporary_profile() as path:
                self.assertEqual(path, self.temp.name)
                raise RuntimeError("journey failed")
        self.assertEqual(self.temp.cleanup.call_count, 3)
        self.assertEqual(sleep.call_count, 2)

    @mock.patch.object(gui.time, "sleep")
    @mock.patch.object(gui.time, "monotonic", side_effect=[0, 11])
    def test_persistent_lock_still_fails(self, _clock, sleep):
        error = locked()
        self.temp.cleanup.side_effect = error
        with self.assertRaises(PermissionError) as raised:
            with gui.temporary_profile():
                pass
        self.assertIs(raised.exception, error)
        sleep.assert_not_called()

    @mock.patch.object(gui.time, "sleep")
    def test_other_permission_errors_are_not_retried(self, sleep):
        self.temp.cleanup.side_effect = locked(5)
        with self.assertRaises(PermissionError):
            with gui.temporary_profile():
                pass
        self.assertEqual(self.temp.cleanup.call_count, 1)
        sleep.assert_not_called()


if __name__ == "__main__":
    unittest.main()
