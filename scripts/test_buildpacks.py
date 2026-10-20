import contextlib
import hashlib
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import buildpacks


def archive(member="pack", symlink=False):
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode="w:gz") as target:
        info = tarfile.TarInfo(member)
        if symlink:
            info.type = tarfile.SYMTYPE
            info.linkname = "/bin/sh"
            target.addfile(info)
        else:
            info.size = 6
            target.addfile(info, io.BytesIO(b"binary"))
    return data.getvalue()


class BuildpacksInstallerTests(unittest.TestCase):
    def invoke(self, root, data, digest=None):
        actual = hashlib.sha256(data).hexdigest()
        with patch.object(buildpacks, "ROOT", Path(root)), \
                patch.object(buildpacks.platform, "system", return_value="Linux"), \
                patch.object(buildpacks.platform, "machine", return_value="x86_64"), \
                patch.object(buildpacks, "ASSETS", {"x86_64": ("linux", digest or actual)}), \
                patch.object(buildpacks.urllib.request, "urlopen", return_value=io.BytesIO(data)), \
                contextlib.redirect_stdout(io.StringIO()):
            buildpacks.install()

    def test_verified_binary_is_private_and_local(self):
        with tempfile.TemporaryDirectory() as directory:
            self.invoke(directory, archive())
            binary = Path(directory) / ".local/dev/tools/pack"
            self.assertEqual(binary.read_bytes(), b"binary")
            self.assertEqual(binary.stat().st_mode & 0o777, 0o700)

    def test_bad_digest_does_not_install(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RuntimeError):
                self.invoke(directory, archive(), "0" * 64)
            self.assertFalse((Path(directory) / ".local").exists())

    def test_never_extract_links_or_arbitrary_paths(self):
        for data in (archive(symlink=True), archive("../pack")):
            with self.subTest(), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(RuntimeError):
                    self.invoke(directory, data)
                self.assertFalse((Path(directory) / ".local").exists())

    def test_refuses_symlink_directory(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as elsewhere:
            (Path(directory) / ".local").symlink_to(elsewhere)
            with self.assertRaises(RuntimeError):
                self.invoke(directory, archive())
            self.assertEqual(list(Path(elsewhere).iterdir()), [])


if __name__ == "__main__":
    unittest.main()
