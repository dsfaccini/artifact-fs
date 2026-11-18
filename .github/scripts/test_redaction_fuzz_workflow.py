"""Tests of how redaction-fuzz.yml seals a failing input, read as text. The
seal jobs of build-test.yml test the action itself, and these tests pin how
those jobs read its upload."""

import unittest
from pathlib import Path

from test_release_workflow import jobs, steps

WORKFLOWS = Path(__file__).resolve().parents[1] / "workflows"
ACTIONS = Path(__file__).resolve().parents[1] / "actions"


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

    # A lookup by name, or a list of the run's artifacts, right after an upload
    # can miss it, and download-artifact lists the run's artifacts even for
    # artifact-ids. So the seal jobs read the upload only by the ID that
    # upload-artifact returns.
    def test_the_seal_jobs_read_the_upload_only_by_its_id(self):
        build_test = jobs((WORKFLOWS / "build-test.yml").read_text())
        seal, log = build_test["seal-fuzz-inputs"], build_test["seal-fuzz-inputs-log"]
        for name, job in [("seal-fuzz-inputs", seal), ("seal-fuzz-inputs-log", log)]:
            self.assertNotIn("actions/download-artifact@", job, name)
            self.assertNotRegex(job, r"/artifacts[\"'?\s]", name)
        action = (ACTIONS / "seal-fuzz-inputs" / "action.yml").read_text()
        self.assertIn("value: ${{ steps.upload.outputs.artifact-id }}", lines(action))
        self.assertIn("artifact-id: ${{ steps.seal.outputs.artifact-id }}", lines(seal))
        self.assertIn("ARTIFACT_ID: ${{ needs.seal-fuzz-inputs.outputs.artifact-id }}", lines(log))
        self.assertIn('gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$ARTIFACT_ID/zip" >upload.zip', lines(log))


if __name__ == "__main__":
    unittest.main()
