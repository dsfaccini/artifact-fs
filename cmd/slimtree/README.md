# slimtree

slimtree makes git worktrees that take little disk. On macOS it clones the
files of an existing worktree of the repository with APFS clones, which share
their blocks with the originals until either side changes a file. Git sees an
ordinary worktree, so `git status`, commits and branches work as they do after
`git worktree add`. On Linux it serves each worktree from an
[artifact-fs](https://github.com/dsfaccini/artifact-fs) FUSE mount, which reads
file contents from Git's objects as they are needed.

## Measured

A new worktree of [pydantic-ai](https://github.com/pydantic/pydantic-ai)
(6,069 tracked paths, 333 MiB), median of 5 rounds:

| | slimtree | `git worktree add` |
|---|---|---|
| Disk, at the base worktree's commit | 0.84 MiB | 346 MiB |
| Disk, at a pull request's commit | 2.08 MiB | 346 MiB |
| Create | 3.31 s | 1.39 s |
| First `git status` | 38 ms | 420 ms |
| Second `git status` | 35 ms | 31 ms |

slimtree saves disk, not time. It takes longer than git to make a worktree,
because it hashes every file once to check each clone against Git
(`git update-index --refresh`). Its first `git status` is then as fast as the
second, but create plus first `git status` still takes 3.36 s against git's
1.81 s.

How it was measured:

- `slimtree --version`: `slimtree 0.1.0a1 (v0.1.0-alpha.1, f2eed729d076,
  go1.27.4, darwin/arm64)`, the binary that `uvx slimtree@0.1.0a1` installs.
  Since 0.1.0a1, `slimtree new` and `slimtree rm` change only in how they
  release their locks; the cloning and hashing the table times are unchanged.
- Both tools made a new branch: `slimtree new <branch> --from <commit> --path
  <path>` and `git worktree add -b <branch> <path> <commit>`, from the same
  base worktree.
- Disk is the data only the new worktree and its admin directory hold: the sum
  of APFS's private size (`ATTR_CMNEXT_PRIVATESIZE`) over their files, read
  right after the create. `du` counts a clone's shared blocks, so it reports
  345 MiB for either tool. The pull request's commit differs from the base
  worktree's in 56 paths.
- Times are the wall-clock time of each command, at the base worktree's commit.
  At the pull request's commit they were: create 3.16 s and 1.51 s, first
  `git status` 33 ms and 477 ms, second `git status` 33 ms and 28 ms, for
  slimtree and git.
- The tools alternated in order each round. Every run started and ran while the
  5-minute load average was under 20; it stayed between 6.7 and 8.9.
- MacBook Air (Apple M4, 24 GB), macOS 27.2, internal SSD, Git 2.54.0 (Apple
  Git-157), 2026-11-17.

## What works in this release

| Command | macOS on Apple silicon | Linux on x86_64 or arm64 |
|---|---|---|
| `slimtree new <branch>` | Makes a worktree from APFS clones | Makes a worktree served from an artifact-fs mount |
| `slimtree stop <worktree>` | Only for `--backend artifactfs` | Unmounts the worktree and stops its daemon; its changes stay |
| `slimtree resume <worktree>` | Only for `--backend artifactfs` | Mounts the worktree again; `--all` mounts every one, as after a reboot |
| `slimtree rm <worktree>` | Removes an APFS clone worktree | Coming |
| `slimtree list` | Lists the worktrees slimtree made | Lists the worktrees slimtree made |
| `slimtree doctor` | Checks what slimtree needs | Checks the FUSE setup, and warns that `rm` of artifact-fs worktrees is not supported yet |

Release builds are tested on macOS 26, Ubuntu 24.04 and Alpine Linux. Linux
support is complete in a later release: until then, `slimtree rm` refuses an
artifact-fs worktree, and `slimtree stop` unmounts one. On macOS,
`--backend artifactfs` mounts through macFUSE's FSKit backend. It is a beta,
and release builds do not test it.

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
slimtree new fix/login    # makes ~/src/app-login on the branch fix/login
slimtree list
slimtree rm app-login
```

A new worktree goes next to the main worktree, named after it and the last
segment of the branch: `<main worktree>-<last segment>`. Two variants:

- `slimtree new fix/login --path ~/wt/login` makes `~/wt/login` instead.
- `slimtree new spike --from origin/main` makes `~/src/app-spike` on a new
  branch `spike` at `origin/main`.

`slimtree new` clones the main worktree, from any worktree of the repository.
It checks out the branch when it exists, and otherwise makes it at `--from`,
else at `HEAD`.

On Linux, `slimtree new` mounts the worktree instead, and a daemon of its own
serves it. Your changes live in the daemon's state under slimtree's home, and
outlast `slimtree stop` and a reboot:

```bash
slimtree stop app-login      # unmounts it
slimtree resume app-login    # mounts it again
slimtree resume --all        # after a reboot
```

`slimtree rm` takes a worktree by its directory name or its path. It removes
only APFS clone worktrees in this release. It refuses a worktree with changes
unless you pass `--force`. It keeps the worktree's `HEAD` in a ref under
`refs/slimtree/archive/` and never deletes a branch. Run `slimtree --help` for
every flag.

## Source and license

Source: https://github.com/dsfaccini/artifact-fs. Apache-2.0; see
[LICENSE](https://github.com/dsfaccini/artifact-fs/blob/main/LICENSE).

slimtree is built on dsfaccini/artifact-fs, a fork of cloudflare/artifact-fs. It is not affiliated with or endorsed by Cloudflare.
