# slimtree

slimtree makes git worktrees that take little disk. On macOS it clones the
files of an existing worktree of the repository with APFS clones, which share
their blocks with the originals until either side changes a file. Git sees an
ordinary worktree, so `git status`, commits and branches work as they do after
`git worktree add`.

**Alpha.** This is slimtree's first release. Commands, flags and output may
change in any release before 1.0.

## What works in this release

| Command | macOS on Apple silicon | Linux on x86_64 or arm64 |
|---|---|---|
| `slimtree new <branch>` | Makes a worktree from APFS clones | Not in this release yet |
| `slimtree rm <worktree>` | Removes a worktree slimtree made | Not in this release yet |
| `slimtree list` | Lists the worktrees slimtree made | Lists the worktrees slimtree made |
| `slimtree doctor` | Checks what slimtree needs | Checks the FUSE setup, and reports that artifactfs worktrees are not in this release yet |

Release builds are tested on macOS 26, Ubuntu 24.04 and Alpine Linux. On Linux,
`slimtree new` will make worktrees served from an
[artifact-fs](https://github.com/dsfaccini/artifact-fs) FUSE mount. That
backend is not in this release, so `slimtree new` exits with an error there and
makes nothing.

## Install

```bash
uv tool install slimtree
slimtree doctor
```

`slimtree doctor` checks what slimtree needs on this machine and in the current
directory. It installs nothing, and prints the fix for each check that fails.

## Use

In a repository whose main worktree is `~/src/app`:

```bash
cd ~/src/app
slimtree new my-feature    # makes ~/src/app-my-feature on the branch my-feature
slimtree list
slimtree rm app-my-feature
```

`slimtree new` clones the main worktree, from any worktree of the repository.
It checks out the branch when it exists, and otherwise makes it at `--from`,
else at `HEAD`. `--path` puts the worktree elsewhere.

`slimtree rm` refuses a worktree with changes unless you pass `--force`. It
keeps the worktree's `HEAD` in a ref under `refs/slimtree/archive/` and never
deletes a branch. Each `slimtree rm`, and `slimtree prune`, deletes the
archives and recovery refs older than the retention rule, a month by default;
`slimtree retention week|month|forever|<N>d` changes the rule. Run
`slimtree --help` for every flag.

## Source and license

Source: https://github.com/dsfaccini/artifact-fs. Apache-2.0; see
[LICENSE](https://github.com/dsfaccini/artifact-fs/blob/main/LICENSE).

slimtree is built on dsfaccini/artifact-fs, a fork of cloudflare/artifact-fs. It is not affiliated with or endorsed by Cloudflare.
