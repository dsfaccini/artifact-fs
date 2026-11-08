---
name: artifact-fs
description: Mounts Git remotes with ArtifactFS as local working trees without a full clone, and manages those mounts. Use when asked to mount a remote repo, mount multiple repos under one daemon, explain committed state versus dirty overlay state, check a mount's status, unmount, remount or remove a repo, shrink its blob cache, or clear a stale mount. Skip it when developing ArtifactFS itself.
---

# ArtifactFS Usage

Use this skill to operate ArtifactFS as a consumer. Prefer the CLI workflow and mounted repos. Do not inspect ArtifactFS internals unless the task is about developing ArtifactFS itself.

## FIRST: Verify Prerequisites

Run from the repo root. If either command fails, STOP and fix the environment first.

```bash
test -e /Library/Filesystems/macfuse.fs || test -e /dev/fuse
go build -o artifact-fs ./cmd/artifact-fs
```

## Critical Rules

- **Use a dedicated state root in your home directory.** Set `ARTIFACT_FS_ROOT`, for example to `$HOME/.local/share/artifact-fs-demo`, so mounts and caches stay isolated. Keep it out of shared directories such as `/tmp`. Another user can create that path first, and ArtifactFS refuses a state root that another user owns or can write.
- **Prefer HTTPS remotes.** For private repos, use Git credential helpers or SSH auth. Do not embed tokens or passwords in the `--remote` URL because `--remote` is a CLI arg and can show up in process listings.
- **`add-repo` does not mount.** It registers a blobless clone and exits. `daemon` mounts all registered repos and stays running.
- **The mounted tree starts from committed Git state.** Remote commits, trees, and refs are the base view. Local edits live in an overlay and appear as dirty state until committed.
- **One daemon can mount many repos.** Register each repo separately, then start one `daemon`. Each repo mounts at `<mount-root>/<name>` from its `add-repo --mount-root`, default `$ARTIFACT_FS_ROOT/mnt`; `daemon --root` changes no registered repo. A second daemon on the same `ARTIFACT_FS_ROOT` exits with an error.
- **Unmount a stale mount with the OS's tool.** A stale mount is one no running daemon serves, so `unmount` cannot reach it. A daemon that starts on the same state root unmounts the stale mounts earlier daemons left at its repos' mount points. Without a daemon, on macOS run `diskutil unmount <path>`, because `umount` fails on an FSKit mount unless the terminal app has the Network Volumes permission. On Linux run `fusermount3 -u <path>`, because `umount` needs root.

## Quick Reference

| Task | Command |
|------|---------|
| Register one repo | `./artifact-fs add-repo --name <name> --remote <url> --ref <branch> --mount-root <mount-root>` |
| Mount all registered repos | `./artifact-fs daemon` |
| Check mount and overlay state | `./artifact-fs status --name <name>` |
| List registered repos | `./artifact-fs list-repos` |
| Unmount a repo until remount | `./artifact-fs unmount --name <name>`; waits up to 2 minutes for the running daemon. Exits non-zero if the unmount failed, a later request replaced it, the wait timed out, or no daemon runs. The repo stays unmounted across daemon restarts |
| Mount a repo again | `./artifact-fs remount --name <name>`; unmounts the repo first if it is mounted, then waits like `unmount` |
| Shrink a repo's blob cache | `./artifact-fs evict-cache --name <name> --max-bytes <bytes>`; evicts least recently accessed blobs, never automatic |
| Unmount and remove a repo | `./artifact-fs remove-repo --name <name>`; the running daemon unmounts it at its next registry poll |

## Quick Start: One Repo

```bash
export ARTIFACT_FS_ROOT="$HOME/.local/share/artifact-fs-demo"
MOUNT_ROOT="$HOME/artifact-fs-mnt"

./artifact-fs add-repo \
  --name workers-sdk \
  --remote https://github.com/cloudflare/workers-sdk.git \
  --ref main \
  --mount-root "$MOUNT_ROOT"

./artifact-fs daemon &
DAEMON_PID=$!
for _ in $(seq 60); do ./artifact-fs status --name workers-sdk | grep -q ' state=mounted ' && break; sleep 1; done

git -C "$MOUNT_ROOT/workers-sdk" rev-parse HEAD
git -C "$MOUNT_ROOT/workers-sdk" status
./artifact-fs status --name workers-sdk

kill $DAEMON_PID
# If a stale mount remains:
if [ "$(uname)" = Darwin ]; then diskutil unmount "$MOUNT_ROOT/workers-sdk"; else fusermount3 -u "$MOUNT_ROOT/workers-sdk"; fi
```

## Mount Semantics

- Base view = the blobless clone's `HEAD`, which preparation sets from the registered ref. File contents hydrate on first read, and the first read in a directory hydrates its other files in the background. Listing directories hydrates nothing.
- Dirty files are overlay state, not part of the base commit. `./artifact-fs status --name <repo>` reports `overlay_dirty=true` while the overlay holds a written, created, renamed or chmodded path, or the deletion of a tracked path. In a repo that follows HEAD, a deletion counts until a HEAD change removes the path; with `--require-commit`, it counts for as long as the overlay holds it.
- `git add` and `git commit` inside the mount are supported. After commit or checkout, the watcher re-indexes the new `HEAD` and reconciles stale overlay entries.
- For a moment after a `HEAD` change, the mount can still show the old commit's attributes. On macOS with FSKit it can also show old names, sizes and content, for as long as FSKit caches them. The README section "Kernel caches after a HEAD change" lists what each backend shows.

## Quick Start: Multiple Repos

```bash
export ARTIFACT_FS_ROOT="$HOME/.local/share/artifact-fs-demo"
MOUNT_ROOT="$HOME/artifact-fs-mnt"

./artifact-fs add-repo --name repo-a --remote https://github.com/org/repo-a.git --ref main --mount-root "$MOUNT_ROOT"
./artifact-fs add-repo --name repo-b --remote https://github.com/org/repo-b.git --ref main --mount-root "$MOUNT_ROOT"

./artifact-fs daemon &
DAEMON_PID=$!
for r in repo-a repo-b; do
  for _ in $(seq 60); do ./artifact-fs status --name "$r" | grep -q ' state=mounted ' && break; sleep 1; done
done

ls "$MOUNT_ROOT/repo-a"
ls "$MOUNT_ROOT/repo-b"
./artifact-fs list-repos

kill $DAEMON_PID
# If stale mounts remain:
for m in "$MOUNT_ROOT/repo-a" "$MOUNT_ROOT/repo-b"; do
  if [ "$(uname)" = Darwin ]; then diskutil unmount "$m"; else fusermount3 -u "$m"; fi
done
```
