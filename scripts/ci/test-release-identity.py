"""Exercise the actual publication guard using only disposable local Git refs."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

GUARD = Path(__file__).with_name("check-release-identity.sh").resolve()
MAKEFILE = GUARD.parents[2] / "Makefile"


class ReleaseIdentity(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git("init", "-q")
        self.git("config", "user.name", "Release fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        (self.root / "release.json").write_text(json.dumps({"version": "0.3.6"}))
        self.git("add", "release.json")
        self.git("commit", "-qm", "Synthetic release source")
        self.sha = self.git("rev-parse", "HEAD").strip()
        self.git("tag", "0.3.6")

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, text=True)

    def run_guard(self, **overrides):
        env = os.environ | {
            "GITHUB_REF_TYPE": "tag",
            "GITHUB_REF_NAME": "0.3.6",
            "GITHUB_SHA": self.sha,
            "REQUESTED_VERSION": "0.3.6",
        } | overrides
        return subprocess.run(
            ["bash", str(GUARD)], cwd=self.root, env=env, capture_output=True
        ).returncode

    def test_exact_tag_source(self):
        self.assertEqual(self.run_guard(), 0)

    def test_release_event_without_manual_version(self):
        self.assertEqual(self.run_guard(REQUESTED_VERSION=""), 0)

    def test_prefixed_release_tag(self):
        self.git("tag", "v0.3.6")
        self.assertEqual(self.run_guard(GITHUB_REF_NAME="v0.3.6"), 0)

    def test_branch_cannot_publish(self):
        self.assertNotEqual(self.run_guard(GITHUB_REF_TYPE="branch"), 0)

    def test_wrong_requested_version_cannot_publish(self):
        self.assertNotEqual(self.run_guard(REQUESTED_VERSION="0.3.7"), 0)

    def test_wrong_source_cannot_publish(self):
        self.assertNotEqual(self.run_guard(GITHUB_SHA="0" * 40), 0)

    def test_mismatched_tag_cannot_publish(self):
        self.git("tag", "0.3.7")
        self.assertNotEqual(self.run_guard(GITHUB_REF_NAME="0.3.7"), 0)

    def test_direct_make_publish_never_calls_container_tool(self):
        marker = self.root / "called"
        tool = self.root / "container-tool"
        tool.write_text(f"#!/bin/sh\ntouch '{marker}'\n")
        tool.chmod(0o755)
        result = subprocess.run(
            ["make", "-f", str(MAKEFILE), "docker-push", f"CONTAINER_TOOL={tool}"],
            cwd=self.root,
            capture_output=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(marker.exists())


if __name__ == "__main__":
    unittest.main()
