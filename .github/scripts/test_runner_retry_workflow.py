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
        permissions = re.search(r"^    permissions:\n((?:      .*\n)+)", self.job, re.M).group(1)
        self.assertEqual(lines(permissions), {"actions: write", "checks: read", "contents: read"})
        for workflow in WORKFLOWS.glob("*.yml"):
            if workflow != WORKFLOW:
                self.assertNotRegex(workflow.read_text(), r"actions: write|write-all", workflow.name)

    def test_the_script_decides_from_the_run_that_completed(self):
        env = lines(re.search(r"^    env:\n((?:      .*\n)+)", self.job, re.M).group(1))
        self.assertLessEqual(
            {
                "RUN_ID: ${{ github.event.workflow_run.id }}",
                "ATTEMPT: ${{ github.event.workflow_run.run_attempt }}",
                "HEAD_REPOSITORY: ${{ github.event.workflow_run.head_repository.full_name }}",
            },
            env,
        )
        (step,) = [step for step in steps(self.job) if "runner_retry.py" in step]
        self.assertIn('"repos/$GITHUB_REPOSITORY/actions/runs/$RUN_ID/attempts/$ATTEMPT/jobs?per_page=100"', step)
        call = re.search(r"python3 -B \.github/scripts/runner_retry\.py ([^|]*)\|", step).group(1)
        self.assertEqual(
            re.findall(r"--[a-z-]+ \S+", call),
            ['--run "$RUN_URL"', '--repository "$GITHUB_REPOSITORY"',
             '--head-repository "$HEAD_REPOSITORY"', '--attempt "$ATTEMPT"'],
        )
        # A step's run script reads the event's fields from env, never from an
        # expression the runner pastes into the script.
        self.assertNotIn("${{", step)

    def test_only_a_retry_decision_reruns_the_failed_jobs(self):
        self.assertEqual(len(re.findall(r"/rerun", self.text)), 1)
        self.assertRegex(
            self.text,
            r"\n( +)if tail -n 1 \"\$RUNNER_TEMP/decision\" \| grep -q '\^retry:'; then\n"
            r"\1  gh api -X POST \"repos/\$GITHUB_REPOSITORY/actions/runs/\$RUN_ID/rerun-failed-jobs\"\n"
            r"\1fi\n",
        )
        self.assertIn('| tee "$RUNNER_TEMP/decision"\n', self.text)


if __name__ == "__main__":
    unittest.main()
