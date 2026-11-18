#!/usr/bin/env python3
"""Decide whether runner-retry.yml re-runs the failed jobs of a finished run.

GitHub cancels a job that no hosted runner took after its acquisition
attempts, before any step, and gives it the NOT_ACQUIRED annotation. The
retry re-runs the run's failed jobs once: on attempt 1, for a run whose head
is in this repository and is still its ref's commit, when each job that
failed or was cancelled is such a job. Every other failure or cancellation
stays visible.

Reads the workflow_run event from $GITHUB_EVENT_PATH, and the run's jobs, their
annotations and its ref with gh api. Prints the run, its attempt and each job,
then the decision and its reason, with every name JSON-quoted so no name can
start a line. Appends retry=true or retry=false to $GITHUB_OUTPUT.
"""

import json
import os
import subprocess
import sys
from urllib.parse import quote

NOT_ACQUIRED = "The job was not acquired by Runner of type hosted even after multiple attempts"

# Conclusions that leave nothing to re-run. A job that a failed job's
# dependency skipped re-runs with that job.
PASSED = {"success", "skipped", "neutral"}


def gh(endpoint):
    """Return every page of a GitHub API GET, in order."""
    out = subprocess.run(["gh", "api", "--paginate", "--slurp", endpoint], check=True, stdout=subprocess.PIPE, text=True)
    return json.loads(out.stdout)


def not_acquired(repository, job):
    """Report whether no runner took the job: a step can write the annotation
    too, but only a job that ran a step."""
    if job["conclusion"] != "cancelled" or job.get("steps") != []:
        return False
    pages = gh(f"repos/{repository}/check-runs/{job['id']}/annotations?per_page=100")
    return any(a["message"] == NOT_ACQUIRED for page in pages for a in page)


def ref_commits(repository, name):
    """Return the commit of the branch and of the tag that `name` names."""
    commits = []
    for kind in ["heads", "tags"]:
        pages = gh(f"repos/{repository}/git/matching-refs/{kind}/{quote(name)}")
        for ref in (ref for page in pages for ref in page if ref["ref"] == f"refs/{kind}/{name}"):
            target = ref["object"]
            while target["type"] == "tag":
                (tag,) = gh(f"repos/{repository}/git/tags/{target['sha']}")
                target = tag["object"]
            commits.append(target["sha"])
    return commits


def decide(run, repository):
    """Return whether to re-run the run's failed jobs, and why."""
    head = (run.get("head_repository") or {}).get("full_name")
    name, sha, attempt = run["head_branch"], run["head_sha"], run["run_attempt"]
    print(f"Run {run['html_url']}, attempt {attempt}, {json.dumps(name)} at {sha} in {json.dumps(head)}:")
    if head != repository:
        return False, f"the run's head is in {json.dumps(head)}, not {repository}"
    if attempt != 1:
        return False, f"attempt {attempt} is a re-run"
    pages = gh(f"repos/{repository}/actions/runs/{run['id']}/attempts/{attempt}/jobs?per_page=100")
    jobs = [job for page in pages for job in page["jobs"]]
    failed = [job for job in jobs if job["conclusion"] not in PASSED]
    unacquired = {job["id"] for job in failed if not_acquired(repository, job)}
    for job in jobs:
        note = ", not acquired by a runner" if job["id"] in unacquired else ""
        print(f"  {json.dumps(job['name'])} (job {job['id']}): {job['conclusion']}{note}")
    if not failed:
        return False, "no job failed"
    if real := [f"{json.dumps(job['name'])} ({job['conclusion']})" for job in failed if job["id"] not in unacquired]:
        return False, f"{', '.join(real)} did not end for want of a runner"
    commits = ref_commits(repository, name) if name else []
    if not commits or any(commit != sha for commit in commits):
        return False, f"{json.dumps(name)} names {', '.join(commits) or 'no commit'} now, not {sha}"
    return True, f"no runner took {', '.join(json.dumps(job['name']) for job in failed)}"


def main(env):
    with open(env["GITHUB_EVENT_PATH"], encoding="utf-8") as f:
        run = json.load(f)["workflow_run"]
    retry, reason = decide(run, env["GITHUB_REPOSITORY"])
    print(f"{'retry' if retry else 'no-retry'}: {reason}")
    with open(env["GITHUB_OUTPUT"], "a", encoding="utf-8") as f:
        f.write(f"retry={'true' if retry else 'false'}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(os.environ))
