"""Tests of release.yml's job structure, read as text: the standard library
has no YAML parser."""

import re
import unittest
from pathlib import Path

WORKFLOW = Path(__file__).resolve().parents[1] / "workflows" / "release.yml"

# What a release waits for before any file of it becomes public.
CHECKS = {"build-test", "semgrep", "examples", "redaction-fuzz", "build", "smoke", "twine-check"}


def jobs(text):
    """Return each job's id and its lines, from the jobs section."""
    found, current = {}, None
    for line in text.split("\njobs:\n", 1)[1].splitlines():
        if m := re.fullmatch(r"  ([a-z0-9-]+):", line):
            current = found[m.group(1)] = []
        elif current is not None:
            current.append(line)
    return {job: "\n".join(lines) for job, lines in found.items()}


def needs(job):
    m = re.search(r"^    needs: \[(.*)\]$", job, re.M)
    return set(m.group(1).split(", ")) if m else set()


def steps(job):
    return re.split(r"^      - ", job, flags=re.M)[1:]


class ReleaseWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.jobs = jobs(WORKFLOW.read_text())

    def test_build_installs_nothing_from_a_package_index(self):
        for word in ["uv", "pip", "twine"]:
            self.assertNotRegex(self.jobs["build"], rf"\b{word}\b")

    def test_twine_checks_the_uploaded_wheels(self):
        job = self.jobs["twine-check"]
        self.assertIn("twine", job)
        self.assertNotIn("actions/checkout", job)
        self.assertEqual(needs(job) | set(re.findall(r"^    needs: (\S+)$", job, re.M)), {"build"})

    def test_artifacts_download_by_name(self):
        downloads = 0
        for name, job in self.jobs.items():
            for step in steps(job):
                if "actions/download-artifact@" in step:
                    downloads += 1
                    self.assertRegex(step, r"\n          name: (wheels|archives)\n", name)
                    self.assertNotIn("merge-multiple", step, name)
        self.assertGreater(downloads, 0)

    def test_attest_and_release_wait_for_every_check(self):
        self.assertLessEqual(CHECKS, needs(self.jobs["attest"]))
        self.assertLessEqual(CHECKS | {"attest"}, needs(self.jobs["github-release"]))

    def test_publish_jobs_name_each_stage_they_publish(self):
        for job, stages in [("publish-testpypi", ["alpha", "rc"]), ("publish-pypi", ["final", "alpha"])]:
            condition = re.search(r"^    if: >-\n((?:      .*\n?)+)", self.jobs[job], re.M).group(1)
            for stage in stages:
                self.assertIn(f"needs.gate.outputs.stage == '{stage}'", condition, job)
            self.assertNotIn("stage !=", condition, job)


if __name__ == "__main__":
    unittest.main()
