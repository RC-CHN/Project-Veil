"""Guard selective checks against silently passing incomplete CI runs."""

import unittest

from ci import SCOPES, check_gate, scopes


class RoutingTest(unittest.TestCase):
    def test_component_dependencies(self):
        cases = {
            "README.md": set(),
            "veil-core/README.md": set(),
            "PROTOCOL.md": {"interop"},
            "veil-core/internal/wire/wire.go": set(SCOPES),
            "veil-core/patches/reality_proof.go.in": set(SCOPES),
            "veil-service/platform/openwrt/veil": {"service", "cross", "windows", "desktop"},
            "veil-desktop/app.go": {"desktop"},
            "interop/rust/Cargo.lock": {"interop"},
            ".github/workflows/ci.yml": set(SCOPES),
            ".go-version": set(SCOPES),
            "new-component/main.go": set(SCOPES),
        }
        for path, expected in cases.items():
            with self.subTest(path=path):
                self.assertEqual({k for k, v in scopes([path]).items() if v}, expected)
        self.assertTrue(all(scopes(["README.md", "veil-core/go.sum"]).values()))

    def test_required_jobs_cannot_be_skipped(self):
        needs = {name: {"result": "success"} for name in SCOPES}
        needs["changes"] = {
            "result": "success",
            "outputs": dict.fromkeys(SCOPES, "true"),
        }
        check_gate(needs)
        for result in ("failure", "cancelled", "skipped"):
            with self.subTest(result=result):
                needs["core"]["result"] = result
                with self.assertRaises(ValueError):
                    check_gate(needs)

    def test_docs_skip_and_failed_routing(self):
        needs = {name: {"result": "skipped"} for name in SCOPES}
        needs["changes"] = {
            "result": "success",
            "outputs": dict.fromkeys(SCOPES, "false"),
        }
        check_gate(needs)
        needs["changes"]["result"] = "failure"
        with self.assertRaises(ValueError):
            check_gate(needs)


if __name__ == "__main__":
    unittest.main()
