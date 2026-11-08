<p align="center">
  <img src="artifact-fs.png" alt="ArtifactFS" width="720">
</p>

# ArtifactFS

[![Build & Test](https://github.com/cloudflare/artifact-fs/actions/workflows/build-test.yml/badge.svg)](https://github.com/cloudflare/artifact-fs/actions/workflows/build-test.yml)

> This is a beta release of ArtifactFS. Your mileage may vary.

ArtifactFS is a Git-backed filesystem daemon (FUSE driver) in Go that mounts repositories as normal working trees while avoiding eager blob downloads.

It exposes the tree quickly, then hydrates file contents on demand. That makes it useful for sandboxes, agents, and other short-lived environments where waiting for a full clone is too expensive.

In practice:

* The operating system sees the full tree almost immediately. The FUSE driver fetches a file's contents when the file is opened or its size is first needed, and opening a file for reading also prefetches the other files of its directory in the background. It prioritizes package manifests, dependency manifests, and source files ahead of images, archives, and videos.
* ArtifactFS is part of [Cloudflare Artifacts](http://workers.cloudflare.com/product/artifacts), a versioned filesystem that speaks git, but it also works with any git repo.
* ArtifactFS is optional. You can clone an Artifact repo directly, but larger repos still take time to clone. ArtifactFS lets you mount the repo and fetch blob contents as they are needed.

## What are Cloudflare Artifacts?

[Cloudflare Artifacts](https://workers.cloudflare.com/product/artifacts) is a versioned filesystem that speaks git. It is built for agent toolchains, sandboxes, and CI/CD systems that need fast access to code repositories.

ArtifactFS is the optional FUSE driver -- it lets you mount an Artifact (or any git repo) as a local filesystem without waiting for a full clone.

## Build and Install

Requires Go 1.26.5+ and a FUSE implementation:

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

After a HEAD change, an FSKit mount can show the old commit for longer than the
other backends; see [Kernel caches after a HEAD change](#kernel-caches-after-a-head-change).
FSKit reuses directory handles across enumerations. Every backend lists a
directory again when an enumeration restarts. On FSKit mounts, artifact-fs
checks a file's owner permission bits itself when the file is opened or
truncated. Both macOS backends reject extended
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
system mount service. The daemon stops waiting for a mount after 30 seconds,
records the failure, and mounts the repo again only once the stalled mount has
ended. macOS treats an FSKit mount as a network volume, so
`umount <path>` returns `Operation not permitted` unless the terminal app has
the Network Volumes permission (Privacy & Security → Files & Folders). The daemon
unmounts an FSKit mount through `unmount(2)`, then through `diskutil unmount`
when `unmount(2)` is refused. That holds for a mount it serves and for one it
unmounts by its path, such as one an earlier daemon left or a late mount the
fuse library could not unmount: the mount table marks an FSKit volume, so the
daemon tells one apart without touching it. `diskutil unmount <path>` removes
an FSKit mount that `umount` cannot.

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

# Start the daemon (mounts each repo where add-repo registered it, blocks until killed)
./artifact-fs daemon &
DAEMON_PID=$!
for _ in $(seq 60); do ./artifact-fs status --name workers-sdk | grep -q ' state=mounted ' && break; sleep 1; done

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
the state root holds the hydrated blobs that reads serve, the fsmonitor hook of
each clone in its default git dir, which Git runs on every `git status`, and,
in `config/tmp`, the SSH wrapper and other files that ArtifactFS's Git commands
run. A state root may be a symlink the current user owns. ArtifactFS
resolves it at startup and builds the paths of repos it registers from the
resolved path. Repos registered before that keep the paths they stored.

## Monitoring hydration and repo status

Check the state of a mounted repo with `status`:

```bash
./artifact-fs status --name workers-sdk
# repo=workers-sdk state=mounted head=d4c61587... ref=main source_ref=refs/heads/main required_commit=none acquisition=not_required base_commit=d4c61587... served_commit=d4c61587... served_generation=3 published_commit=d4c61587... published_generation=3 phase=serve remote_refresh=enabled ahead=0 behind=0 diverged=false last_fetch=2026-03-27T12:00:00Z result=ok prepare_error=none mount_error=none last_error=none last_error_phase=none last_error_at=none next_retry=none attempts=0 hydrated_blobs=42 hydrated_bytes=131072 overlay_dirty=false
```

| Field | Meaning |
|-------|---------|
| `state` | `mounted`, `degraded` (the FUSE mount is down or the last remote refresh failed), `unmounted`, `preparing`, or `failed`. A repo reports `preparing` while the CLI prepares it, for `add-repo` without `--async` or for `prepare`. An async repo reports `preparing` or `failed` while its preparation is pending or has failed, also while its mount is down. Without a running daemon, a repo reports these prepare states, and `unmounted` otherwise. Only an async repo reports `failed`. After a failed CLI preparation, a repo reports `unmounted`, or the state of the mount the daemon keeps serving, with the error in `prepare_error` |
| `head` / `ref` | Backward-compatible aliases for the current base commit and Git HEAD ref |
| `source_ref` | The source ref `add-repo` registered, as a full ref name (`--ref`, default `refs/heads/main`); `HEAD` for an adopted worktree |
| `required_commit` | Required source commit, or `none` |
| `acquisition` | Historical acquisition evidence: `not_required`, `pending`, or `verified` |
| `base_commit` | The served commit while the daemon serves the repo, otherwise the published one |
| `served_commit` / `served_generation` | Commit and snapshot generation of the daemon's runtime for the repo, also while its FUSE mount is down and the daemon retries it; `none`/`0` when no daemon runs the repo |
| `published_commit` / `published_generation` | Commit and generation the snapshot store last published. After a failed overlay transition they run ahead of the served ones until the daemon's retry succeeds |
| `phase` | The step the daemon's runtime for the repo is in: `prepare` (async preparation), `mount` (including retries of a FUSE mount that failed), `serve` (a mount is served) or `unmount`; `none` when no daemon runs or before it records one |
| `remote_refresh` | Whether daemon and manual remote refreshes are enabled |
| `ahead` / `behind` / `diverged` | Divergence of HEAD, at the daemon's last successful refresh, from the ref that refresh fetched: the fetch ref's tracking ref, or a `--require-commit` repo's source ref. A `--require-commit` repo with `--depth` reports `0` once its source moves by `--depth` commits or more, or is rewritten: each refresh of it passes `--depth`, which then leaves the new tip no history in common with HEAD. A smaller move reports exact counts, but the refresh still shortens HEAD's history in the git dir and adds a shallow boundary. Earlier builds compared with `--ref`'s branch, which gave other counts when the fetch ref differs from `--ref`, and reported `0` for a `--require-commit` repo |
| `prepare_error` | Last redacted preparation error, if any |
| `mount_error` | `last_error` when any step of mounting the repo failed last (phase `mount`: from cloning or fetching its source and building its snapshot to the FUSE mount itself, including unmounting a dead mount before a retry) or its FUSE connection failed or exited (phase `serve`); a mount that serves clears it |
| `last_error` / `last_error_phase` / `last_error_at` | Last redacted failure the daemon retries, the phase that failed (`mount`, `serve`, `unmount`, `head_reconcile`, `fetch` or `registry_sync`) and when. `serve` is a FUSE connection that failed or exited. A success of that phase clears it |
| `next_retry` / `attempts` | When the daemon retries the failed phase, and how many times in a row it failed; `next_retry` is `none` when no daemon runs |
| `hydrated_blobs` / `hydrated_bytes` | Blob count and bytes in the repo's blob cache, read from counters kept with the cache (see [Evicting the blob cache](#evicting-the-blob-cache)) |
| `overlay_dirty` | `true` if the overlay holds a created, modified or renamed file, a symlink, a directory, or the delete of a path that `base_commit` has. The delete of a path `base_commit` lacks, such as an editor's removed swap file, does not count. In a repo that follows HEAD, a delete counts until a HEAD change removes its path. A `--require-commit` repo does not follow HEAD, so its deletes count for as long as the overlay holds them |
| `last_fetch` / `result` | Result (`ok` or `failed`) of the daemon's last remote refresh, with the time of its last successful one; or `ok` with the time of a later fetch that left `FETCH_HEAD` filled, such as a `fetch` command that succeeded; otherwise `never`. Git empties `FETCH_HEAD` when a fetch starts, so a fetch that is still running or failed before it received the remote's objects does not count |

`status` reads what the daemon records in the registry, so it reports the same
state from any process. Only one daemon runs per state root: it holds a lock on
`config/daemon.lock`, and a second `daemon` on that root exits with an error.

When you upgrade, stop the running daemon before you start the new one. A
daemon from a build without the daemon lock takes no lock, so a new daemon
would start beside it on the same state root.

## Unmounting and remounting a repo

`unmount` asks the running daemon to unmount a repo and keep it unmounted, also
across daemon restarts, until `remount`. `remount` asks it to mount the repo
again, unmounting it first if it is mounted. Both wait until the daemon reports
the outcome. They exit non-zero when the daemon failed, when no daemon runs on
the state root, when a later `unmount` or `remount` replaced the request, or
after two minutes without an outcome. The daemon retries a failed unmount or
mount; `status` shows the retry. The daemon runs each unmount on its own
thread. A stop runs there whole, so it never holds up the daemon's other repos:
it unmounts the repo, retrying a busy mount for up to 20 seconds, then waits up
to 30 seconds for the repo's FUSE server and workers. A stop whose unmount has
not returned after 21 seconds keeps waiting, and `status` shows `unmount has
not returned after 21s` as `last_error`, with `last_error_phase` `unmount`.
A stop whose wait for the FUSE server and workers times out has unmounted the
repo: `unmount` or `remount` exits non-zero, `status` shows `unmounted` with
`timed out draining unmounted repo` as `last_error` and `last_error_phase`
`unmount`, and the daemon retries the stop at its next sync.
Before a mount, the daemon waits at most 21 seconds for an unmount it starts,
and does not wait for the one it starts after a FUSE connection fails. An
unmount that hangs, as one can on a dead FSKit volume, holds up the daemon's
other repos for at most that one wait, then stalls only its own repo. While an
unmount the daemon started outside a stop has not returned, `unmount` and
`remount` of the repo fail at once with `an earlier unmount of the repo has not
returned`, and `status` shows that error as `last_error`, with
`last_error_phase` `unmount` while a stop waits for it and `mount` while a mount
retry does.

A request stays in the registry until the daemon carries it out, also after the
command timed out or the daemon stopped. A later `unmount` or `remount` replaces
it.

```bash
./artifact-fs unmount --name workers-sdk
./artifact-fs remount --name workers-sdk
```

The commands wake the daemon through the owner-only socket
`config/daemon.sock`, which carries no data. When the socket's path,
`<state root>/config/daemon.sock`, does not fit a Unix socket address (at most
103 bytes on macOS, 107 on Linux), the daemon opens no socket and acts at its
next registry poll, within 3 seconds.

Hydration (blob downloading) is transparent: the file tree is visible immediately after mount, and reads block only until the requested blob is fetched. The first time a base file in a directory is opened for reading or read at a given snapshot generation, the mount also queues up to 256 of that directory's files, the opened one included, for background hydration, ranking code and manifests (`package.json`, `go.mod`, `README.md`) ahead of binary files. Listing or walking directories queues nothing. A blobless clone knows a file's size only once its blob is local, so the first `stat` of a file hydrates that one file. A worker takes up to 256 queued blobs, all reads or all prefetches, and fetches the missing ones with one `git fetch` from the promisor remote, so a blobless clone pays one fetch per batch, not one per blob. Queued reads go ahead of queued prefetches, and a blob a prefetch has already downloaded is copied into the cache before more prefetches download. With more than one worker, one worker takes only reads and those copies, so a read never waits for an unrelated prefetch download, though it can wait for one copy of a blob already on disk. With a single worker, a read can wait for a running prefetch download. A read of a file whose own prefetch is downloading waits for that download.

For every repo, ArtifactFS reads blobs (`git cat-file --batch`) and trees (`git ls-tree`), and fetches queued blobs in batches (`git fetch --stdin`), without a controlling terminal, with `GIT_TERMINAL_PROMPT=0`, an empty `SSH_ASKPASS` and `SSH_ASKPASS_REQUIRE=never`. Unless `GIT_SSH` is set, ArtifactFS sets it to a wrapper that runs `ssh -o BatchMode=yes`, so SSH tries no passwords that a server could count as failed logins. Git runs `GIT_SSH` only when neither `GIT_SSH_COMMAND` nor a `core.sshCommand` applies, including one that an include sets for the repository being cloned, so the SSH command the user set wins. When a batch fetch runs, or a read lazily fetches a missing object, a remote that needs an interactive password or passphrase prompt makes the fetch fail instead of prompting. The SSH command the user set, credential helpers, and any program set through `GIT_ASKPASS` or `core.askPass` still run.

Every other Git command ArtifactFS runs that can contact a remote runs the same way: without a controlling terminal, with `GIT_TERMINAL_PROMPT=0`, an empty `SSH_ASKPASS`, `SSH_ASKPASS_REQUIRE=never`, and BatchMode for plain `ssh`. These are the clones, `ls-remote` calls and fetches of `add-repo`, `prepare`, the daemon's preparation and remote refresh, and the `fetch` command, every `read-tree` that builds or moves a git dir's index, and the `diff-tree` and `diff-index` that check a failed move. In a git dir cloned with `--filter=tree:0`, those three fetch the trees it lacks. ArtifactFS's other Git commands read only commits and refs, or run with `GIT_NO_LAZY_FETCH=1`. A remote that needs a password, a passphrase or a new host key fails the command, and `status` reports a failed refresh. The SSH command the user set, credential helpers, and any program set through `GIT_ASKPASS` or `core.askPass` still run.

Git Credential Manager can still show its GUI prompt unless `GCM_INTERACTIVE=never` is set. Other waits are not terminal prompts either, so these settings do not end them: an ssh-agent key added with `ssh-add -c`, a FIDO security key waiting for a touch, `ControlMaster ask` or `autoask`, and a `ProxyCommand` that waits for a browser login. Such a wait holds the command until its context ends: a remote refresh after 30 seconds, a preparation after 30 minutes, a hydration batch fetch after 2 minutes, and the `fetch` command until Ctrl-C.

When the Git index matches the published snapshot and the overlay is empty, startup marks snapshot entries valid for Git's filesystem monitor before the mount serves the repo: before a prepared repo mounts, and before an async repo's gate opens. This lets the first `git status` use the daemon's change list instead of reading every tracked blob, and each later `git status` receives only the paths the overlay changed since Git's previous query, plus each file whose writes are not yet flushed. Paths with no `text` attribute, or under `text=auto`, `-text` or `binary`, are marked, unless `eol=lf` applies without `text=auto`. Paths under `text`, `text=input` or `eol=lf` without `text=auto` stay unmarked, because Git's check-in turns CRLF into LF and a blob that holds CRLF then differs from its file; Git compares those paths itself. `-text eol=lf` stays unmarked too, because `git check-attr` prints it like the literal `text=unset`, which Git converts under `eol=lf`. A `git status` that runs while the worktree is unmounted sees the empty mountpoint, so Git's next query gets a rescan. When startup marks the baseline again, its own query takes that rescan, and the first `git status` after the mount receives only the overlay's changes. ArtifactFS skips this baseline when the index or a `.gitattributes` blob is uncertain, when `core.attributesFile` is configured or `core.autocrlf` is set to anything but false, when an `eol=crlf`, `crlf`, `filter`, `ident` or `working-tree-encoding` attribute applies, or when staged work or overlay changes exist; Git then performs its normal status scan.

Use `--hydration-concurrency` to set each repo's number of hydration workers (default 4). A repo's workers share a pool of up to that many persistent `git cat-file --batch` processes that copy fetched blobs into the cache, so higher values trade memory for faster bulk hydration:

```bash
./artifact-fs daemon --hydration-concurrency 8
```

### Evicting the blob cache

The blob cache keeps every hydrated blob until you evict it. Nothing evicts automatically. `evict-cache` removes the least recently accessed blobs, by file access time, until the cache holds at most `--max-bytes` bytes. The flag is required and has no default; `--max-bytes 0` empties the cache:

```bash
./artifact-fs evict-cache --name workers-sdk --max-bytes 1073741824
# evicted_blobs=812 evicted_bytes=402653184 hydrated_blobs=2210 hydrated_bytes=1073610752
```

It is safe while the daemon runs. A file already open keeps reading its blob, and the next open of an evicted file hydrates it again. A blob whose access time changes after `evict-cache` lists the cache stays. Repos that share a `--blob-cache-dir` share one budget, and evicting through any of them evicts the shared directory.

Eviction follows the access times the filesystem records, so its order depends on the mount:

- `noatime`: reads never update the access time, so eviction removes the oldest-hydrated blobs first.
- `relatime`, the Linux default: after the first read, a read updates the access time only when it is more than a day old, so eviction order is accurate to about a day.
- APFS on macOS: a read updates the access time only while it is not newer than the modification time, so only the first read after hydration counts, and eviction removes the blobs first read longest ago.
- The kernel can serve repeated reads of a mounted file from its page cache without reading the blob file, and those reads leave its access time unchanged.

The cache shards blobs by the first two hex characters of their object ID, `<blob-cache-dir>/<2 hex>/<oid>`. When a repo mounts, the daemon moves blobs of the earlier flat layout into their shards and recounts the cache. `evict-cache` does the same before it evicts, and `status` does it once for a cache that has no counters yet. While one of them recounts, hydration into the same cache waits, so mounting a repo stalls hydration for every repo that shares its `--blob-cache-dir` for one walk of the directory, about half a second per 100,000 blobs on an APFS laptop. Hydration and eviction update the counters `status` reports as they add and remove blobs, under a lock on the cache's `stats` file, so `status` walks the cache only when it has no counters yet. The recount corrects counters that a crash between a blob change and its counter update, or a change made by hand, left wrong.

## Logging

ArtifactFS emits newline-delimited JSON to stderr. Preparation logs include the mode, source, attempt, phase, state, duration, and deadline. Clone and fetch operations retry known transient transport failures up to three total attempts. Preparation has a 30-minute timeout. Caller cancellation stops the active Git command and retry backoff. The command gets `SIGTERM`, so Git removes the lock files it created, such as `index.lock`, and `SIGKILL` 250 ms later if it still runs. A branch update that fails or is canceled after Git moved the index restores the index, then the branch, within 20 seconds in all, unless HEAD already reached the fetched commit, for example because Git finished the update before the cancel stopped it. Synchronous preparation records a redacted `prepare canceled` status, while daemon shutdown leaves async preparation queued for restart.

Existing-clone preparation reports remote configuration, fetch, and branch update as separate `configure_remote`, `fetch`, and `update_branch` phases.

Each error the FUSE library reports becomes a `fuse error` record:

- `ERROR`: a bug the library answers with `EIO`. That is a panic it recovers from an op, logged with its stack; an op that exits through `runtime.Goexit`; or a reply whose byte count falls outside its buffer.
- `WARN`: a reply the kernel refuses, other than with `ENOENT`. On FSKit, a read whose reply is shorter than the size last reported for the file, which the library answers with `EIO`. On Linux, a mount whose `FUSE_POLL` setup failed.
- `DEBUG`: every other failed op, including a request the library cannot decode, because expected errnos such as `EEXIST` fail ops too.

The daemon logs at `INFO`, so it drops the `DEBUG` records. The library reports no `ENOSYS` or `EINTR` from any op, no `ENOENT` from a lookup, no `ENOTEMPTY` from `rmdir` or `rename`, and no `ENODATA` or `ERANGE` from `GetXattr` or `ListXattr`.

A repo's FUSE connection can end while its mount stays in place: after an error fatal to the connection, such as a failed read from the kernel, or after the connection is aborted. The daemon then logs `fuse connection failed, unmounting`, records the connection's error as the repo's `mount_error` (for an abort, which carries no error, `fuse connection ended while the mount point still holds the mount`), unmounts the repo, and mounts it again. While that unmount fails and the mount table still lists the mount point, or the table, the path or the mount point cannot be read, each mount retry tries the unmount again instead of mounting. The daemon looks the mount point up in the kernel's mount table by its absolute path, with symlinks in the parent and a symlinked final component resolved. On macOS the lookup ignores case.

A daemon that stops without unmounting, for example after a crash, leaves its mounts behind. A daemon that stops cleanly records each repo it unmounted as stopped. The next daemon on the same state root reads from the stopped daemon's last status records which repos it still ran, and which mounts an earlier daemon left that it still claimed. Before it prepares or mounts such a repo, it unmounts the mount the table lists at the repo's mount point when that mount is one a daemon made for the repo, and logs `unmounting a mount an earlier daemon left`:

- On Linux, a mount of type `fuse.artifact-fs` whose source is `artifact-fs:<repo>`.
- On macOS, any mount whose type starts with `macfuse`. FSKit lists every macFUSE mount with the type `macfuse` and a source `macfuse://<UUID>` that names no repo, so a macFUSE mount another tool made at the mount point after a crash is unmounted too. The macFUSE kernel extension's source is closed, and no source code states the type it lists. macFUSE's [Mount Options](https://github.com/macfuse/macfuse/wiki/Mount-Options/5f8969079d1fe965a513685be6a28fe4cf49338e) page shows the type `macfuse`, and `macfuse_<name>` under the `fstypename` option, which artifact-fs never sets. The open-source [osxfuse extension](https://github.com/osxfuse/kext/blob/534773bd3135f04b7f6fb801faa2f8991daf4a6b/osxfuse/fuse_vfsops.c#L155) builds both names the same way, from its own name `osxfuse`.

While that unmount fails, the daemon does not mount the repo and retries with the mount backoff. The daemon keeps its claim on the leftover until it unmounts it or finds no mount there that it would unmount. Each status record carries the claim, so a daemon that stops first leaves the claim to the next one, and `status` still shows a stopped repo `unmounted`. A repo that `unmount` disabled releases the claim too: the daemon unmounts the leftover then, and while that unmount fails, `status` shows it as an `unmount` failure that the daemon retries with the mount backoff. A running daemon ends a removed repo's claim at its next registry sync, so a repo added again under that name after the sync claims nothing. A repo removed and added again while no daemon runs keeps the claim. It leaves every other mount alone, such as an `sshfs` mount on Linux, or any mount at the mount point of a repo the stopped daemon did not run or had stopped, including one of a daemon on another state root. On FSKit this unmount falls back to `diskutil unmount`, as described above.

A mount that does not finish within 30 seconds ends in the background. When the fuse library cannot unmount it once it completes, it stays at the mount point, and the daemon unmounts the mount point before it mounts the repo again.

Stopping a repo, for `unmount`, `remove-repo` or a daemon shutdown, unmounts it first and then cancels the FUSE operations still in flight, which return `EINTR`.

Successful `add-repo`:

```json
{"time":"2026-07-16T12:00:00Z","level":"INFO","msg":"repo preparation started","repo":"workers-sdk","mode":"sync","attempt":1,"source":"fresh_clone","phase":"validate","state":"started","duration_ms":0,"branch":"refs/heads/main","fetch_ref":"refs/heads/main","deadline_set":true,"timeout_ms":1799999}
{"time":"2026-07-16T12:00:04Z","level":"INFO","msg":"repo preparation completed","repo":"workers-sdk","mode":"sync","attempt":1,"source":"fresh_clone","phase":"complete","state":"completed","duration_ms":4217,"deadline_set":true,"timeout_ms":1799999,"head_oid":"d4c61587...","head_ref":"main","snapshot_generation":1}
```

Transient network failure followed by recovery:

```json
{"time":"2026-07-16T12:01:00Z","level":"WARN","msg":"git operation attempt failed","operation":"clone","repo":"workers-sdk","attempt":1,"max_attempts":3,"retryable":true,"duration_ms":842,"timed_out":false,"canceled":false,"error":"HTTP 503: unexpected disconnect"}
{"time":"2026-07-16T12:01:00Z","level":"INFO","msg":"retrying transient git operation failure","operation":"clone","repo":"workers-sdk","attempt":1,"next_attempt":2,"backoff_ms":214}
{"time":"2026-07-16T12:01:02Z","level":"INFO","msg":"git operation recovered","operation":"clone","repo":"workers-sdk","attempts":2,"duration_ms":2087}
```

Preparation timeout:

```json
{"time":"2026-07-16T12:30:00Z","level":"ERROR","msg":"repo preparation failed","repo":"workers-sdk","mode":"sync","attempt":1,"source":"fresh_clone","phase":"clone","state":"failed","duration_ms":1800000,"deadline_set":true,"timeout_ms":1799999,"timed_out":true,"canceled":false,"error":"git clone failed after 1 attempt; caller context: context deadline exceeded"}
```

Diagnostics are bounded and redact credentials and complete remote references. To follow preparation and network activity:

```bash
./artifact-fs daemon 2>/tmp/daemon.log &
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

The running daemon mounts a placeholder at its next registry poll, within 3 seconds. Operations inside that repo mount, such as `ls`, `less`, or `git -C /tmp/workers-sdk status`, wait until the clone/fetch and snapshot publish have completed. If preparation fails, those operations return an I/O error until preparation is retried:

```bash
./artifact-fs status --name workers-sdk
./artifact-fs prepare --name workers-sdk
```

`prepare` never unmounts the repo. The daemon prepares an async repo again behind its gate, so operations wait as they did during the first preparation. For a repo `add-repo` prepared without `--async`, `prepare` fetches and prepares in the CLI while the daemon keeps serving the repo, and `status` reports `preparing` meanwhile. Once the fetch moves HEAD, the watcher moves the live mount to that commit. When that `prepare` fails, the daemon keeps serving the repo, and a restarted daemon serves the git dir's HEAD. A repo whose `--remote` carried inline credentials stores no remote URL, so `prepare` fetches nothing for it and republishes the git dir's HEAD. `status` shows the `prepare_error` with the commit the daemon serves. An async repo that is ready and has no `--require-commit` has nothing to prepare, and `prepare` says so. `set-refresh` never prepares a repo again.

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

`--ref` and `--fetch-ref` take a full ref name, a branch name, or `origin/<branch>`, and `add-repo` stores each as a full ref name. `origin/X` and `refs/remotes/origin/X` name branch `X`, so a branch literally named `origin/X` needs `--ref refs/heads/origin/X`. `--fetch-ref` defaults to `--ref`. Preparation fetches the fetch ref and serves it, also right after a fresh clone of `--ref`. With `--require-commit`, preparation fetches and verifies `--ref`, and `add-repo` rejects `--fetch-ref`.

With `--depth`, `prepare` transfers at most that many commits of a fetch ref whose branch the git dir does not hold yet, or of a fetch ref that is not a branch, so it makes a `--prepared-gitdir` git dir shallow in those cases. A fetch onto history the git dir holds transfers only the new commits. The remote refresh and the `fetch` command pass `--depth` only while the git dir lacks both the fetch ref's tracking ref and its branch, which never happens after a successful preparation. A `--require-commit` repo is the exception: each refresh of it passes `--depth`.

The remote refresh and the `fetch` command fetch what preparation fetches, into the same ref: the fetch ref, or the `--ref` of a repo with `--require-commit`. They do not update the git dir's other remote-tracking branches, even when its refspec names them, as earlier builds did. To update those, run `git fetch` in the git dir.

A prepared git dir whose remote-tracking branch an earlier build's refresh made shallow refuses its next preparation with `refusing non-fast-forward update`. Deepen it once in that git dir, with `git fetch --deepen=<n>` or `git fetch --unshallow`.

## Adopting linked Git worktrees

`--adopt-gitdir` registers an initialized linked worktree without cloning, fetching,
changing refs or upstream tracking, or resetting its Git index. The GitDir must
already contain a valid HEAD and index, and its registered worktree path must
match the mountpoint. Primary GitDirs, relocation and reftable ref storage are
rejected before config writes. The daemon mounts the worktree only on an empty
mountpoint, which `add-repo` does not check: adoption
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
ARTIFACT_FS_ROOT=/path/to/topic-state artifact-fs daemon
```

Adoption disables remote refresh and rejects acquisition options (`--remote`,
`--ref`, `--branch`, `--fetch-ref`, `--depth`, `--require-commit`, `--async`, and
`--prepared-gitdir`). An explicit `--refresh never` is accepted. Local HEAD
watching remains active, including shared loose and packed branch refs, so
commits and branch changes reconcile normally. Run ordinary Git fetch/push
commands when needed; the worktree's original remote configuration is retained.
If `origin` or a promisor remote embeds credentials, adoption logs a warning that names no
remote and proceeds: fetches of missing blobs and trees, Git's lazy fetches and the
hydrator's batch fetches, use the promisor remotes, usually `origin`.

ArtifactFS enables `extensions.worktreeConfig`, migrating shared `core.worktree`
and `core.bare=true` settings into the primary worktree's config when necessary.
Existing primary overrides are retained. It pins the adopted worktree's path,
filesystem semantics and FSMonitor hook in that worktree's config; native
siblings retain their settings.

Stop the owning daemon and wait for process exit and actual unmount before
restoring the saved gitfile, unlocking metadata or removing the worktree.
`remove-repo` updates the registry and Git config and returns. The running daemon
unmounts the repo at its next registry poll, so the return does not mean the live mount has stopped.
It unsets what adoption pinned in the worktree's config for the mount
(`core.fsmonitor`, `fsmonitor.allowRemote`, `core.filemode`, `core.ignorecase`
and `core.symlinks`), so the worktree inherits its repository's settings again.
A value the worktree's own config held before adoption does not come back. It
keeps the worktree's path pins and `extensions.worktreeConfig`, which sibling
worktrees may rely on. Preserve a recovery ref before deleting a
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

`--prepared-gitdir` is not supported with `--require-commit` because verified acquisition replaces the repo's Git directory with one it built and verified.

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
                         │  │  (disk)  │                 │ snap + ovl   │  │
                         │  └──────────┘                 │  merged view │  │
                         │       ▲                       └──────┬───────┘  │
                         │       │                              │          │
                         │  ┌────┴─────┐   hydrate,       ┌─────┴────────┐ │
                         │  │ Hydrator │◄─────────────────│    Engine    │ │
                         │  │          │   prefetch       │              │ │
                         │  │ priority │                  │ read / write │ │
                         │  │ queue    │                  │ create / rm  │ │
                         │  └──────────┘                  │              │ │
                         │                                │ copy-on-     │ │
                         │  ┌──────────┐   write ops      │ write        │ │
                         │  │ Overlay  │◄─────────────────│              │ │
                         │  │ (SQLite  │                  └─────┬────────┘ │
                         │  │  + upper │                        │          │
                         │  │  dir)    │                 ┌──────┴───────┐  │
                         │  └──────────┘                 │  FUSE Layer  │  │
                         │                               │  (macFUSE /  │  │
                         │                               │   /dev/fuse) │  │
                         │                               └──────┬───────┘  │
                         │  ┌──────────┐                 ┌──────┴───────┐  │
                         │  │ Watcher  │  polls gitdir   │ Mount point  │  │
                         │  │ (500ms)  │  HEAD, then     │ /tmp/myrepo  │  │
                         │  └──────────┘  re-indexes and └──────────────┘  │
                         │                reconciles                       │
                         └─────────────────────────────────────────────────┘
```

### Data flow

1. **Clone/fetch** -- `add-repo` runs a blobless clone or verified source acquisition. `--depth` controls transferred history independently. Verified acquisition fetches the canonical source ref into a private candidate ref and checks `--require-commit` before it installs the Git directory. When the remote advertises partial-clone filtering, only commits, trees, and refs are fetched initially; otherwise Git falls back to downloading blobs eagerly.

2. **Index** -- `git ls-tree -r -t -z <selected-commit>` enumerates every path in the selected tree. Sizes are resolved locally via `git cat-file --batch-check` with `GIT_NO_LAZY_FETCH=1` to avoid network round-trips. The result is bulk-inserted into a SQLite `base_nodes` table as a new generation.

3. **Mount** -- The FUSE layer exposes the tree immediately. A synthesized `.git` gitfile points at the real gitdir so git commands work inside the mount.

4. **Read** -- The Resolver merges the snapshot (base tree) with the overlay (local writes). For base files, reads block until the Hydrator fetches the blob and streams it to the blob cache through a persistent `git cat-file --batch` process. Two or more missing blobs queued together are fetched with one `git fetch --stdin`, up to 256 per fetch, and reads and prefetches never share a fetch. The first open for reading or read of a base file in a directory queues up to 256 of that directory's files, the opened one included, the same way.

5. **Write** -- The Engine promotes base files to the overlay via copy-on-write (hydrate, then copy to the `upper/` directory). Subsequent reads come from the overlay. A delete is recorded as a delete entry in the overlay database; deleting a file copies it first only while a handle on it is open. A write reaches the overlay file at once, and `stat` shows its size and mtime at once. For a file whose overlay entry is already recorded, such as a base file promoted by copy-on-write, the overlay database records the first write after a flush at once. A file created through the mount is recorded at its create, and its writes are pending from then on. The database records the size and mtime of pending writes when a handle that wrote the file is flushed or closed, when the file is `fsync`ed, or when any overlay change other than a write reaches the overlay. An `fsync` returns only after the file and the database reach stable storage. Between syncs, an OS crash can lose the database's latest changes; a daemon crash loses none of them. No FUSE operation holds a lock while a blob hydrates, so a slow fetch stalls neither a HEAD switch nor other operations. A failed operation returns the errno of its cause, such as `ENOSPC`, and an interrupted one returns `EINTR`.

6. **Background** -- For ordinary workspaces, a watcher polls HEAD and the ref it names every 500ms and republishes HEAD changes. A HEAD change compares the overlay with the new generation while reads and writes continue; they wait only while the result is applied. Repositories with `--require-commit` do not run this watcher, so their verified base commit remains fixed. Remote polling is controlled separately by `--refresh`.

### Kernel caches after a HEAD change

A HEAD change switches the mount to a new snapshot generation. The kernel may
still cache what the mount reported before: names, missing names, attributes
and file pages. The mount reports entries, negative entries and attributes with
a 1 s TTL. The synthesized `.git` file never changes, and the mount reports it
with a 1 minute TTL. Every base file and directory reports the time of the HEAD
commit the mount serves as its mtime.

On every backend, an inode reads the generation of the attributes the kernel
last received for it, so its reads match the size the kernel holds. The daemon
keeps a generation readable for 2 s after the mount stops serving it, and prunes
it at a later HEAD change or preparation.

| Backend | After a HEAD change |
|---------|---------------------|
| Linux | The mount invalidates three things at once: the attributes and cached pages of each inode the kernel holds whose permission bits or blob changed, or that the kernel last saw from the overlay and that now resolves to a base blob; the entry of each name that vanished, and both the entry and the inode of each name that changed type; and each negative entry whose name now exists. The mount caches a missing name as a negative entry for 1 s. A name made through the mount replaces its negative entry, and a write that recreates a vanished file invalidates it. The mount negotiates `FUSE_AUTO_INVAL_DATA` without writeback caching, so the kernel drops a file's cached pages when its size or mtime changes. The kernel keeps a base file's pages across opens only while its opens serve the same blob. For up to 1 s, a reader can still see attributes the mount does not invalidate, such as the old mtime of a file whose blob did not change, or of a directory. A lookup that resolves a file before the switch and whose reply the mount records after its invalidation pass read the inode table, for an inode the kernel does not hold, makes the kernel create the inode with the old attributes. That leaves old names and sizes for up to 1 s, and old pages within one open: the kernel drops a file's pages only when its size or mtime changes, or at the next open that serves another blob. A lookup or getattr that reports an inode's new identity between the switch and the invalidations does not stop that inode's invalidation, and the kernel discards an attribute reply older than an invalidation. |
| macFUSE kernel extension | The mount sends the inode and entry invalidations it sends on Linux. The extension mounts with `novncache`, so it caches no names and the mount caches no negative entries. It drops a file's cached attributes and pages on each `InvalidateInode`, and a getattr that reports a size other than the one it holds drops all the file's cached pages. A lookup leaves a held file's size alone. An open does not drop the pages, and `KeepPageCache` has no effect. So the mount still invalidates an inode whose new identity a lookup or getattr reported between the switch and the invalidations. After each `InvalidateInode` of a regular file, the extension fetches the file's attributes again, under the lock that a getattr holds until it has stored its reply. A vanished name gets only its entry invalidation, because its file's content did not change: its open files keep reading what they read. Two races remain. A lookup can resolve a file before the switch, for an inode the extension does not hold, and the mount can record its reply after its invalidation pass read the inode table. That file's old pages can last until the next HEAD change. An `mmap` page-in between an `InvalidateInode` and the reply to the extension's own attribute fetch can page in the old blob, because page-ins take only the extension's global lock, not the file's. A fetch that reports the same size keeps those pages, and they can last until the next invalidation of that inode or a getattr that reports a different size. A `read(2)` waits for that fetch, so it is not affected. For up to 1 s, a reader can still see the old mtime of a file whose blob did not change, and of a directory. macFUSE 5's extension is closed source, so this row follows the open-source [osxfuse extension](https://github.com/osxfuse/kext/tree/534773bd3135f04b7f6fb801faa2f8991daf4a6b). |
| FSKit | FSKit takes no invalidations, so the mount sends none and caches no negative entries. FSKit keeps what it cached until it asks again, which the mount cannot hasten. FSKit keeps its own attribute cache, and artifact-fs knows no bound for it. Until then, a reader can see the old commit's names, sizes and modes, and an inode reads the old generation while the daemon keeps it. After that, the inode reads the current content. The fuse library answers EIO to a read inside the size it last reported that the current content does not fill. A read that FSKit sends before a reply reporting a smaller size reaches it can still get a short reply without EIO. |

### Inode numbers

A path keeps its inode number while it exists, also after the kernel forgets
the inode, so Git's stat data stays valid. A rename moves the numbers of the
whole subtree. A path that stops existing retires its number: an unlink, rmdir
or rename over it through the mount, or a HEAD change that removes it. A file,
directory or symlink made there later gets a new number, even while the kernel
still holds the old inode.

A file or directory that the kernel still holds when a HEAD change removes its
path gets the path back if a later HEAD change restores the path unchanged: a
directory again, or a file or symlink with the same type, permission bits and
blob. An open file and a shell's working directory therefore keep working across
a HEAD change made outside the mount and later undone, such as with `git
update-ref`, `git reset --soft`, or a ref moved from another worktree. A `git
checkout` inside the mount instead unlinks and recreates through the mount each
path it changes, which makes the file or directory held there stale. A
successful write through such an open file recreates its path under the file's
own number, while no other file holds the path. A rename of a parent directory
moves that path too.

The held file or directory becomes stale when its path comes back changed, when
another file takes the path through a create, a lookup or a listing, or when an
unlink, rmdir or rename over it through the mount removes it. `open` on a stale
file returns `ESTALE`, and its handles never reach the path again. A handle
reads the content it already holds, and returns `ESTALE` when it holds none.
`getattr` on a stale file, or on a held file whose path is away, answers with
the file's mode and the size and times of that content, and returns `ESTALE`
when its handles hold none. Linux fetches an open file's attributes before a
read once they expire, so a read through such a file keeps working. An unlink of
a file's path or a rename over it through the mount gives the file's open
handles one copy of their own, which they share. A write or `setattr` through
the stale file, such as `ftruncate`, `fchmod` or `futimens`, then changes that
copy, and `getattr` reports the change. The copy keeps a time that `setattr`
sets to the microsecond. Without such a copy, a write or `setattr` returns
`ESTALE`. A lookup below a stale directory misses. Numbers last for one mount: a
remount or a daemon restart numbers paths again.

The mount keeps one mapping for each existing path that the kernel has looked
up or that an op made through the mount. After each HEAD change, it finds the
paths that vanished: it reads the overlay once and lists the snapshot children
of each directory that holds a mapped path. When a rename overlapped that check,
it checks the vanished paths again while no rename can run. On Linux and the
macFUSE kernel extension, a mapped path whose inode the kernel holds is checked
on its own instead: resolving it decides whether its number retires, and
comparing its type, permission bits and blob with what the kernel last received
decides which invalidations it gets.

### Subsystems

| Package | Role |
|---------|------|
| `daemon` | Repo lifecycle. `Controller` runs every CLI command except `daemon` and the `gc`, `doctor` and `prefetch` stubs. `Service` holds the daemon lock, syncs the registry, prepares, mounts and unmounts each repo, reconciles HEAD, runs the refresh loop, and writes `repo_runtime` |
| `fusefs` | FUSE adapter (inode management, op dispatch), Resolver (merged view), Engine (the adapter's only API: one method per operation) |
| `gitstore` | Git CLI wrapper: clone, fetch, ls-tree, batch pool for `cat-file --batch` |
| `snapshot` | SQLite store for `base_nodes` keyed by `(generation, path)` |
| `overlay` | SQLite metadata + `upper/` directory for local writes, delete entries, reconciliation |
| `blobcache` | Blob cache layout (sharded by object ID), its counters, and LRU eviction for `evict-cache` |
| `hydrator` | Priority queue with deduped waiters; workers fetch queued blobs in batches. With more than one worker, a reserved worker takes only reads and copies of local blobs and blocks on `reservedReady`; the others block on `workReady` |
| `watcher` | Polls the contents of `HEAD` and of the ref it names, loose or packed, at 500ms intervals |
| `registry` | SQLite store of repo configs, prepare and mount requests, acquisition receipts, and the `repo_runtime` records the daemon writes and `status` reads |
| `model` | Shared types and canonical interfaces (`Registry`, `GitStore`, `SnapshotStore`, `OverlayStore`, `Hydrator`) |

## Supported git operations

Work in progress. The table reflects operations exercised by the FUSE E2E suite; some were also tested against [cloudflare/workers-sdk](https://github.com/cloudflare/workers-sdk) mounted via macFUSE.

### Filesystem operations

| Operation | Status | Notes |
|-----------|--------|-------|
| `ls` (root and subdirectories) | Supported | Includes synthesized `.git` gitfile; listing hydrates nothing |
| `cat` / read file | Supported | Triggers on-demand hydration for unhydrated blobs |
| `stat` (file size, mode) | Supported | Sizes resolved via `git cat-file --batch-check` when the blob is local; otherwise the first `stat` hydrates the file |
| `mkdir` | Supported | Persisted in writable overlay |
| Create new file | Supported | Persisted in writable overlay |
| Write / append to file | Supported | Copy-on-write for tracked files |
| Rename file | Supported | Works for both overlay and tracked (snapshot-only) files |
| Rename directory | Supported | Moves tracked and overlay-created directory trees |
| Delete file (`rm`) | Supported | Delete entry recorded in overlay; fetches nothing unless the file is open |
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
| `git remote -v` | Supported | `add-repo` stores the remote URL without inline credentials; an adopted worktree keeps its own remotes |
| `git stash` | Supported | Includes push/pop with untracked files |
| `git status` | Supported | ~7s on 5800-entry repo |
| `git diff` | Supported | Shows correct unified diff for modified files |
| `git add` | Supported | Stages modified files |
| `git reset --hard` | Supported | Restores content, modes, and staged creates |
| `git clean -fd` | Supported | Removes untracked files and directory trees |
| `git commit` | Supported | Reconciles content, executable-bit, symlink, rename, and delete changes |
| `git checkout` | Supported | Re-indexes tree and reconciles overlay; Git updates its own index |
| `git merge --no-ff` | Supported | Publishes merge commits and reconciles the merged worktree |
| `git rebase` | Supported | Reconciles rewritten commits and branch state |
| `git fetch` | Supported | Background refresh loop fetches the fetch ref, or a `--require-commit` repo's `--ref`, periodically. Run `git fetch` in the git dir for other branches |
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
AFS_RUN_E2E_TESTS=1 go test -v -run 'TestE2E|TestFUSEMountSmoke' -count=1 -timeout 20m .

# Run against a specific remote repo
AFS_RUN_E2E_TESTS=1 \
  AFS_E2E_REPO=https://github.com/cloudflare/workers-sdk.git \
  go test -v -run 'TestE2E|TestFUSEMountSmoke' -count=1 -timeout 20m .
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

See [AGENTS.md](AGENTS.md) for build commands, architecture details, and conventions. Before submitting changes, run the CI checks in the order that AGENTS.md lists them under "Validation".

Enable the pre-commit checks with `git config core.hooksPath .githooks`. For staged Go changes they run `gofmt`, `go vet`, `modernize`, a CLI build, and the changed packages' unit tests. The hook skips the tests when the machine is under load; set `AFS_PRECOMMIT_TESTS` to `always` or `never` to override, or tune `AFS_PRECOMMIT_LOAD_PER_CPU` (default `1`).

## Credits

The ArtifactFS FUSE driver takes inspiration from and draws from implementation details in:

* [TigrisFS](https://github.com/tigrisdata/tigrisfs/)
* [gitfs](https://github.com/presslabs/gitfs)
* [SlothFS](https://gerrit.googlesource.com/gitfs/)

## License

(c) Cloudflare, 2026. Apache-2.0 licensed.
