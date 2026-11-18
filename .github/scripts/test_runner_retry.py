"""Tests for runner_retry.py, run as runner-retry.yml runs it, with a fake gh
that answers each API GET with the pages of a real response's shape."""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from urllib.parse import quote

SCRIPT = Path(__file__).resolve().with_name("runner_retry.py")
REPOSITORY = "dsfaccini/artifact-fs"
NOT_ACQUIRED = "The job was not acquired by Runner of type hosted even after multiple attempts"
CAPACITY = "Due to capacity constraints, jobs targeting macOS arm64 runners may experience longer queue times."

# Answers `gh api --paginate --slurp <endpoint>` from $FAKE_GH_API, and fails
# as gh fails on a 404 for an endpoint it does not hold.
FAKE_GH = """
import json, os, sys
args = sys.argv[1:]
if len(args) != 4 or args[:3] != ["api", "--paginate", "--slurp"]:
    sys.exit(f"fake gh: unexpected arguments {args}")
with open(os.environ["FAKE_GH_API"]) as f:
    pages = json.load(f).get(args[3])
if pages is None:
    sys.exit("gh: Not Found (HTTP 404)")
print(json.dumps(pages))
"""

STEPS = [{"name": "Set up job", "status": "completed", "conclusion": "success", "number": 1}]


def job(id, name, conclusion="success", steps=STEPS):
    return {"id": id, "name": name, "status": "completed", "conclusion": conclusion, "steps": steps}


def unacquired(id, name):
    return job(id, name, "cancelled", [])


# Attempt 1 of Build & Test run 44367029746, which no macOS runner took.
RUN_ID = 44367029746
SHA = "824457e48150f550894ffcedff4367a3ab135455"
BRANCH = "slimtree-linux-numbers"
DARWIN_TEST = 135023426375
GO_STABLE = 135023426865
BUILD_TEST = [
    job(135023425820, "seal-fuzz-inputs"),
    job(135023426277, "build-test (Go go.mod)"),
    job(135023426357, "fuse-e2e (root)"),
    unacquired(DARWIN_TEST, "darwin-test"),
    job(135023426702, "fuse-e2e (non-root)"),
    job(GO_STABLE, "build-test (Go stable)"),
    job(135023719767, "seal-fuzz-inputs-log"),
]

# Attempt 1 of Release run 44346067198, a dry run whose macOS smoke test no
# runner took. The jobs after the smoke tests skip on a dry run.
SMOKE_MACOS = 134949800673
DARWIN_IN_RELEASE = 134948759413
RELEASE = [
    job(134948661582, "gate"),
    job(134948758306, "build"),
    job(DARWIN_IN_RELEASE, "build-test / darwin-test"),
    job(134949800668, "smoke (ubuntu-24.04)"),
    unacquired(SMOKE_MACOS, "smoke (macos-26)"),
    job(134949800696, "twine-check"),
    job(134960134893, "attest", "skipped", []),
    job(134960135895, "github-release", "skipped", []),
    job(134960136209, "publish-testpypi", "skipped", []),
    job(134960137699, "publish-pypi", "skipped", []),
]


def with_job(jobs, id, **fields):
    return [{**j, **fields} if j["id"] == id else j for j in jobs]


def annotation(message, level="failure"):
    return {"path": ".github", "start_line": 1, "end_line": 1, "annotation_level": level, "message": message}


def ref(name, sha, kind="commit"):
    return {"ref": name, "object": {"sha": sha, "type": kind}}


class RunnerRetryTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        gh = self.dir / "bin" / "gh"
        gh.parent.mkdir()
        gh.write_text(f"#!{sys.executable}\n{FAKE_GH}")
        gh.chmod(0o755)

    def decide(self, jobs, annotations, refs=None, attempt=1, head_repository=REPOSITORY, branch=BRANCH):
        """Run the script on a run of the jobs at SHA, and return its output
        and the decision it appended to $GITHUB_OUTPUT. annotations maps a job's
        ID to its annotations, and refs maps a branch or tag to its refs."""
        if refs is None:
            refs = {f"heads/{branch}": [ref(f"refs/heads/{branch}", SHA)]}
        api = {f"repos/{REPOSITORY}/actions/runs/{RUN_ID}/attempts/{attempt}/jobs?per_page=100":
               [{"total_count": len(jobs), "jobs": jobs}]}
        for id, found in annotations.items():
            api[f"repos/{REPOSITORY}/check-runs/{id}/annotations?per_page=100"] = [found]
        for kind in ["heads", "tags"]:
            api[f"repos/{REPOSITORY}/git/matching-refs/{kind}/{quote(branch)}"] = [refs.get(f"{kind}/{branch}", [])]
        for sha, target in refs.get("tag objects", {}).items():
            api[f"repos/{REPOSITORY}/git/tags/{sha}"] = [{"sha": sha, "object": target}]
        (self.dir / "api.json").write_text(json.dumps(api))
        event = {"workflow_run": {
            "id": RUN_ID, "run_attempt": attempt, "html_url": f"https://github.com/{REPOSITORY}/actions/runs/{RUN_ID}",
            "head_branch": branch, "head_sha": SHA,
            "head_repository": {"full_name": head_repository},
        }}
        (self.dir / "event.json").write_text(json.dumps(event))
        output = self.dir / "github_output"
        output.write_text("")
        result = subprocess.run(
            [sys.executable, "-B", str(SCRIPT)],
            env={
                "PATH": f"{self.dir / 'bin'}{os.pathsep}{os.environ['PATH']}",
                "FAKE_GH_API": str(self.dir / "api.json"),
                "GITHUB_EVENT_PATH": str(self.dir / "event.json"),
                "GITHUB_REPOSITORY": REPOSITORY,
                "GITHUB_OUTPUT": str(output),
            },
            check=True, capture_output=True, text=True,
        )
        return result.stdout, output.read_text()

    def assertDecision(self, out, output, decision):
        self.assertEqual(out.splitlines()[-1], decision)
        self.assertEqual(output, "retry=true\n" if decision.startswith("retry:") else "retry=false\n")

    def test_a_job_no_runner_took_retries_on_attempt_1(self):
        out, output = self.decide(BUILD_TEST, {DARWIN_TEST: [annotation(NOT_ACQUIRED), annotation(CAPACITY, "notice")]})
        self.assertDecision(out, output, 'retry: no runner took "darwin-test"')
        self.assertEqual(out.splitlines()[0],
                         f'Run https://github.com/{REPOSITORY}/actions/runs/{RUN_ID}, attempt 1, '
                         f'"{BRANCH}" at {SHA} in "{REPOSITORY}":')
        self.assertIn(f'  "darwin-test" (job {DARWIN_TEST}): cancelled, not acquired by a runner\n', out)
        self.assertIn(f'  "build-test (Go stable)" (job {GO_STABLE}): success\n', out)

    def test_a_release_retries_past_the_jobs_it_skipped(self):
        out, output = self.decide(RELEASE, {SMOKE_MACOS: [annotation(NOT_ACQUIRED)] + [annotation(CAPACITY, "notice")] * 3},
                                  branch="main", refs={"heads/main": [ref("refs/heads/main", SHA)]})
        self.assertDecision(out, output, 'retry: no runner took "smoke (macos-26)"')

    def test_a_tag_retries_while_it_names_the_run_s_commit(self):
        for refs in [
            {"tags/v0.2.0": [ref("refs/tags/v0.2.0", SHA)]},
            # An annotated tag names its tag object, which names the commit.
            {"tags/v0.2.0": [ref("refs/tags/v0.2.0", "e6ccc24487ef515d34a09c8bee6392de1c4f393b", "tag")],
             "tag objects": {"e6ccc24487ef515d34a09c8bee6392de1c4f393b": {"sha": SHA, "type": "commit"}}},
        ]:
            with self.subTest(refs=refs):
                out, output = self.decide(RELEASE, {SMOKE_MACOS: [annotation(NOT_ACQUIRED)]}, refs, branch="v0.2.0")
                self.assertDecision(out, output, 'retry: no runner took "smoke (macos-26)"')

    def test_every_job_no_runner_took_retries(self):
        jobs = with_job(RELEASE, DARWIN_IN_RELEASE, conclusion="cancelled", steps=[])
        out, output = self.decide(jobs, {SMOKE_MACOS: [annotation(NOT_ACQUIRED)], DARWIN_IN_RELEASE: [annotation(NOT_ACQUIRED)]})
        self.assertDecision(out, output, 'retry: no runner took "build-test / darwin-test", "smoke (macos-26)"')

    def test_a_branch_whose_name_a_url_must_escape_retries(self):
        branch = "fix/issue#2%"
        out, output = self.decide(BUILD_TEST, {DARWIN_TEST: [annotation(NOT_ACQUIRED)]},
                                  {f"heads/{branch}": [ref(f"refs/heads/{branch}", SHA)]}, branch=branch)
        self.assertDecision(out, output, 'retry: no runner took "darwin-test"')

    def test_attempt_2_does_not_retry(self):
        out, output = self.decide(BUILD_TEST, {DARWIN_TEST: [annotation(NOT_ACQUIRED)]}, attempt=2)
        self.assertDecision(out, output, "no-retry: attempt 2 is a re-run")

    def test_a_test_failure_does_not_retry(self):
        jobs = with_job(with_job(BUILD_TEST, DARWIN_TEST, conclusion="success", steps=STEPS), GO_STABLE, conclusion="failure")
        out, output = self.decide(jobs, {})
        self.assertDecision(out, output, 'no-retry: "build-test (Go stable)" (failure) did not end for want of a runner')

    def test_a_test_failure_beside_a_job_no_runner_took_does_not_retry(self):
        jobs = with_job(BUILD_TEST, GO_STABLE, conclusion="failure")
        out, output = self.decide(jobs, {DARWIN_TEST: [annotation(NOT_ACQUIRED)]})
        self.assertDecision(out, output, 'no-retry: "build-test (Go stable)" (failure) did not end for want of a runner')

    def test_the_annotation_counts_only_for_its_own_job(self):
        jobs = with_job(with_job(BUILD_TEST, DARWIN_TEST, conclusion="success", steps=STEPS), GO_STABLE,
                        conclusion="cancelled", steps=[])
        out, output = self.decide(jobs, {GO_STABLE: [], DARWIN_TEST: [annotation(NOT_ACQUIRED)]})
        self.assertDecision(out, output, 'no-retry: "build-test (Go stable)" (cancelled) did not end for want of a runner')

    # A step can write the annotation, and a job that no runner took ran none.
    def test_only_a_cancelled_job_without_steps_counts_as_not_acquired(self):
        for conclusion, steps in [("failure", STEPS), ("cancelled", STEPS), ("failure", []), ("timed_out", [])]:
            with self.subTest(conclusion=conclusion, steps=steps):
                jobs = with_job(BUILD_TEST, DARWIN_TEST, conclusion=conclusion, steps=steps)
                out, output = self.decide(jobs, {DARWIN_TEST: [annotation(NOT_ACQUIRED)]})
                self.assertDecision(out, output, f'no-retry: "darwin-test" ({conclusion}) did not end for want of a runner')

    def test_a_head_in_a_fork_does_not_retry(self):
        out, output = self.decide(BUILD_TEST, {DARWIN_TEST: [annotation(NOT_ACQUIRED)]}, head_repository="someone/artifact-fs")
        self.assertDecision(out, output, f'no-retry: the run\'s head is in "someone/artifact-fs", not {REPOSITORY}')

    def test_a_cancellation_without_the_annotation_does_not_retry(self):
        out, output = self.decide(BUILD_TEST, {DARWIN_TEST: [annotation(CAPACITY, "notice")]})
        self.assertDecision(out, output, 'no-retry: "darwin-test" (cancelled) did not end for want of a runner')

    def test_only_the_exact_annotation_retries(self):
        for message in [NOT_ACQUIRED + ".", NOT_ACQUIRED.replace("hosted", "self-hosted"), NOT_ACQUIRED.lower()]:
            with self.subTest(message=message):
                out, output = self.decide(BUILD_TEST, {DARWIN_TEST: [annotation(message)]})
                self.assertDecision(out, output, 'no-retry: "darwin-test" (cancelled) did not end for want of a runner')

    def test_a_run_without_a_failed_job_does_not_retry(self):
        out, output = self.decide(with_job(BUILD_TEST, DARWIN_TEST, conclusion="success", steps=STEPS), {})
        self.assertDecision(out, output, "no-retry: no job failed")

    def test_a_run_whose_ref_moved_on_does_not_retry(self):
        newer = "f0ed1400fb0b9819226d03b2f52d9b93b2365b80"
        for refs, names in [
            ({f"heads/{BRANCH}": [ref(f"refs/heads/{BRANCH}", newer)]}, newer),
            ({}, "no commit"),
            # matching-refs also lists the refs that only start with the name.
            ({f"heads/{BRANCH}": [ref(f"refs/heads/{BRANCH}-2", SHA)]}, "no commit"),
            ({f"heads/{BRANCH}": [ref(f"refs/heads/{BRANCH}", SHA)], f"tags/{BRANCH}": [ref(f"refs/tags/{BRANCH}", newer)]},
             f"{SHA}, {newer}"),
        ]:
            with self.subTest(refs=refs):
                out, output = self.decide(BUILD_TEST, {DARWIN_TEST: [annotation(NOT_ACQUIRED)]}, refs)
                self.assertDecision(out, output, f'no-retry: "{BRANCH}" names {names} now, not {SHA}')

    def test_a_job_s_name_cannot_start_a_line(self):
        for name in ["lint\nretry: forged", "::error::forged", "lint\n::stop-commands::x"]:
            with self.subTest(name=name):
                jobs = with_job(BUILD_TEST, GO_STABLE, name=name, conclusion="failure")
                out, output = self.decide(jobs, {DARWIN_TEST: [annotation(NOT_ACQUIRED)]})
                self.assertDecision(out, output, f"no-retry: {json.dumps(name)} (failure) did not end for want of a runner")
                for line in out.splitlines():
                    self.assertFalse(line.lstrip().startswith(("retry:", "::")), line)


if __name__ == "__main__":
    unittest.main()
