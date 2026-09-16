"""Failure-path tests for the acceptance supervisor's owned fixture resources."""
import threading
import unittest
from unittest import mock

import accept_linux


class FixtureFailureTests(unittest.TestCase):
    def test_partial_initialization_joins_started_servers(self):
        before = {t.ident for t in threading.enumerate()}
        # TCP has already started when the first UDP listener fails. No live
        # server thread may escape the constructor's failure path.
        with mock.patch.object(accept_linux, "UDPFixture", side_effect=OSError("injected bind failure")):
            with self.assertRaises(OSError):
                accept_linux.Fixtures()
        self.assertEqual(before, {t.ident for t in threading.enumerate()})


if __name__ == "__main__":
    unittest.main()
