> This repository is a fork of [cloudflare/artifact-fs](https://github.com/cloudflare/artifact-fs).
> artifact-fs's docs are in
> [docs/artifact-fs.md](https://github.com/dsfaccini/artifact-fs/blob/main/docs/artifact-fs.md).

# slimtree

slimtree makes git worktrees that take little disk. On macOS it clones the
files of an existing worktree of the repository with APFS clones, which share
their blocks with the originals until either side changes a file. Git sees an
ordinary worktree, so `git status`, commits and branches work as they do after
`git worktree add`. On Linux it serves each worktree from an
[artifact-fs](https://github.com/dsfaccini/artifact-fs/blob/main/docs/artifact-fs.md)
FUSE mount, which reads file contents from Git's objects as they are needed.

## Quick start

```bash
uv tool install slimtree
cd ~/src/app              # the repository's main worktree
slimtree doctor           # checks what slimtree needs; installs nothing
slimtree new fix/login    # makes ~/src/app-login on the branch fix/login
slimtree list
slimtree rm app-login
```

## Measured

A new worktree of [pydantic-ai](https://github.com/pydantic/pydantic-ai)
(6,069 tracked paths, 333 MiB), median of 5 rounds.

### macOS

| | slimtree | `git worktree add` |
|---|---|---|
| Disk, at the base worktree's commit | 0.84 MiB | 346 MiB |
| Disk, at a pull request's commit | 2.07 MiB | 346 MiB |
| Create | 1.10 s | 1.49 s |
| First `git status` | 35 ms | 453 ms |
| Second `git status` | 33 ms | 29 ms |
| Remove | 0.39 s | 0.33 s |

slimtree saves disk and, by median, makes a worktree faster than git. It
clones the base worktree's files instead of writing them, and records each
clone's stat data in the new worktree's index, so its first `git status` hashes
no clone: it took 35 ms against its second's 33 ms at the base worktree's
commit, and 38 ms against 31 ms at the pull request's. Create plus first
`git status` takes 1.16 s against git's 1.85 s at the base worktree's commit,
and 1.17 s against 1.42 s at the pull request's. Remove takes longer:
`slimtree rm` archives the worktree's record and keeps a recovery ref of its
HEAD before it removes the worktree.

slimtree checks no clone byte for byte against its blob. Its trust is only as
good as the base's own `git status`: its index's stat data, `core.checkStat`,
`core.trustctime` and its fsmonitor.

<details>
<summary>When slimtree trusts a clone</summary>

slimtree trusts a clone only when all of these hold:

- the base's `git status` does not list the file, and the base's HEAD has the
  same mode and blob there as the new worktree's commit;
- slimtree did not rewrite the clone from Git;
- the base's index does not mark the path assume-unchanged or skip-worktree;
- the base's attributes convert nothing for the path;
- the clone is a regular file of its blob's size;
- its mtime is older than the second in which slimtree read the base's HEAD;
- the base's HEAD is the same commit when slimtree registers the worktree.

</details>

<details>
<summary>How it was measured</summary>

- `slimtree --version`: `slimtree dev (untagged, e5fed326a40c, go1.27.4,
  darwin/arm64)`, built from main e5fed32 as the release workflow builds it
  (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`), without its version
  stamp.
- Both tools made a new branch: `slimtree new <branch> --from <commit> --path
  <path>` and `git worktree add -b <branch> <path> <commit>`, from the same
  base worktree. Each run then read the disk, ran `git status --porcelain=v1`
  twice, and removed the worktree with `slimtree rm <path>` or
  `git worktree remove <path>`.
- The first `git status` ran between 2.0 s and 15.3 s after the create. The
  disk readings fill that gap. In 9 of the 20 runs they took less than 2 s,
  and the run waited out the rest. Git here compares file times in whole
  seconds, as a Git built without `USE_NSEC` does, so a gap over 1 s puts the
  first `git status`'s index write in a later second than every file the
  create wrote. git's second `git status` then took between 28 ms and 47 ms in
  every round.
- Disk is the data only the new worktree and its admin directory hold: the sum
  of APFS's private size (`ATTR_CMNEXT_PRIVATESIZE`) over their files, read
  right after the create. `du` counts a clone's shared blocks, so it reports
  345 MiB for either tool. The pull request's commit differs from the base
  worktree's in 56 paths.
- Times are the wall-clock time of each command, at the base worktree's commit.
  At the pull request's commit they were: create 1.14 s and 1.21 s, first
  `git status` 38 ms and 196 ms, second `git status` 31 ms and 28 ms, remove
  0.40 s and 0.35 s, for slimtree and git.
- Create took between 1.04 s and 1.32 s for slimtree and between 1.17 s and
  2.75 s for git at the base worktree's commit, and between 1.03 s and 4.73 s
  and between 1.12 s and 1.48 s at the pull request's. There, two of
  slimtree's five creates, 1.78 s and 4.73 s, took longer than git's slowest.
- Traces of 10 more slimtree creates at the pull request's commit show that
  refresh hashed no file in any of them. The slow ones spent their time in
  slimtree's clone and `lstat` passes and in git's own commands. The slowest
  traced create ran at a 1-minute load of 20.5 to 29.5, and every one of its
  steps slowed. Memory pressure on the machine is a likely cause, but the runs
  recorded no memory data, and the 4.73 s round itself has no trace.
- git's first `git status` took between 154 ms and 960 ms at the base
  worktree's commit, and between 79 ms and 506 ms at the pull request's.
  slimtree's took between 32 ms and 57 ms, and between 34 ms and 47 ms, and its
  second between 28 ms and 37 ms, and between 29 ms and 34 ms.
- Remove took between 0.36 s and 0.50 s for slimtree and between 0.31 s and
  0.39 s for git at the base worktree's commit, and between 0.36 s and 0.52 s
  and between 0.33 s and 0.52 s at the pull request's.
- The tools alternated in order each round. Every run started and ran while the
  5-minute load average was under 20; it stayed between 5.9 and 7.0.
- MacBook Air (Apple M4, 24 GB), macOS 27.2, internal SSD, Git 2.54.0 (Apple
  Git-157), 2026-11-18.

</details>

### Linux

| | slimtree | `git worktree add` |
|---|---|---|
| Disk, at the base worktree's commit | 3.51 MiB | 348 MiB |
| Disk, at a pull request's commit | 3.51 MiB | 348 MiB |
| Create | 0.34 s | 0.99 s |
| First `git status` | 140 ms | 476 ms |
| Second `git status` | 11 ms | 10 ms |
| Remove | 0.28 s | 0.13 s |

On Linux slimtree saves disk, makes a worktree in about a third of git's time,
and runs the first `git status` 3.4 times as fast as git at the base
worktree's commit and 3.7 times as fast at the pull request's. Its second
`git status` is within about 1.5 ms of git's. It checks out no files: the mount
reads file contents from Git's objects as they are needed, and the worktree's
FSMonitor hook tells `git status` which files changed. Remove takes longer:
`slimtree rm` stops the daemon and archives the worktree's state before it
removes the worktree.

<details>
<summary>How it was measured</summary>

- `slimtree --version`: `slimtree dev (untagged, 9888021600a7, go1.27.4,
  linux/amd64)`, built from 9888021 (#129) as the release workflow builds it
  (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`), without its version
  stamp.
- Both tools made a new branch at the macOS table's two commits:
  `slimtree --backend artifactfs new <branch> --from <commit> --path <path>`
  and `git worktree add -b <branch> <path> <commit>`, from the same base
  worktree. Each run then waited 2 s, ran `git status --porcelain=v1` twice,
  and removed the worktree with `slimtree rm <path>` or
  `git worktree remove <path>`.
- The wait is for Git, which on this machine compares file times in whole
  seconds, as a Git built without `USE_NSEC` does. A file whose mtime shares
  the second in which Git last wrote the index is racily clean, and every
  `git status` hashes it again until one writes the index in a later second.
  Without the wait, the first `git status` could write the index in the same
  second as `git worktree add` wrote its files, and git's second `git status`
  then took up to 433 ms. With the wait, as when a person runs `git status`
  seconds after making a worktree, git's second `git status` took between
  9.3 ms and 10.8 ms in every round.
- git's first `git status` still hashes the files `git worktree add` wrote in
  the second it wrote the index, and how many those are depends on where a
  second boundary fell during the checkout: between 862 and 5,246 of 6,057
  files at the base worktree's commit, and between 716 and 3,928 of 6,069 at
  the pull request's. Its time rose with that number: between 116 ms and
  667 ms, and between 85 ms and 608 ms. slimtree's files carry their commit's
  time, before the index's, so none of them is racily clean. Its first
  `git status` took between 118 ms and 157 ms, and its second between 10 ms
  and 13 ms.
- The wait also slows slimtree's first `git status`, from 117 ms to about
  140 ms, measured on a build of 059723a. slimtree and git run the same
  instructions at either wait, and a fixed-work program unrelated to slimtree
  slows the same way, so the cause is how this machine behaves after it
  idles. The mechanism is not confirmed.
- Each slimtree run had its own `SLIMTREE_HOME`, so its blob cache started
  empty. The blob cache is shared, so a later worktree of the same repository
  reuses the blobs an earlier one hydrated.
- Disk is what `du` counts right after the create. It was the same in every
  round: byte for byte for slimtree, within 8 KiB for git. For slimtree at the
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
  run: 6.1 ms (5.6 ms to 11.4 ms), the difference between
  `slimtree --backend artifactfs list` with and without that summary, median
  of 20 pairs at a load average of 1.9.
- `slimtree rm` leaves a 0.88 MiB archive of the worktree's state, which
  `slimtree prune` deletes under the retention rule.
- Times are the wall-clock time of each command, at the base worktree's commit.
  At the pull request's commit they were: create 0.34 s and 0.97 s, first
  `git status` 133 ms and 489 ms, second `git status` 11 ms and 10 ms, remove
  0.28 s and 0.13 s, for slimtree and git.
- Create took between 0.29 s and 0.35 s for slimtree and between 0.98 s and
  1.01 s for git at the base worktree's commit, and between 0.33 s and 0.40 s
  and between 0.97 s and 1.00 s at the pull request's. Remove took 0.28 s in
  every round for slimtree at the base worktree's commit and between 0.27 s
  and 0.28 s at the pull request's, and between 0.13 s and 0.14 s and between
  0.12 s and 0.14 s for git.
- The daemon's log splits slimtree's create into three phases: until the
  daemon began to mount, the mount, and the rest until `slimtree new` returned.
  Each phase's median, so the three need not sum to create's, was 210 ms,
  105 ms and 13 ms at the base worktree's commit, and 194 ms, 133 ms and 13 ms
  at the pull request's. In the last phase, `awaitMount` reads the daemon's
  status each time the daemon writes it, then checks the mount with
  `git rev-parse`, and `slimtree new` writes the worktree's record. It took
  between 13 ms and 15 ms at the base worktree's commit, and between 12 ms and
  14 ms at the pull request's.
- The tools alternated in order each round. Every run started and ran while the
  5-minute load average was under 20; it stayed between 1.88 and 1.98.
- AMD Ryzen 7 7700 (8 cores, 16 threads), 62 GiB, Ubuntu 26.04.1, Linux
  7.0.0-43, ext4 on RAID 1 over two NVMe SSDs, fuse3 3.18.2, Git 2.53.0,
  2026-11-18.

</details>

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

## Use

`slimtree doctor` checks what slimtree needs on this machine and in the current
directory. It installs nothing, and prints the fix for each check that fails.

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
`refs/slimtree/archive/` and never deletes a branch. Run `slimtree --help` for
every flag.

<details>
<summary>What <code>slimtree rm</code> keeps, and for how long</summary>

For a worktree on an artifact-fs mount, Linux's default, `slimtree rm` also
refuses while another process works inside the mount, also with `--force`. It then stops the worktree's
daemon, and keeps the files your changes made, with the worktree's git dir, in
`state.tar.gz` in the archive. `--exclude <glob>` leaves out the files at paths
with a component that the glob matches, such as `--exclude node_modules`. Each
`slimtree rm`, and
`slimtree prune`, deletes the archives and recovery refs older than the
retention rule, a month by default, and evicts artifact-fs's cached blobs not
read within it; `slimtree retention week|month|forever|<N>d`
changes the rule.

</details>

## Source and license

Source: https://github.com/dsfaccini/artifact-fs. Apache-2.0; see
[LICENSE](https://github.com/dsfaccini/artifact-fs/blob/main/LICENSE).

slimtree is built on dsfaccini/artifact-fs, a fork of cloudflare/artifact-fs. It is not affiliated with or endorsed by Cloudflare.
