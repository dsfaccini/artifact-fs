"""Tests for release_gate.py, run as release.yml runs it."""

import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

GATE = Path(__file__).resolve().with_name("release_gate.py")


def git(cwd, *args):
    return subprocess.run(
        ["git", "-c", "user.name=test", "-c", "user.email=test@example.invalid", *args],
        cwd=cwd, check=True, capture_output=True, text=True,
    ).stdout.strip()


class GateTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.output = self.dir / "github_output"
        self.output.touch()
        # A repository whose origin/main holds the commit on_main, and the
        # commit off_main a commit past it.
        self.repo = self.dir / "repo"
        self.repo.mkdir()
        git(self.repo, "init", "-q", "-b", "main")
        git(self.repo, "commit", "-q", "--allow-empty", "-m", "on main")
        self.on_main = git(self.repo, "rev-parse", "HEAD")
        git(self.repo, "update-ref", "refs/remotes/origin/main", self.on_main)
        git(self.repo, "commit", "-q", "--allow-empty", "-m", "off main")
        self.off_main = git(self.repo, "rev-parse", "HEAD")

    def gate(self, cwd=None, **env):
        env = {
            "PATH": os.environ["PATH"],
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GITHUB_OUTPUT": str(self.output),
            **env,
        }
        return subprocess.run([sys.executable, "-B", str(GATE)], cwd=cwd or self.repo, env=env, capture_output=True, text=True)

    def tag_push(self, tag, sha):
        return self.gate(GITHUB_EVENT_NAME="push", GITHUB_REF_TYPE="tag", GITHUB_REF_NAME=tag, GITHUB_SHA=sha)

    def test_tag_on_main_publishes(self):
        for tag, version, stage in [
            ("v0.1.0-alpha.1", "0.1.0a1", "alpha"),
            ("v0.1.0-rc.2", "0.1.0rc2", "rc"),
            ("v0.1.0", "0.1.0", "final"),
        ]:
            with self.subTest(tag=tag):
                self.output.write_text("")
                result = self.tag_push(tag, self.on_main)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(self.output.read_text(), f"tag={tag}\nversion={version}\nstage={stage}\npublish=true\n")

    def test_refused_tag_writes_no_outputs(self):
        for tag in ["v0.1.0-beta.1", "v0.1.0-alpha1", "v0.1.0-rc1", "v1", "1.0.0-rc.10", "v0.1.0-rc.1-x"]:
            with self.subTest(tag=tag):
                result = self.tag_push(tag, self.on_main)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("is not a release tag", result.stdout)
                self.assertEqual(self.output.read_text(), "")

    def test_tag_off_main_writes_no_outputs(self):
        result = self.tag_push("v0.1.0-alpha.1", self.off_main)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("which is not on main", result.stdout)
        self.assertEqual(self.output.read_text(), "")

    def test_other_runs_are_dry_runs(self):
        # A dry run checks no ancestry, so it runs outside a repository too.
        outside = self.dir / "outside"
        outside.mkdir()
        for name, env, tag in [
            ("branch push", {"GITHUB_EVENT_NAME": "push", "GITHUB_REF_TYPE": "branch", "GITHUB_REF_NAME": "main"}, "v0.1.0-alpha.1"),
            ("manual run", {"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF_TYPE": "branch", "DRY_RUN_TAG": "v0.2.0-rc.1"}, "v0.2.0-rc.1"),
            (
                "manual run on a tag",
                {"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF_TYPE": "tag", "GITHUB_REF_NAME": "v9.9.9", "DRY_RUN_TAG": "v0.2.0"},
                "v0.2.0",
            ),
        ]:
            with self.subTest(name):
                self.output.write_text("")
                result = self.gate(cwd=outside, **env)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(f"tag={tag}\n", self.output.read_text())
                self.assertTrue(self.output.read_text().endswith("publish=false\n"), self.output.read_text())

    def test_refused_dry_run_tag_writes_no_outputs(self):
        result = self.gate(GITHUB_EVENT_NAME="workflow_dispatch", GITHUB_REF_TYPE="branch", DRY_RUN_TAG="v0.1.0-beta.1")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertEqual(self.output.read_text(), "")


if __name__ == "__main__":
    unittest.main()
