---
name: artifact-fs
description: Mounts Git remotes with ArtifactFS as local working trees without a full clone. Use when asked to mount a remote repo, explain committed state versus dirty overlay state, or mount multiple repos under one daemon.
---

# ArtifactFS Usage

Use this skill to operate ArtifactFS as a consumer. Prefer the CLI workflow and mounted repos. Do not inspect ArtifactFS internals unless the task is about developing ArtifactFS itself.

## FIRST: Verify Prerequisites

Run from the repo root. If either command fails, STOP and fix the environment first.

```bash
test -e /Library/Filesystems/macfuse.fs || test -e /dev/fuse
go build -tags fskit -o artifact-fs ./cmd/artifact-fs
```

## Critical Rules

- **Use a dedicated state root in your home directory.** Set `ARTIFACT_FS_ROOT`, for example to `$HOME/.local/share/artifact-fs-demo`, so mounts and caches stay isolated. Keep it out of shared directories such as `/tmp`. Another user can create that path first, and ArtifactFS refuses a state root that another user owns or can write.
- **Prefer HTTPS remotes.** For private repos, use Git credential helpers or SSH auth. Do not embed tokens or passwords in the `--remote` URL because `--remote` is a CLI arg and can show up in process listings.
- **`add-repo` does not mount.** It registers a blobless clone and exits. `daemon` mounts all registered repos and stays running.
- **The mounted tree starts from committed Git state.** Remote commits, trees, and refs are the base view. Local edits live in an overlay and appear as dirty state until committed.
- **One daemon can mount many repos.** Register each repo separately, then start one `daemon --root <mount-root>`.
- **Unmount a stale mount with the OS's tool.** On macOS run `diskutil unmount <path>`, because macOS denies `umount` from a terminal for FSKit mounts. On Linux run `fusermount3 -u <path>`, because `umount` needs root.

## Quick Reference

| Task | Command |
|------|---------|
| Register one repo | `./artifact-fs add-repo --name <name> --remote <url> --branch <branch> --mount-root <mount-root>` |
| Mount all registered repos | `./artifact-fs daemon --root <mount-root>` |
| Check mount and overlay state | `./artifact-fs status --name <name>` |
| List registered repos | `./artifact-fs list-repos` |
| Unmount or remove a repo | `./artifact-fs unmount --name <name>` or `./artifact-fs remove-repo --name <name>` |

## Quick Start: One Repo

```bash
export ARTIFACT_FS_ROOT="$HOME/.local/share/artifact-fs-demo"
MOUNT_ROOT="$HOME/artifact-fs-mnt"

./artifact-fs add-repo \
  --name workers-sdk \
  --remote https://github.com/cloudflare/workers-sdk.git \
  --branch main \
  --mount-root "$MOUNT_ROOT"

./artifact-fs daemon --root "$MOUNT_ROOT" &
DAEMON_PID=$!

git -C "$MOUNT_ROOT/workers-sdk" rev-parse HEAD
git -C "$MOUNT_ROOT/workers-sdk" status
./artifact-fs status --name workers-sdk

kill $DAEMON_PID
# If a stale mount remains:
if [ "$(uname)" = Darwin ]; then diskutil unmount "$MOUNT_ROOT/workers-sdk"; else fusermount3 -u "$MOUNT_ROOT/workers-sdk"; fi
```

## Mount Semantics

- Base view = branch `HEAD` from the blobless clone. File contents hydrate on first read.
- Dirty files are overlay state, not part of the base commit. `./artifact-fs status --name <repo>` reports this as `overlay_dirty=true`.
- `git add` and `git commit` inside the mount are supported. After commit or checkout, the watcher re-indexes the new `HEAD` and reconciles stale overlay entries.

## Quick Start: Multiple Repos

```bash
export ARTIFACT_FS_ROOT="$HOME/.local/share/artifact-fs-demo"
MOUNT_ROOT="$HOME/artifact-fs-mnt"

./artifact-fs add-repo --name repo-a --remote https://github.com/org/repo-a.git --branch main --mount-root "$MOUNT_ROOT"
./artifact-fs add-repo --name repo-b --remote https://github.com/org/repo-b.git --branch main --mount-root "$MOUNT_ROOT"

./artifact-fs daemon --root "$MOUNT_ROOT" &
DAEMON_PID=$!

ls "$MOUNT_ROOT/repo-a"
ls "$MOUNT_ROOT/repo-b"
./artifact-fs list-repos

kill $DAEMON_PID
# If stale mounts remain:
for m in "$MOUNT_ROOT/repo-a" "$MOUNT_ROOT/repo-b"; do
  if [ "$(uname)" = Darwin ]; then diskutil unmount "$m"; else fusermount3 -u "$m"; fi
done
```
