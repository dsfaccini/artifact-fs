"""Tests of how redaction-fuzz.yml seals a failing input, read as text. The
seal jobs of build-test.yml test the action itself."""

import unittest
from pathlib import Path

from test_release_workflow import jobs, steps

WORKFLOWS = Path(__file__).resolve().parents[1] / "workflows"


def lines(step):
    return {line.strip() for line in step.splitlines()}


def setup_go_steps(job):
    return [step for step in steps(job) if "uses: actions/setup-go@" in step]


class RedactionFuzzWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.fuzz = jobs((WORKFLOWS / "redaction-fuzz.yml").read_text())["fuzz"]

    def test_a_failed_fuzz_step_seals_the_corpus_to_the_repository_recipient(self):
        (fuzz,) = [step for step in steps(self.fuzz) if ".github/scripts/fuzz.sh" in step]
        self.assertIn("id: fuzz", lines(fuzz))
        (seal,) = [step for step in steps(self.fuzz) if "uses: ./.github/actions/seal-fuzz-inputs" in step]
        self.assertLessEqual(
            {
                "if: failure() && steps.fuzz.outcome == 'failure'",
                "corpus: internal/auth/testdata/fuzz",
                "recipient: ${{ vars.FUZZ_AGE_RECIPIENT }}",
            },
            lines(seal),
        )

    # A saved cache would hold GOCACHE's fuzz/ inputs, and a pull request's run
    # can restore it.
    def test_no_job_that_fuzzes_saves_a_go_cache(self):
        build_test = jobs((WORKFLOWS / "build-test.yml").read_text())
        seal_jobs = {name: job for name, job in build_test.items() if name.startswith("seal-fuzz-inputs")}
        self.assertEqual(set(seal_jobs), {"seal-fuzz-inputs", "seal-fuzz-inputs-log"})
        checked = {"fuzz": self.fuzz} | seal_jobs
        for name in ["fuzz", "seal-fuzz-inputs"]:
            self.assertTrue(setup_go_steps(checked[name]), name)
        for name, job in checked.items():
            for step in setup_go_steps(job):
                self.assertIn("cache: false", lines(step), name)


if __name__ == "__main__":
    unittest.main()
