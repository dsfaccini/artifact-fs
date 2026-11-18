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
(6,069 tracked paths, 333 MiB), median of 5 rounds.

### macOS

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

### Linux

| | slimtree | `git worktree add` |
|---|---|---|
| Disk, at the base worktree's commit | 3.51 MiB | 348 MiB |
| Disk, at a pull request's commit | 3.51 MiB | 348 MiB |
| Create | 0.41 s | 0.98 s |
| First `git status` | 117 ms | 263 ms |
| Second `git status` | 11 ms | 243 ms |
| Remove | 0.31 s | 0.30 s |

On Linux slimtree saves disk, and it makes a worktree in less than half git's
time. It checks out no files: the mount reads file contents from Git's objects
as they are needed, and the worktree's FSMonitor hook tells `git status` which
files changed. slimtree's `git status` took about the same time in every
round, and git's varied widely, as the bullets below give. Remove takes about
as long as git's at the base worktree's commit, and longer at the pull
request's: `slimtree rm` stops the daemon and archives the worktree's state
before it removes the worktree.

How it was measured:

- `slimtree --version`: `slimtree dev (untagged, 059723a8cedf, go1.27.4,
  linux/amd64)`, built from main 059723a as the release workflow builds it
  (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`), without its version
  stamp.
- Both tools made a new branch at the macOS table's two commits:
  `slimtree --backend artifactfs new <branch> --from <commit> --path <path>`
  and `git worktree add -b <branch> <path> <commit>`, from the same base
  worktree. Each run then ran `git status --porcelain=v1` twice and removed the
  worktree with `slimtree rm <path>` or `git worktree remove <path>`.
- Each slimtree run had its own `SLIMTREE_HOME`, so its blob cache started
  empty. The blob cache is shared, so a later worktree of the same repository
  reuses the blobs an earlier one hydrated.
- Disk is what `du` counts right after the create. It was the same in every
  round: byte for byte for slimtree, within 4 KiB for git. For slimtree at the
  base worktree's commit: the worktree's record directory in its home
  (2.67 MiB, mostly the state root's databases; 2.68 MiB at the pull request's
  commit), its admin directory in the base's `.git/worktrees` (0.83 MiB), and
  the blob cache (0.01 MiB). The mount point is an empty directory. For git: the
  worktree (347 MiB) and its admin directory (0.84 MiB). ext4 shares no blocks
  between files, so `du` counts git's checkout in full.
- The first `git status` hydrated 2 blobs, the root's `.gitignore` and
  `.gitattributes`. They hold 4,272 bytes; `du` of the blob cache, which also
  counts its shard directories, grew by 16 KiB. The slimtree worktree then took
  3.56 MiB, and 3.57 MiB at the pull request's commit. The second `git status`
  hydrated none.
- `slimtree new` copies its own binary (12.1 MiB) into its home when the home
  holds no copy of that build, and every worktree's daemon runs that copy.
  `slimtree rm` deletes the copy once no worktree uses it, so after a home's
  last worktree goes, the next `slimtree new` copies the binary again. Each
  home got the copy before the run, so create's time and disk leave the copy
  out.
- Create's time includes the summary that slimtree prints on a home's first
  run: 5.9 ms (5.6 ms to 6.4 ms), the difference between
  `slimtree --backend artifactfs list` with and without that summary, median
  of 20 pairs at a load average of 1.2.
- `slimtree rm` leaves a 0.88 MiB archive of the worktree's state, which
  `slimtree prune` deletes under the retention rule.
- Times are the wall-clock time of each command, at the base worktree's commit.
  At the pull request's commit they were: create 0.40 s and 0.96 s, first
  `git status` 116 ms and 484 ms, second `git status` 11 ms and 14 ms, remove
  0.30 s and 0.13 s, for slimtree and git.
- Create took between 0.34 s and 0.42 s for slimtree and between 0.97 s and
  0.99 s for git at the base worktree's commit, and between 0.40 s and 0.41 s
  and between 0.96 s and 0.97 s at the pull request's. slimtree's remove took
  between 0.29 s and 0.32 s at the base worktree's commit, and between 0.29 s
  and 0.30 s at the pull request's.
- The daemon's log splits slimtree's create into three phases: until the
  daemon began to mount, the mount, and the rest until `slimtree new` returned.
  Each phase's median, so the three need not sum to create's, was 207 ms,
  105 ms and 103 ms at the base worktree's commit, and 191 ms, 129 ms and 81 ms
  at the pull request's. The last phase took 15 ms in round 2 at the base
  worktree's commit and between 103 ms and 114 ms in its other rounds, and
  between 74 ms and 89 ms in every round at the pull request's.
- git's first `git status` took between 69 ms and 342 ms at the base worktree's
  commit, and between 184 ms and 664 ms at the pull request's. Its second took
  between 50 ms and 348 ms at the base worktree's commit. At the pull request's
  it took between 10 ms and 14 ms in three rounds, and 167 ms and 192 ms in
  rounds 1 and 5, where its first was the fastest. git's remove took between
  0.13 s and 0.34 s at the base worktree's commit. At the pull request's it
  took 0.28 s and 0.29 s in rounds 1 and 5, and between 0.12 s and 0.13 s in
  the others. slimtree's first `git status` took between 112 ms and 133 ms,
  and its second between 10 ms and 12 ms.
- The tools alternated in order each round. Every run started and ran while the
  5-minute load average was under 20; it stayed between 1.93 and 2.05.
- AMD Ryzen 7 7700 (8 cores, 16 threads), 62 GiB, Ubuntu 26.04.1, Linux
  7.0.0-43, ext4 on RAID 1 over two NVMe SSDs, fuse3 3.18.2, Git 2.53.0,
  2026-11-18.

## What works in this release

| Command | macOS on Apple silicon | Linux on x86_64 or arm64 |
|---|---|---|
| `slimtree new <branch>` | Makes a worktree from APFS clones | Makes a worktree served from an artifact-fs mount |
| `slimtree stop <worktree>` | Only for `--backend artifactfs` | Unmounts the worktree and stops its daemon; its changes stay |
| `slimtree resume <worktree>` | Only for `--backend artifactfs` | Mounts the worktree again; `--all` mounts every one, as after a reboot |
| `slimtree rm <worktree>` | Removes an APFS clone worktree | Stops the worktree's daemon, archives its files and removes it |
| `slimtree list` | Lists the worktrees slimtree made | Lists the worktrees slimtree made |
| `slimtree prune` | Deletes the archives and recovery refs `rm` left that are older than the retention rule | Deletes the archives and recovery refs `rm` left that are older than the retention rule, evicts cached blobs not read within it, and removes the daemon binary copies no worktree uses |
| `slimtree retention [<rule>]` | Shows or sets how long archives are kept: `week`, `month` (the default), `forever` or `<N>d` | Shows or sets the rule, which also decides how long a cached blob stays unread before `prune` evicts it |
| `slimtree doctor` | Checks what slimtree needs | Checks the FUSE setup |

Release builds are tested on macOS 26, Ubuntu 24.04 and Alpine Linux. On
macOS, `--backend artifactfs` mounts through macFUSE's FSKit backend. It is a
beta, and release builds do not test it.

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

`slimtree rm` takes a worktree by its directory name or its path. It refuses a
worktree with changes unless you pass `--force`, and `slimtree rm --check` runs
only its checks. It keeps the worktree's `HEAD` in a ref under
`refs/slimtree/archive/` and never deletes a branch. For a worktree on an
artifact-fs mount, Linux's default, it also refuses while another process
works inside the mount, also with `--force`. It then stops the worktree's
daemon, and keeps the files your changes made, with the worktree's git dir, in
`state.tar.gz` in the archive. `--exclude <glob>` leaves out the files at paths
with a component that the glob matches, such as `--exclude node_modules`. Each
`slimtree rm`, and
`slimtree prune`, deletes the archives and recovery refs older than the
retention rule, a month by default, and evicts artifact-fs's cached blobs not
read within it; `slimtree retention week|month|forever|<N>d`
changes the rule. Run `slimtree --help` for every flag.

## Source and license

Source: https://github.com/dsfaccini/artifact-fs. Apache-2.0; see
[LICENSE](https://github.com/dsfaccini/artifact-fs/blob/main/LICENSE).

slimtree is built on dsfaccini/artifact-fs, a fork of cloudflare/artifact-fs. It is not affiliated with or endorsed by Cloudflare.
