#!/usr/bin/env python3
"""Decide whether runner-retry.yml re-runs the failed jobs of a finished run.

GitHub ends a job that no hosted runner took after its acquisition attempts,
as when macOS runners run short, with NOT_ACQUIRED, and the job runs no step.
The retry re-runs the run's failed jobs once: on attempt 1, for a run whose
head is in this repository, when each job that failed or was cancelled
carries NOT_ACQUIRED. Every other failure or cancellation stays visible.

Reads {"jobs": [...], "annotations": [...]} on stdin: the attempt's jobs as the
jobs API lists them, and each annotation of a job that did not succeed, with
the job's ID in "job". Prints the run, its attempt and each job, then a last
line that starts with "retry:" or "no-retry:" and gives the reason.
"""

import argparse
import json
import sys

NOT_ACQUIRED = "The job was not acquired by Runner of type hosted even after multiple attempts"

# Conclusions that leave nothing to re-run. A job that a failed job's
# dependency skipped re-runs with that job.
PASSED = {"success", "skipped", "neutral"}


def decide(jobs, annotations, repository, head_repository, attempt):
    """Return a line for each job, then the decision line."""
    unacquired = {a["job"] for a in annotations if a["message"] == NOT_ACQUIRED}
    lines = [f"  {job['name']} (job {job['id']}): {job['conclusion']}"
             + (", not acquired by a runner" if job["id"] in unacquired else "")
             for job in jobs]
    failed = [job for job in jobs if job["conclusion"] not in PASSED]
    if head_repository != repository:
        return [*lines, f"no-retry: the run's head is in {head_repository}, not {repository}"]
    if attempt != 1:
        return [*lines, f"no-retry: attempt {attempt} is a re-run"]
    if not failed:
        return [*lines, "no-retry: no job failed"]
    if real := [f"{job['name']} ({job['conclusion']})" for job in failed if job["id"] not in unacquired]:
        return [*lines, f"no-retry: {', '.join(real)} did not end for want of a runner"]
    return [*lines, f"retry: no runner took {', '.join(job['name'] for job in failed)}"]


def main(argv, stdin):
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--run", required=True, help="the run's URL, for the log")
    parser.add_argument("--repository", required=True, help="the repository this workflow runs in")
    parser.add_argument("--head-repository", required=True, help="the repository of the run's head")
    parser.add_argument("--attempt", required=True, type=int, help="the run attempt that finished")
    args = parser.parse_args(argv)
    run = json.load(stdin)
    print(f"Run {args.run}, attempt {args.attempt}, head in {args.head_repository}:")
    print("\n".join(decide(run["jobs"], run["annotations"], args.repository, args.head_repository, args.attempt)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:], sys.stdin))
