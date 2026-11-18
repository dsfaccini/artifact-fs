"""Tests of runner-retry.yml's trigger, permissions and decision, read as
text. test_runner_retry.py tests the decision itself."""

import re
import unittest
from pathlib import Path

from test_release_workflow import jobs, steps

WORKFLOWS = Path(__file__).resolve().parents[1] / "workflows"
WORKFLOW = WORKFLOWS / "runner-retry.yml"


def section(text, key):
    """Return the lines of a top-level key's block."""
    return re.search(rf"^{key}:\n((?:(?:  .*)?\n)*?)(?=\S)", text, re.M).group(1)


def lines(text):
    return {line.strip() for line in text.splitlines() if line.strip() and not line.strip().startswith("#")}


def job_permissions(job):
    m = re.search(r"^    permissions:(.*)\n((?:      .*\n)*)", job + "\n", re.M)
    return m and lines(m.group(1) + "\n" + m.group(2))


class RunnerRetryWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.text = WORKFLOW.read_text()
        (self.job,) = jobs(self.text).values()

    def test_it_runs_once_each_run_of_build_test_and_release_completes(self):
        self.assertEqual(
            lines(section(self.text, "on")),
            {"workflow_run:", 'workflows: ["Build & Test", "Release"]', "types: [completed]"},
        )
        names = {re.search(r"^name: (.*)$", (WORKFLOWS / f).read_text(), re.M).group(1)
                 for f in ["build-test.yml", "release.yml"]}
        self.assertEqual(names, {"Build & Test", "Release"})

    def test_only_its_job_can_write_actions(self):
        self.assertIn("\npermissions: {}\n", self.text)
        self.assertEqual(job_permissions(self.job), {"actions: write", "checks: read", "contents: read"})
        others = [w for w in [*WORKFLOWS.glob("*.yml"), *WORKFLOWS.glob("*.yaml")] if w != WORKFLOW]
        self.assertGreater(len(others), 0)
        for workflow in others:
            text = workflow.read_text()
            self.assertNotRegex(text, r"actions:\s*['\"]?write|write-all", workflow.name)
            # A job without permissions of its own or of its workflow gets the
            # repository's default, which can write actions.
            if not re.search(r"^permissions:", text, re.M):
                for name, job in jobs(text).items():
                    self.assertTrue(job_permissions(job), f"{workflow.name}: {name} declares no permissions")

    def test_the_script_alone_decides(self):
        (decide,) = [step for step in steps(self.job) if "runner_retry.py" in step]
        self.assertEqual(lines(decide), {
            "name: Decide whether no runner took the failed jobs",
            "id: decide",
            "run: python3 -B .github/scripts/runner_retry.py",
        })

    def test_only_a_retry_decision_reruns_the_failed_jobs(self):
        self.assertEqual(len(re.findall(r"/rerun", self.text)), 1)
        (rerun,) = [step for step in steps(self.job) if "/rerun" in step]
        self.assertEqual(lines(rerun), {
            "name: Re-run the failed jobs",
            "if: steps.decide.outputs.retry == 'true'",
            "env:",
            "RUN_ID: ${{ github.event.workflow_run.id }}",
            'run: gh api -X POST "repos/$GITHUB_REPOSITORY/actions/runs/$RUN_ID/rerun-failed-jobs"',
        })


if __name__ == "__main__":
    unittest.main()
