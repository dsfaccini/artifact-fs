"""Tests for runner_retry.py, run as runner-retry.yml runs it."""

import json
import subprocess
import sys
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().with_name("runner_retry.py")
REPOSITORY = "dsfaccini/artifact-fs"
NOT_ACQUIRED = "The job was not acquired by Runner of type hosted even after multiple attempts"
CAPACITY = "Due to capacity constraints, jobs targeting macOS arm64 runners may experience longer queue times."


def job(id, name, conclusion="success"):
    return {"id": id, "name": name, "status": "completed", "conclusion": conclusion}


def annotations(id, *messages):
    return [{"job": id, "message": message} for message in messages]


# Attempt 1 of Build & Test run 44367029746, which no macOS runner took.
DARWIN_TEST = 135023426375
BUILD_TEST = [
    job(135023425820, "seal-fuzz-inputs"),
    job(135023426277, "build-test (Go go.mod)"),
    job(135023426357, "fuse-e2e (root)"),
    job(DARWIN_TEST, "darwin-test", "cancelled"),
    job(135023426702, "fuse-e2e (non-root)"),
    job(135023426865, "build-test (Go stable)"),
    job(135023719767, "seal-fuzz-inputs-log"),
]
GO_STABLE = 135023426865

# Attempt 1 of Release run 44346067198, a dry run whose macOS smoke test no
# runner took. The jobs after the smoke tests skip on a dry run.
SMOKE_MACOS = 134949800673
RELEASE = [
    job(134948661582, "gate"),
    job(134948758306, "build"),
    job(134948759413, "build-test / darwin-test"),
    job(134949800668, "smoke (ubuntu-24.04)"),
    job(SMOKE_MACOS, "smoke (macos-26)", "cancelled"),
    job(134949800696, "twine-check"),
    job(134960134893, "attest", "skipped"),
    job(134960135895, "github-release", "skipped"),
    job(134960136209, "publish-testpypi", "skipped"),
    job(134960137699, "publish-pypi", "skipped"),
]


def with_conclusion(jobs, id, conclusion):
    return [{**j, "conclusion": conclusion} if j["id"] == id else j for j in jobs]


class RunnerRetryTest(unittest.TestCase):
    def decide(self, jobs, annotations, attempt=1, head_repository=REPOSITORY):
        """Return the script's output, and its last line, the decision."""
        result = subprocess.run(
            [sys.executable, "-B", str(SCRIPT), "--run", "https://github.com/dsfaccini/artifact-fs/actions/runs/1",
             "--repository", REPOSITORY, "--head-repository", head_repository, "--attempt", str(attempt)],
            input=json.dumps({"jobs": jobs, "annotations": annotations}),
            check=True, capture_output=True, text=True,
        )
        return result.stdout, result.stdout.splitlines()[-1]

    def test_a_job_no_runner_took_retries_on_attempt_1(self):
        out, decision = self.decide(BUILD_TEST, annotations(DARWIN_TEST, NOT_ACQUIRED, CAPACITY))
        self.assertEqual(decision, "retry: no runner took darwin-test")
        self.assertEqual(out.splitlines()[0],
                         f"Run https://github.com/dsfaccini/artifact-fs/actions/runs/1, attempt 1, head in {REPOSITORY}:")
        self.assertIn(f"  darwin-test (job {DARWIN_TEST}): cancelled, not acquired by a runner\n", out)
        self.assertIn(f"  build-test (Go stable) (job {GO_STABLE}): success\n", out)

    def test_a_release_retries_past_the_jobs_it_skipped(self):
        _, decision = self.decide(RELEASE, annotations(SMOKE_MACOS, NOT_ACQUIRED, CAPACITY, CAPACITY, CAPACITY))
        self.assertEqual(decision, "retry: no runner took smoke (macos-26)")

    def test_every_job_no_runner_took_retries(self):
        jobs = with_conclusion(RELEASE, 134948759413, "cancelled")
        _, decision = self.decide(jobs, annotations(SMOKE_MACOS, NOT_ACQUIRED) + annotations(134948759413, NOT_ACQUIRED))
        self.assertEqual(decision, "retry: no runner took build-test / darwin-test, smoke (macos-26)")

    def test_attempt_2_does_not_retry(self):
        _, decision = self.decide(BUILD_TEST, annotations(DARWIN_TEST, NOT_ACQUIRED), attempt=2)
        self.assertEqual(decision, "no-retry: attempt 2 is a re-run")

    def test_a_test_failure_does_not_retry(self):
        jobs = with_conclusion(with_conclusion(BUILD_TEST, DARWIN_TEST, "success"), GO_STABLE, "failure")
        _, decision = self.decide(jobs, [])
        self.assertEqual(decision, "no-retry: build-test (Go stable) (failure) did not end for want of a runner")

    def test_a_test_failure_beside_a_job_no_runner_took_does_not_retry(self):
        jobs = with_conclusion(BUILD_TEST, GO_STABLE, "failure")
        _, decision = self.decide(jobs, annotations(DARWIN_TEST, NOT_ACQUIRED))
        self.assertEqual(decision, "no-retry: build-test (Go stable) (failure) did not end for want of a runner")

    def test_the_annotation_counts_only_for_its_own_job(self):
        jobs = with_conclusion(with_conclusion(BUILD_TEST, DARWIN_TEST, "success"), GO_STABLE, "timed_out")
        _, decision = self.decide(jobs, annotations(DARWIN_TEST, NOT_ACQUIRED))
        self.assertEqual(decision, "no-retry: build-test (Go stable) (timed_out) did not end for want of a runner")

    def test_a_head_in_a_fork_does_not_retry(self):
        _, decision = self.decide(BUILD_TEST, annotations(DARWIN_TEST, NOT_ACQUIRED), head_repository="someone/artifact-fs")
        self.assertEqual(decision, f"no-retry: the run's head is in someone/artifact-fs, not {REPOSITORY}")

    def test_a_cancellation_without_the_annotation_does_not_retry(self):
        _, decision = self.decide(BUILD_TEST, annotations(DARWIN_TEST, CAPACITY))
        self.assertEqual(decision, "no-retry: darwin-test (cancelled) did not end for want of a runner")

    def test_only_the_exact_annotation_retries(self):
        for message in [NOT_ACQUIRED + ".", NOT_ACQUIRED.replace("hosted", "self-hosted"), NOT_ACQUIRED.lower()]:
            with self.subTest(message=message):
                _, decision = self.decide(BUILD_TEST, annotations(DARWIN_TEST, message))
                self.assertEqual(decision, "no-retry: darwin-test (cancelled) did not end for want of a runner")

    def test_a_run_without_a_failed_job_does_not_retry(self):
        _, decision = self.decide(with_conclusion(BUILD_TEST, DARWIN_TEST, "success"), [])
        self.assertEqual(decision, "no-retry: no job failed")


if __name__ == "__main__":
    unittest.main()
