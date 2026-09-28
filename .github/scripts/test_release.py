"""Release gates must reject incomplete, damaged or misidentified packages."""

import hashlib
import io
import json
from pathlib import Path
import tempfile
import tarfile
import unittest
import zipfile

from release import payload, verify, verify_manifest


class ReleaseTest(unittest.TestCase):
    def test_complete_release_and_unexpected_file(self):
        with tempfile.TemporaryDirectory() as temp:
            folder = Path(temp)
            manifest = json.dumps(
                {"version": "0.3.1", "commit": "abc", "dirty": False}
            ).encode()
            for name in payload("0.3.1"):
                path = folder / name
                if name.endswith(".zip"):
                    with zipfile.ZipFile(path, "w") as archive:
                        archive.writestr("package/manifest.json", manifest)
                elif name.endswith(".tar.gz"):
                    with tarfile.open(path, "w:gz") as archive:
                        info = tarfile.TarInfo("package/share/veil/manifest.json")
                        info.size = len(manifest)
                        archive.addfile(info, io.BytesIO(manifest))
                else:
                    path.write_bytes(b"package fixture")
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
                (folder / (name + ".sha256")).write_text(f"{digest}  {name}\n")
            self.assertEqual(len(verify(folder, "0.3.1", "abc")), 21)
            self.assertEqual(len((folder / "SHA256SUMS").read_text().splitlines()), 10)
            (folder / "SHA256SUMS").unlink()
            (folder / "unexpected.exe").touch()
            with self.assertRaisesRegex(ValueError, "unexpected"):
                verify(folder, "0.3.1", "abc")

    def test_manifest_identity(self):
        good = {"version": "0.3.1", "commit": "abc", "dirty": False}
        verify_manifest(json.dumps(good), "0.3.1", "abc")
        for field, value in (("version", "ci"), ("commit", "other"), ("dirty", True)):
            with self.subTest(field=field), self.assertRaises(ValueError):
                verify_manifest(json.dumps(good | {field: value}), "0.3.1", "abc")

    def test_missing_and_corrupted_payload(self):
        with tempfile.TemporaryDirectory() as temp:
            folder = Path(temp)
            with self.assertRaisesRegex(ValueError, "missing"):
                verify(folder, "0.3.1", "abc")
            for name in payload("0.3.1"):
                (folder / name).write_bytes(b"broken")
                (folder / (name + ".sha256")).write_text("wrong checksum\n")
            with self.assertRaisesRegex(ValueError, "checksum"):
                verify(folder, "0.3.1", "abc")


if __name__ == "__main__":
    unittest.main()
