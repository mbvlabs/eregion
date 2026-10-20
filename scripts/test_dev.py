"""Security and lifecycle tests for the disposable development launcher."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import dev


class DevelopmentLauncherTests(unittest.TestCase):
    def test_private_atomic_write(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "credentials"
            dev.private_write(path, "secret")
            self.assertEqual(path.read_text(), "secret")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            dev.private_write(path, "replacement")
            self.assertEqual(path.read_text(), "replacement")

    def test_refuse_symlink_destination(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "target"
            target.write_text("keep")
            link = Path(directory) / "credentials"
            link.symlink_to(target)
            with self.assertRaises(RuntimeError):
                dev.private_write(link, "overwrite")
            self.assertEqual(target.read_text(), "keep")

    def test_refuse_symlink_temporary(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "target"
            target.write_text("keep")
            (Path(directory) / "credentials.tmp").symlink_to(target)
            with self.assertRaises(OSError):
                dev.private_write(Path(directory) / "credentials", "overwrite")
            self.assertEqual(target.read_text(), "keep")

    def test_process_identity_prevents_pid_reuse(self):
        state = {"pid": 42, "start": "old"}
        with patch.object(dev, "start_identity", return_value="new"):
            self.assertFalse(dev.alive(state))
        with patch.object(dev, "start_identity", return_value="old"):
            self.assertTrue(dev.alive(state))
        self.assertFalse(dev.alive(None))

    def test_current_process_identity(self):
        if Path("/proc/self/stat").exists():
            self.assertIsNotNone(dev.start_identity(os.getpid()))

    def test_refuse_unowned_container(self):
        result = type("Result", (), {"returncode": 0, "stdout": json.dumps([
            {"Config": {"Labels": {"io.eregion.dev-root": "another-checkout"}}}
        ])})()
        with patch.object(dev.subprocess, "run", return_value=result):
            with self.assertRaises(RuntimeError):
                dev.container()

    def test_stale_pid_is_never_signalled(self):
        with patch.object(dev, "load", return_value={"pid": 42, "start": "old"}), \
                patch.object(dev, "alive", return_value=False), \
                patch.object(dev.os, "kill") as kill:
            dev.stop_process()
            kill.assert_not_called()

    def test_restart_retains_database(self):
        with patch.object(dev, "stop_process") as stop, patch.object(dev, "up") as up, \
                patch.object(dev, "container") as container:
            dev.restart()
            stop.assert_called_once()
            up.assert_called_once()
            container.assert_not_called()


if __name__ == "__main__":
    unittest.main()
