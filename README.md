<p align="center">
  <img src="artifact-fs.png" alt="ArtifactFS" width="720">
</p>

# ArtifactFS

[![Build & Test](https://github.com/cloudflare/artifact-fs/actions/workflows/build-test.yml/badge.svg)](https://github.com/cloudflare/artifact-fs/actions/workflows/build-test.yml)

> This is a beta release of ArtifactFS. Your mileage may vary.

ArtifactFS is a Git-backed filesystem daemon (FUSE driver) in Go that mounts repositories as normal working trees while avoiding eager blob downloads.

It exposes the tree quickly, then hydrates file contents on demand. That makes it useful for sandboxes, agents, and other short-lived environments where waiting for a full clone is too expensive.

In practice:

* The operating system sees the full tree almost immediately, while the FUSE driver fetches file contents in the background. It prioritizes package manifests, dependency manifests, and source files ahead of large blobs.
* ArtifactFS is part of [Cloudflare Artifacts](http://workers.cloudflare.com/product/artifacts), a versioned filesystem that speaks git, but it also works with any git repo.
* ArtifactFS is optional. You can clone an Artifact repo directly, but larger repos still take time to clone. ArtifactFS lets you mount the repo and fetch blob contents as they are needed.

## What are Cloudflare Artifacts?

[Cloudflare Artifacts](https://workers.cloudflare.com/product/artifacts) is a versioned filesystem that speaks git. It is built for agent toolchains, sandboxes, and CI/CD systems that need fast access to code repositories.

ArtifactFS is the optional FUSE driver -- it lets you mount an Artifact (or any git repo) as a local filesystem without waiting for a full clone.

## Build and Install

Requires Go 1.26+ and a FUSE implementation:

- **macOS** -- [macFUSE](https://osxfuse.github.io/)
- **Linux** -- `fuse3` (`apt install fuse3` on Debian/Ubuntu, `dnf install fuse3` on Fedora)

Install the CLI from the module:

```bash
go install github.com/cloudflare/artifact-fs/cmd/artifact-fs@latest
```

This installs upstream `cloudflare/artifact-fs`, not this fork, so it builds
without the FSKit backend below. Upstream has no `ARTIFACT_FS_FUSE_BACKEND` and
mounts on macOS only through the macFUSE kernel extension. This fork keeps the
upstream module path, so `go install` cannot fetch it. Build it from a checkout:

```bash
GOWORK=off go build -o artifact-fs ./cmd/artifact-fs
```

### Native macOS FSKit backend in this fork

This fork depends on `github.com/dsfaccini/fuse`, a fork of `jacobsa/fuse` that
loads macFUSE's FSKit framework at run time, with no cgo. Every macOS build has
the FSKit backend.

FSKit needs macOS 26 or later and macFUSE 5.5 or later. Enable the `macFUSE`
and `macFUSE (local)` file system extensions under System Settings > General >
Login Items & Extensions > File System Extensions. FSKit uses this user-space
file system extension and needs no kernel extension.

`ARTIFACT_FS_FUSE_BACKEND` selects the macOS transport:

| Value | Transport |
|-------|-----------|
| `auto` (default) | FSKit. Falls back to the macFUSE kernel extension only when FSKit is unavailable on this Mac and the kernel extension is already loaded. |
| `fskit` | FSKit only. |
| `kernel` | The macFUSE kernel extension. macOS prompts to load it when it is not loaded. |

Any other value fails the mount. Linux accepts the same values and ignores them.

A failed FSKit mount logs what to fix, such as enabling the file system
extensions or upgrading macOS. FSKit counts as unavailable when macFUSE is older
than 5.5 or its FSKit framework cannot be loaded, macOS is too old, the helper
tool failed to install, or the file system extension is missing or not enabled. `auto` does not fall back after
any other FSKit failure, because a retry could mount twice. The daemon logs the
transport that mounted each repo.

FSKit reuses directory handles across enumerations. Every backend lists a
directory again when an enumeration restarts. Both macOS backends reject extended
attributes, so macOS writes no AppleDouble `._` files to store them. macOS
attaches extended attributes such as `com.apple.provenance` to nearly every local
file, so copying files onto the mount with `cp` or `ditto` may fail; `cp -X`
skips extended attributes and should avoid it. On an NFS mount that rejects
extended attributes the same way, `cp` exits 1 after copying the data and
`ditto` fails without leaving the file. None of this is measured on a FUSE
mount yet, and `cp -X` is not measured at all. Both
backends serve tracked `._*` and `.DS_Store` files. A `.DS_Store` that Finder
writes lands in the overlay as an untracked file.
The native mount API has no supported cancellation for a permanently stalled
system mount service. macOS treats an FSKit mount as a network volume, so
`umount <path>` returns `Operation not permitted` unless the terminal app has
the Network Volumes permission (Privacy & Security → Files & Folders). The daemon
unmounts through the same `unmount(2)` call. `diskutil unmount <path>` removes an
FSKit mount that `umount` cannot.

Quick start against a public repo:

```bash
export ARTIFACT_FS_ROOT="$HOME/.local/share/artifact-fs"
MOUNT_ROOT="$HOME/artifact-fs-mnt"

# Register, clone, and build the initial snapshot
./artifact-fs add-repo \
  --name workers-sdk \
  --remote https://github.com/cloudflare/workers-sdk.git \
  --ref refs/heads/main \
  --mount-root "$MOUNT_ROOT"

# Start the daemon (mounts via FUSE, blocks until killed)
./artifact-fs daemon --root "$MOUNT_ROOT" &
DAEMON_PID=$!

# Use the repo
ls "$MOUNT_ROOT/workers-sdk/"
cat "$MOUNT_ROOT/workers-sdk/README.md"
git -C "$MOUNT_ROOT/workers-sdk" log --oneline -5

# Cleanup
kill $DAEMON_PID
```

ArtifactFS creates the state root and its subdirectories with mode `0700`, and
narrows existing subdirectories to `0700`. It refuses a state root, or one of
its subdirectories, that another user owns or that group or others can write:
the state root holds each clone's fsmonitor hook, which Git runs on every
`git status`. A state root may be a symlink the current user owns. ArtifactFS
resolves it at startup and builds the paths of repos it registers from the
resolved path. Repos registered before that keep the paths they stored.

## Monitoring hydration and repo status

Check the state of a mounted repo with `status`:

```bash
./artifact-fs status --name workers-sdk
# repo=workers-sdk state=mounted head=d4c61587... ref=main source_ref=refs/heads/main required_commit=none acquisition=not_required base_commit=d4c61587... remote_refresh=enabled ahead=0 behind=0 diverged=false last_fetch=2026-03-27T12:00:00Z result=ok prepare_error=none mount_error=none hydrated_blobs=42 hydrated_bytes=131072 overlay_dirty=false
```

| Field | Meaning |
|-------|---------|
| `state` | `mounted`, `unmounted`, `preparing`, or `failed` |
| `head` / `ref` | Backward-compatible aliases for the current base commit and Git HEAD ref |
| `source_ref` | Canonical remote ref selected during acquisition |
| `required_commit` | Required source commit, or `none` |
| `acquisition` | Historical acquisition evidence: `not_required`, `pending`, or `verified` |
| `base_commit` | Commit backing the currently published filesystem generation |
| `remote_refresh` | Whether daemon and manual remote refreshes are enabled |
| `ahead` / `behind` / `diverged` | Currently zero/false in one-shot CLI status output |
| `prepare_error` | Last redacted preparation error, if any |
| `mount_error` | Last redacted FUSE mount or connection error, if any; a successful mount clears it |
| `hydrated_blobs` / `hydrated_bytes` | Blob count and bytes present in the local hydration cache |
| `overlay_dirty` | `true` if the overlay contains created or modified files; delete-only overlays are not counted |
| `last_fetch` / `result` | `FETCH_HEAD` modification time and `ok` when present; otherwise `never` |

Hydration (blob downloading) is transparent: the file tree is visible immediately after mount, and reads block only until the requested blob is fetched. The first read of a file in a directory also queues up to 256 of that directory's files for background hydration, ranking code and manifests (`package.json`, `go.mod`, `README.md`) ahead of binary files. Listing or walking directories queues nothing. A blobless clone knows a file's size only once its blob is local, so the first `stat` of a file hydrates that one file. A worker takes up to 256 queued blobs, all reads or all prefetches, and fetches the missing ones with one `git fetch` from the promisor remote, so a blobless clone pays one fetch per batch, not one per blob. Queued reads go ahead of queued prefetches, and a blob a prefetch has already downloaded is copied into the cache before more prefetches download. With more than one worker, one worker takes only reads and those copies, so a read never waits for an unrelated prefetch download, though it can wait for one copy of a blob already on disk. With a single worker, a read can wait for a running prefetch download. A read of a file whose own prefetch is downloading waits for that download.

For every repo, ArtifactFS reads blobs (`git cat-file --batch`) and trees (`git ls-tree`), and fetches queued blobs in batches (`git fetch --stdin`), without a controlling terminal, with `GIT_TERMINAL_PROMPT=0` and an empty `SSH_ASKPASS`. When a batch fetch runs, or a read lazily fetches a missing object, a remote that needs an interactive password or passphrase prompt makes the fetch fail instead of prompting. The repo's `core.sshCommand`, credential helpers, and any program set through `GIT_ASKPASS` or `core.askPass` still run. Git Credential Manager can still show its GUI prompt unless `GCM_INTERACTIVE=never` is set.

When the Git index matches the published snapshot and the overlay is empty, startup marks snapshot entries valid for Git's filesystem monitor before opening the mount. This lets the first `git status` use the daemon's change list instead of reading every tracked blob, and each later `git status` receives only the paths the overlay changed since Git's previous query. Paths under `text=auto` are marked, and so are paths under `-text` or `binary` without `eol=lf`. Paths under `text`, `text=input` or `eol=lf` without `text=auto` stay unmarked, because Git's check-in turns CRLF into LF and a blob that holds CRLF then differs from its file; Git compares those paths itself. `-text eol=lf` stays unmarked too, because `git check-attr` prints it like the literal `text=unset`, which Git converts under `eol=lf`. A `git status` that runs while the worktree is unmounted sees the empty mountpoint, so Git's next query gets a rescan. When startup marks the baseline again, its own query takes that rescan, and the first `git status` after the mount receives only the overlay's changes. ArtifactFS skips this baseline when the index or a `.gitattributes` blob is uncertain, when `core.attributesFile` is configured or `core.autocrlf` is set to anything but false, when an `eol=crlf`, `crlf`, `filter`, `ident` or `working-tree-encoding` attribute applies, or when staged work or overlay changes exist; Git then performs its normal status scan.

Use `--hydration-concurrency` to control the number of parallel blob-fetch workers (default 4). Each worker maintains a persistent `git cat-file --batch` process that copies fetched blobs into the cache, so higher values trade memory for faster bulk hydration:

```bash
./artifact-fs daemon --root /tmp --hydration-concurrency 8
```

## Logging

ArtifactFS emits newline-delimited JSON to stderr. Preparation logs include the mode, source, attempt, phase, state, duration, and deadline. Clone and fetch operations retry known transient transport failures up to three total attempts. Preparation has a 30-minute timeout. Caller cancellation stops the active Git command and retry backoff; synchronous preparation records a redacted `prepare canceled` status, while daemon shutdown leaves async preparation queued for restart.

Async existing-clone preparation reports remote configuration, fetch, and branch update as separate `configure_remote`, `fetch`, and `update_branch` phases.

Each error the FUSE library reports becomes a `fuse error` record:

- `ERROR`: a bug the library answers with `EIO`. That is a panic it recovers from an op, logged with its stack; an op that exits through `runtime.Goexit`; or a reply whose byte count falls outside its buffer.
- `WARN`: a reply the kernel refuses, other than with `ENOENT`. On FSKit, a read whose reply is shorter than the size last reported for the file, which the library answers with `EIO`. On Linux, a mount whose `FUSE_POLL` setup failed.
- `DEBUG`: every other failed op, including a request the library cannot decode, because expected errnos such as `EEXIST` fail ops too.

The daemon logs at `INFO`, so it drops the `DEBUG` records. The library reports no `ENOSYS` or `EINTR` from any op, no `ENOENT` from a lookup, no `ENOTEMPTY` from `rmdir` or `rename`, and no `ENODATA` or `ERANGE` from `GetXattr` or `ListXattr`.

A repo's FUSE connection can end while its mount stays in place: after an error fatal to the connection, such as a failed read from the kernel, or after the connection is aborted. The daemon then logs `fuse connection failed, unmounting`, records the connection's error as the repo's `mount_error` (for an abort, which carries no error, `fuse connection ended while the mount point still holds the mount`), unmounts the repo, and mounts it again. While that unmount fails and the mount table still lists the mount point, or the table, the path or the mount point cannot be read, each mount retry tries the unmount again instead of mounting. The daemon looks the mount point up in the kernel's mount table by its absolute path, with symlinks in the parent and a symlinked final component resolved. On macOS the lookup ignores case.

Successful `add-repo`:

```json
{"time":"2026-07-16T12:00:00Z","level":"INFO","msg":"repo preparation started","repo":"workers-sdk","mode":"sync","source":"fresh_clone","attempt":1,"phase":"validate","state":"started","duration_ms":0,"branch":"refs/heads/main","fetch_ref":"main","deadline_set":true,"timeout_ms":1799999}
{"time":"2026-07-16T12:00:04Z","level":"INFO","msg":"repo preparation completed","repo":"workers-sdk","mode":"sync","source":"fresh_clone","attempt":1,"phase":"complete","state":"completed","duration_ms":4217,"deadline_set":true,"timeout_ms":1799999,"head_oid":"d4c61587...","head_ref":"main","snapshot_generation":1}
```

Transient network failure followed by recovery:

```json
{"time":"2026-07-16T12:01:00Z","level":"WARN","msg":"git operation attempt failed","operation":"clone","repo":"workers-sdk","attempt":1,"max_attempts":3,"retryable":true,"duration_ms":842,"timed_out":false,"canceled":false,"error":"HTTP 503: unexpected disconnect"}
{"time":"2026-07-16T12:01:00Z","level":"INFO","msg":"retrying transient git operation failure","operation":"clone","repo":"workers-sdk","attempt":1,"next_attempt":2,"backoff_ms":214}
{"time":"2026-07-16T12:01:02Z","level":"INFO","msg":"git operation recovered","operation":"clone","repo":"workers-sdk","attempts":2,"duration_ms":2087}
```

Preparation timeout:

```json
{"time":"2026-07-16T12:30:00Z","level":"ERROR","msg":"repo preparation failed","repo":"workers-sdk","mode":"sync","source":"fresh_clone","attempt":1,"phase":"clone","state":"failed","duration_ms":1800000,"deadline_set":true,"timeout_ms":1799999,"timed_out":true,"canceled":false,"error":"git clone failed after 1 attempt; caller context: context deadline exceeded"}
```

Diagnostics are bounded and redact credentials and complete remote references. To follow preparation and network activity:

```bash
./artifact-fs daemon --root /tmp 2>/tmp/daemon.log &
tail -f /tmp/daemon.log | grep -Ei 'prepar|git operation'
```

## Async repo preparation

By default, `add-repo` waits for the blobless clone and initial snapshot before returning. Use `--async` when the daemon should prepare the repo in the background:

```bash
./artifact-fs add-repo \
  --name workers-sdk \
  --remote https://github.com/cloudflare/workers-sdk.git \
  --ref refs/heads/main \
  --mount-root /tmp \
  --async
```

The daemon mounts a placeholder immediately. Operations inside that repo mount, such as `ls`, `less`, or `git -C /tmp/workers-sdk status`, wait until the clone/fetch and snapshot publish have completed. If preparation fails, those operations return an I/O error until preparation is retried:

```bash
./artifact-fs status --name workers-sdk
./artifact-fs prepare --name workers-sdk
```

Async HTTPS remotes must use ambient credentials, such as a configured Git credential helper or repo-local Git config. Inline credentials in the remote URL are rejected for async repositories.

For workflows that create the gitdir separately, `--prepared-gitdir` makes the async step fetch and prepare an existing gitdir instead of running `git clone`:

```bash
git init --separate-git-dir /tmp/workers-sdk.git --initial-branch main /tmp/workers-sdk
git -C /tmp/workers-sdk remote add origin https://github.com/cloudflare/workers-sdk.git

./artifact-fs add-repo \
  --name workers-sdk \
  --ref refs/heads/main \
  --mount-root /tmp \
  --async \
  --prepared-gitdir \
  --git-dir /tmp/workers-sdk.git \
  --fetch-ref main
```

## Adopting linked Git worktrees

`--adopt-gitdir` registers an initialized linked worktree without cloning, fetching,
changing refs or upstream tracking, or replacing its Git index. The GitDir must
already contain a valid HEAD and index, and its registered worktree path must
match the mountpoint. Primary GitDirs, relocation and reftable ref storage are
rejected before config writes. The mountpoint must be empty: adoption
uses Git metadata and does not import native working files. This mode preserves normal linked
worktree discovery, shared objects and refs, and private HEAD/index state:

```bash
git -C /path/to/base worktree add --no-checkout -b topic /path/to/mounts/topic main
topic_gitdir=$(git -C /path/to/mounts/topic rev-parse --absolute-git-dir)
git --git-dir="$topic_gitdir" read-tree HEAD
git -C /path/to/base worktree lock --reason 'ArtifactFS workspace' /path/to/mounts/topic
# Save the gitfile outside the mountpoint so the mountpoint is empty.
mv /path/to/mounts/topic/.git /path/to/topic.gitfile

ARTIFACT_FS_ROOT=/path/to/topic-state artifact-fs add-repo \
  --name topic --adopt-gitdir --git-dir "$topic_gitdir" \
  --mount-root /path/to/mounts --blob-cache-dir /path/to/shared-blobs
ARTIFACT_FS_ROOT=/path/to/topic-state artifact-fs daemon --root /path/to/mounts
```

Adoption disables remote refresh and rejects acquisition options (`--remote`,
`--ref`, `--branch`, `--fetch-ref`, `--depth`, `--require-commit`, `--async`, and
`--prepared-gitdir`). An explicit `--refresh never` is accepted. Local HEAD
watching remains active, including shared loose and packed branch refs, so
commits and branch changes reconcile normally. Run ordinary Git fetch/push
commands when needed; the worktree's original remote configuration is retained.
If `origin` embeds credentials, adoption logs a warning that names no remote and proceeds:
Git's lazy fetches of missing blobs and trees use that URL.

ArtifactFS enables `extensions.worktreeConfig`, migrating shared `core.worktree`
and `core.bare=true` settings into the primary worktree's config when necessary.
Existing primary overrides are retained. It pins the adopted worktree's path,
filesystem semantics and FSMonitor hook in that worktree's config; native
siblings retain their settings.

Stop the owning daemon and wait for process exit and actual unmount before
restoring the saved gitfile, unlocking metadata or removing the worktree.
`remove-repo` only updates the registry and Git config when invoked outside the
owning daemon; its return alone does not acknowledge that the live mount has stopped.
It unsets the worktree's `core.fsmonitor` and keeps `extensions.worktreeConfig`,
which sibling worktrees may rely on. Preserve a recovery ref before deleting a
branch containing local commits.

## Verified shallow sources

Use a verified source when a job must inspect the exact revision selected for a deployment:

```bash
artifact-fs add-repo \
  --name workers-sdk-check \
  --remote https://github.com/cloudflare/workers-sdk.git \
  --ref refs/heads/main \
  --require-commit "$DEPLOY_SHA" \
  --depth 1 \
  --refresh never \
  --mount-root /tmp
```

These options express three independent policies: `--require-commit` asserts what the canonical source ref must resolve to, `--depth` bounds transferred history, and `--refresh never` disables daemon and manual remote refreshes. The required commit must be a full 40- or 64-character object ID. If the fetched ref resolves to a different commit, preparation fails before any filesystem generation is published.

ArtifactFS fetches the canonical ref into a private candidate ref, verifies its peeled commit, and publishes directly from that commit rather than ambient `HEAD`. A successful acquisition receipt is persisted and reported as `acquisition=verified`. Verified acquisition is historical evidence, not a continuously re-evaluated boolean.

A required source commit fixes the published base generation: ArtifactFS does not start its HEAD watcher for that repository, so later local Git ref changes are not adopted by the mount. The mounted filesystem remains writable through the overlay, and blob hydration may still contact the promisor remote. This is therefore a **verified source**, not an immutable filesystem.

The remote must advertise Git partial-clone filtering for file contents to hydrate over the network on demand. If it does not, Git downloads the selected revision's blobs eagerly, but depth 1 still prevents historical commits and obsolete blob versions from being transferred.

`--prepared-gitdir` is not supported with `--require-commit` because verified acquisition atomically installs a private Git directory.

## Sandboxes and Containers

See the [generic container example](examples/README.md) for Docker-compatible runtimes or the [Cloudflare Sandbox SDK example](examples/cloudflare-sandbox-sdk/README.md) for Workers and Containers.

## Architecture

ArtifactFS has two distinct phases: a one-shot **setup** (`add-repo`) that registers and usually prepares a fast blobless clone, and a long-running **daemon** that mounts it via FUSE and serves file operations. With `add-repo --async`, setup only registers the repo; the daemon performs clone/fetch and snapshot publishing while FUSE operations wait behind a readiness gate.

```
                         ┌─────────────────────────────────────────────────┐
                         │                    Daemon                       │
                         │                                                 │
  ┌──────────┐  clone    │  ┌──────────┐    ls-tree      ┌──────────────┐  │
  │  Remote  │◄──────────┼──│ GitStore │────────────────►│   Snapshot   │  │
  │   repo   │  fetch    │  │          │  cat-file       │   (SQLite)   │  │
  └──────────┘           │  │ batch    │  --batch-check  │              │  │
                         │  │ pool     │                 │  base_nodes  │  │
                         │  └────┬─────┘                 │  per gen     │  │
                         │       │ cat-file              └──────┬───────┘  │
                         │       │ --batch                      │          │
                         │       ▼                              ▼          │
                         │  ┌──────────┐                 ┌──────────────┐  │
                         │  │  Blob    │                 │   Resolver   │  │
                         │  │  Cache   │                 │              │  │
                         │  │  (disk)  │◄────hydrate─────│ snap + ovl   │  │
                         │  └──────────┘                 │  merged view │  │
                         │       ▲                       └──────┬───────┘  │
                         │       │                              │          │
                         │  ┌────┴─────┐   prefetch       ┌─────┴────────┐ │
                         │  │ Hydrator │◄─────────────────│    Engine    │ │
                         │  │          │                  │              │ │
                         │  │ priority │   ensureOverlay  │ read / write │ │
                         │  │ queue    │   copy-on-write  │ create / rm  │ │
                         │  └──────────┘                  └─────┬────────┘ │
                         │                                      │          │
                         │  ┌──────────┐                 ┌──────┴───────┐  │
                         │  │ Overlay  │◄────────────────│  FUSE Layer  │  │
                         │  │ (SQLite  │  write ops      │  (macFUSE /  │  │
                         │  │  + upper │                 │   /dev/fuse) │  │
                         │  │  dir)    │                 └──────┬───────┘  │
                         │  └──────────┘                        │          │
                         │                                      │          │
                         │  ┌──────────┐  HEAD poll       ┌─────┴────────┐ │
                         │  │ Watcher  │─────────────────►│ Mount point  │ │
                         │  │ (500ms)  │  re-index +      │ /tmp/myrepo  │ │
                         │  └──────────┘  reconcile       └──────────────┘ │
                         └─────────────────────────────────────────────────┘
```

### Data flow

1. **Clone/fetch** -- `add-repo` runs a blobless clone or verified source acquisition. `--depth` controls transferred history independently. Verified acquisition fetches the canonical source ref into a private candidate ref and checks `--require-commit` before atomically installing the Git directory. When the remote advertises partial-clone filtering, only commits, trees, and refs are fetched initially; otherwise Git falls back to downloading blobs eagerly.

2. **Index** -- `git ls-tree -r -t -z <selected-commit>` enumerates every path in the selected tree. Sizes are resolved locally via `git cat-file --batch-check` with `GIT_NO_LAZY_FETCH=1` to avoid network round-trips. The result is bulk-inserted into a SQLite `base_nodes` table as a new generation.

3. **Mount** -- The FUSE layer exposes the tree immediately. A synthesized `.git` gitfile points at the real gitdir so git commands work inside the mount.

4. **Read** -- The Resolver merges the snapshot (base tree) with the overlay (local writes). For base files, reads block until the Hydrator fetches the blob and streams it to the blob cache through a persistent `git cat-file --batch` process. Blobs queued together are fetched with one `git fetch --stdin`; the first read in a directory queues its other files the same way.

5. **Write** -- The Engine promotes base files to the overlay via copy-on-write (hydrate, then copy to the `upper/` directory). Subsequent reads come from the overlay. Deletes are recorded as whiteouts; deleting a file copies it first only while a handle on it is open. No FUSE operation holds a lock while a blob hydrates, so a slow fetch stalls neither a HEAD switch nor other operations. A failed operation returns the errno of its cause, such as `ENOSPC`, and an interrupted one returns `EINTR`.

6. **Background** -- For ordinary workspaces, a watcher polls HEAD/refs every 500ms and republishes HEAD changes. A HEAD change compares the overlay with the new generation while reads and writes continue; they wait only while the result is applied. Repositories with `--require-commit` do not run this watcher, so their verified base commit remains fixed. Remote polling is controlled separately by `--refresh`.

### Subsystems

| Package | Role |
|---------|------|
| `daemon` | Orchestrates repo lifecycle, refresh loop, watcher callbacks |
| `fusefs` | FUSE adapter (inode management, op dispatch), Resolver (merged view), Engine (the adapter's only API: one method per operation) |
| `gitstore` | Git CLI wrapper: clone, fetch, ls-tree, batch pool for `cat-file --batch` |
| `snapshot` | SQLite store for `base_nodes` keyed by `(generation, path)` |
| `overlay` | SQLite metadata + `upper/` directory for local writes, whiteouts, reconciliation |
| `hydrator` | Priority queue with deduped waiters; workers fetch queued blobs in batches. With more than one worker, a reserved worker takes only reads and copies of local blobs and blocks on `reservedReady`; the others block on `workReady` |
| `watcher` | Polls gitdir mtimes (HEAD, index, refs) at 500ms intervals |
| `registry` | SQLite-backed repo config persistence |
| `model` | Shared types and canonical interfaces (`GitStore`, `SnapshotStore`, `OverlayStore`, `Hydrator`) |

## Supported git operations

Work in progress. The table reflects operations exercised by the FUSE E2E suite; some were also tested against [cloudflare/workers-sdk](https://github.com/cloudflare/workers-sdk) mounted via macFUSE.

### Filesystem operations

| Operation | Status | Notes |
|-----------|--------|-------|
| `ls` (root and subdirectories) | Supported | Includes synthesized `.git` gitfile; hydrates nothing |
| `cat` / read file | Supported | Triggers on-demand hydration for unhydrated blobs |
| `stat` (file size, mode) | Supported | Sizes resolved via `git cat-file --batch-check` when the blob is local; otherwise the first `stat` hydrates the file |
| `mkdir` | Supported | Persisted in writable overlay |
| Create new file | Supported | Persisted in writable overlay |
| Write / append to file | Supported | Copy-on-write for tracked files |
| Rename file | Supported | Works for both overlay and tracked (snapshot-only) files |
| Rename directory | Supported | Moves tracked and overlay-created directory trees |
| Delete file (`rm`) | Supported | Whiteout recorded in overlay; fetches nothing unless the file is open |
| `rmdir` | Supported | Checks directory is empty first |
| Truncate | Supported | Hydrates blob before truncating |
| `chmod` | Supported | Preserves executable-bit changes across commits |
| Create symlink (`ln -s`) | Supported | Stores the target in the writable overlay |
| Rename symlink | Supported | Preserves the target and reconciles after commit |
| Symlink read (`readlink`) | Supported | Symlink target read from blob content |

### Git operations

| Operation | Status | Notes |
|-----------|--------|-------|
| `git log` | Supported | Reads from pack objects |
| `git branch` | Supported | |
| `git rev-parse HEAD` | Supported | |
| `git show` | Supported | |
| `git remote -v` | Supported | Credentials stripped from output |
| `git stash` | Supported | Includes push/pop with untracked files |
| `git status` | Supported | ~7s on 5800-entry repo |
| `git diff` | Supported | Shows correct unified diff for modified files |
| `git add` | Supported | Stages modified files |
| `git reset --hard` | Supported | Restores content, modes, and staged creates |
| `git clean -fd` | Supported | Removes untracked files and directory trees |
| `git commit` | Supported | Reconciles content, executable-bit, symlink, rename, and delete changes |
| `git checkout` | Supported | Re-indexes tree, reconciles overlay, refreshes git index |
| `git merge --no-ff` | Supported | Publishes merge commits and reconciles the merged worktree |
| `git rebase` | Supported | Reconciles rewritten commits and branch state |
| `git fetch` | Supported | Background refresh loop fetches periodically |
| `git pull --ff-only` | Supported | Publishes the fast-forwarded snapshot and reconciles the overlay |
| `git push` | Supported | Uses the configured upstream and ambient credentials |

### Known limitations

| Issue | Impact |
|-------|--------|
| Git submodules are not initialized | Gitlink paths appear as empty directories so status stays clean |
| Checkout filters and EOL transforms are not applied to snapshot-only reads | Files use canonical blob bytes until Git writes a transformed overlay |
| `git status` still scans directories to find untracked files; if the cold baseline is skipped because the index, attributes, or overlay are uncertain, Git also scans tracked content normally | Performance |
| `git reset` takes ~6.5s for index refresh | Performance -- same root cause as `git status` |

## Testing

Unit tests:

```bash
go test ./...
```

End-to-end tests mount a git repo via FUSE and exercise filesystem + git operations (including commit and overlay reconciliation). They require a FUSE implementation (macFUSE on macOS, `fuse3` on Linux) and are off by default. On macOS they skip when the selected `ARTIFACT_FS_FUSE_BACKEND` transport cannot mount without prompting to load the macFUSE kernel extension. A run that mounts through FSKit starts at most 8 mounts, and its tests that move HEAD or change the shared mount skip ([macfuse#1231](https://github.com/macfuse/macfuse/issues/1231)).

By default, e2e tests create a local bare repo -- no network required. Set `AFS_E2E_REPO` to test against a real remote.

```bash
# Run e2e tests (uses a local test repo by default)
AFS_RUN_E2E_TESTS=1 go test -v -run TestE2E -count=1 -timeout 20m .

# Run against a specific remote repo
AFS_RUN_E2E_TESTS=1 \
  AFS_E2E_REPO=https://github.com/cloudflare/workers-sdk.git \
  go test -v -run TestE2E -count=1 -timeout 20m .
```

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AFS_RUN_E2E_TESTS` | `0` | Set to `1` to enable end-to-end tests |
| `AFS_E2E_REPO` | local bare repo | Git remote URL for e2e tests. Use an ambient Git credential helper for authenticated HTTPS remotes. |
| `ARTIFACT_FS_ROOT` | `~/.local/share/artifact-fs` (macOS) or `/var/lib/artifact-fs` (Linux) | Runtime data root for the daemon and CLI |

## Contributing

Contributions are welcome, but not all contributions will be accepted. As guidance:

1. **Ensure you open an issue describing your change** - why it's a problem, how to reproduce it (if it's a bug)
2. **Your PR should be clear and concise** - including why it should be upstreamed.
3. **You are expected to have self-reviewed** - any PRs that are straight from automation with glaring issues, that don't build, or don't add good tests are likely to be closed.

AI/LLM submissions are welcome, but overall issue/PR quality is ultimately the responsibility of the submitter, and the codebase is the responsibility (and long term maintenance burden) of the maintainers.

See [AGENTS.md](AGENTS.md) for build commands, architecture details, and conventions. Run `go test ./...` and `go vet ./...` before submitting changes.

Enable the pre-commit checks with `git config core.hooksPath .githooks`. For staged Go changes they run `gofmt`, `go vet`, a CLI build, and the changed packages' unit tests. The hook skips the tests when the machine is under load; set `AFS_PRECOMMIT_TESTS` to `always` or `never` to override, or tune `AFS_PRECOMMIT_LOAD_PER_CPU` (default `1`).

## Credits

The ArtifactFS FUSE driver takes inspiration from and draws from implementation details in:

* [TigrisFS](https://github.com/tigrisdata/tigrisfs/)
* [gitfs](https://github.com/presslabs/gitfs)
* [SlothFS](https://gerrit.googlesource.com/gitfs/)

## License

(c) Cloudflare, 2026. Apache-2.0 licensed.
