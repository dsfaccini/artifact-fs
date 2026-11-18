#!/usr/bin/env python3
"""Decide what a run of release.yml builds, and whether it publishes.

A push of a release tag publishes, once the tagged commit is on origin/main.
Every other run is a dry run that builds as $DRY_RUN_TAG, else as
DEFAULT_DRY_RUN_TAG. The gate appends tag, version, stage and publish to
$GITHUB_OUTPUT. When it refuses the run, it exits 1 and appends nothing.
"""

import os
import subprocess
import sys

from build_wheel import release

DEFAULT_DRY_RUN_TAG = "v0.1.0-alpha.1"


def main(env):
    publish = env.get("GITHUB_EVENT_NAME") == "push" and env.get("GITHUB_REF_TYPE") == "tag"
    tag = env.get("GITHUB_REF_NAME", "") if publish else env.get("DRY_RUN_TAG") or DEFAULT_DRY_RUN_TAG
    try:
        version, stage = release(tag)
    except ValueError as e:
        print(f"::error::{e}")
        return 1
    if publish:
        sha = env["GITHUB_SHA"]
        if subprocess.run(["git", "merge-base", "--is-ancestor", sha, "origin/main"]).returncode != 0:
            print(f"::error::{tag} names {sha}, which is not on main.")
            return 1
    outputs = f"tag={tag}\nversion={version}\nstage={stage}\npublish={str(publish).lower()}\n"
    with open(env["GITHUB_OUTPUT"], "a", encoding="utf-8") as f:
        f.write(outputs)
    print(outputs, end="")
    return 0


if __name__ == "__main__":
    sys.exit(main(os.environ))
