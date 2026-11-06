# AGENTS.md

Repository-specific guidance for `artifact-fs`.

## Code review rules

- Flag credentials in Git CLI arguments or unredacted logs. Pass credentials through the environment-based helper in `internal/gitstore/credentials.go`. Redact a log field with `auth.RedactLogString`, which replaces each remote it detects with `REDACTED_REMOTE`. Redact an error message with `auth.RedactString`, which replaces the credentials it detects with `REDACTED`. Detection is heuristic, so never log a value known to hold credentials.
- Keep adoption's inline-credential check in `WarnAdoptedOriginCredentials` a warning that names no remote, not a rejection. Adoption stores no URL and never clones, fetches or refreshes. Only Git's lazy fetches of missing blobs and trees use the user's existing origin. Flag any other path that persists inline credentials or lets the daemon fetch with them.
- Flag blob bytes converted to `string`. Keep `BlobToCache` binary-safe by streaming `git cat-file --batch` output to disk.
- Keep `GIT_NO_LAZY_FETCH=1` on `git cat-file --batch-check` in `batchResolveSizes`. Removing it turns size resolution in blobless clones into network round-trips.
- Run a Git command that can lazy-fetch missing objects with `lazyFetchEnv()` and `configureCommandSession`, never `nonInteractiveGitEnv()`. `nonInteractiveGitEnv()` sets `GIT_SSH_COMMAND`, which overrides the repo's `core.sshCommand`, and an adopted worktree keeps that setting.
- Add a blob cache file only with `blobcache.Stage` and `blobcache.Install`, and remove one only with `blobcache.Evict`. Each updates the counters `status` reads under the cache's `stats` flock. A direct write or unlink leaves the counters wrong until the next mount or `evict-cache` recounts them.
- Normalize paths only with `model.CleanPath()`. Do not add local wrappers.
- Use the canonical `model.*` interfaces for snapshot, overlay, hydrator, and gitstore access. Do not add subset interfaces around them.

## Architecture

- `cmd/artifact-fs` is the only binary entrypoint.
- `internal/cli` runs the `daemon` command on `daemon.Service`, and every other command on `daemon.Controller`. Only `daemon.Service` mounts.
- `internal/daemon` owns repo lifecycle: registry sync, snapshot publish, overlay reconcile, FUSE mount, watcher, refresh loop.
- `internal/fusefs` is the merged view and writable filesystem layer.
- `internal/gitstore` is the performance-sensitive git wrapper; most easy-to-break invariants live there.
- `internal/snapshot` and `internal/overlay` are persistent SQLite-backed stores.

## Runtime contracts

- `ARTIFACT_FS_ROOT` is the state root. `artifact-fs daemon --root` is the mount root. They are different things.
- `add-repo` is one-shot by default: register repo, clone blobless, build the initial snapshot, then exit. It does not mount FUSE or start background goroutines.
- `add-repo --async` only registers prepare state. The daemon mounts a gated placeholder, prepares clone/fetch and snapshot in the background, then opens the gate and starts watcher/refresh.
- `daemon` is long-running: it mounts registered repos and starts watcher, refresh, and hydrator workers.
- The daemon holds a flock on `<root>/config/daemon.lock`. A second daemon on one state root fails. CLI commands probe that lock to learn whether a daemon runs.
- The registry carries every request from the CLI to the daemon. The control socket only wakes the daemon.
- Only the daemon writes `repo_runtime`. It writes the record at every transition and every retried failure. `status` reads that record and the stores, never daemon memory.
- `unmount` and `remount` write an intent to `enabled` and `mount_request`, then nudge `<root>/config/daemon.sock`. They wait until `repo_runtime` echoes the request.
- `git.CloneBlobless` already populates the git index with `read-tree HEAD`; be careful about extra index resets because they can discard staged state.

## Code conventions

- `Readdir()` stays thin; merged directory logic belongs in `ReaddirTyped()`.
- The watcher polls `HEAD` plus the current HEAD ref path. If you change it, preserve branch-switch and packed-ref behavior covered by `internal/watcher/watcher_test.go`.
- The overlay stores deletes as SQLite entries with `kind='delete'`; there is no on-disk whiteout file layer.
- SQLite is `modernc.org/sqlite` in WAL mode via `internal/meta`.
- Change a store's schema only by appending a migration to its `migrations` list. `meta.Migrate` records each applied migration in `PRAGMA user_version` and never reruns it. An edit to a shipped migration never reaches an existing database. With each new migration, add a fixture at its `user_version` to that store's `migrate_test.go`. That fixture fails the tests when a later edit changes a shipped migration.
- The FUSE adapter calls only `Engine` methods, one per operation. Do not reach into `engine.Overlay`, `engine.Hydrator`, or the `Resolver` from the adapter.
- Hold no lock while a blob hydrates. An `Engine` method that needs base content takes a `*Blobs` and returns a `notHydratedError` when a blob is missing. `Engine.Do` then hydrates with no lock held and reruns the method.
- Open every blob an `Engine` method copies before it changes anything. A directory rename is the one exception: it opens each blob as it copies it, so it may already have promoted some directories and copied some files, all with base content, and the rerun completes it.
- Take the FUSE locks in this order: `handleOps`, `DirHandle.mu`, the resolver's `transition`, `ArtifactFuse.mu`, `FileHandle.mu`.
- Release each FUSE lock with `defer`, or hold it only around code that cannot panic. The fuse library recovers a panic in an op and keeps serving, so a lock left held blocks every later op.
- Wrap an operation error's cause with `%w`, and let `toErrno` map it for the kernel. Do not return `syscall.EIO` for a failure that carries an errno.
- Switch a runtime's generation only through `Service.switchGeneration`. It sends the kernel the invalidations the switch needs through `MountedFS.InvalidateChanged`.
- Never wait on a kernel invalidation inside an op or while holding a lock an op takes. The kernel can hold a directory lock while it waits for that op's reply, and the invalidation waits for the same lock.
- Record every entry reply with `allocEntry` and every attribute reply with `setReported`. `InvalidateChanged` compares what the kernel received with the merged view, and `allocEntry` drops the name's negative entry record. The exception is `GetInodeAttributes` for the root and the synthesized `.git` file: `InvalidateChanged` skips both.
- Prune snapshot generations only through `Service.pruneGenerations`. An inode reads the generation of its last reported attributes, so a retired generation stays readable for `defaultGenerationRetention`.

## Writing tests

- Write a test to catch a regression, never to raise coverage. Do not add coverage targets.
- Make every regression test fail on the code before the fix. Revert the fix and watch the test fail.
- Drive behavior through the entry point real callers use, not an internal helper.
- Use real Git repositories in `t.TempDir()`. Fake the `git` binary only to inject failures a local repository cannot produce, such as HTTP errors.
- Assert the observable outcome: what `git status` reports, the resolved entry, or the persisted state. A nil error or a log line is not an outcome.
- Assert a result that only the code path under test produces. An unchanged index passes whichever check skips the bootstrap. `TestBootstrapCleanStatusIndexSkipsDeleteOnlyOverlay` therefore first bootstraps the same fixture without the overlay entry.
- Call `gittest.IsolateConfig` from `TestMain` in every package whose tests run git. Prove it with a `TestGitIgnoresMachineConfig` that calls `gittest.RequireConfigIsolated`. The machine's global and system git config must not change a result. The root package keeps the machine config when `AFS_E2E_REPO` is set, so a real remote can authenticate through the ambient credential helper.
- In package tests, wait for an event, not for a fixed duration. Poll for the marker the fake git writes, and fail with the operation's error if it returns first. Only `go test -timeout` bounds the wait. E2E tests keep bounded waits, because a timeout panic skips their unmount cleanup.
- Bound every fake process that a regression would leave running, for example with `sleep 60`. A timeout panic skips cleanup, so an unbounded fake outlives the test. Detect the regression from the order of the fake's markers, not from how long it ran.
- Expire a deadline under test with `gittest.ExpireOnFile` once the fake git reaches the step under test. A short timeout can expire in an earlier step on a loaded machine.
- Assert that something has not happened yet inside `synctest.Test`, after `synctest.Wait`. Drive ticker-based code there too. `synctest.Wait` does not treat a goroutine blocked on a `sync.Mutex` or a subprocess as idle, so those tests cannot use it.
- Never mount in a package test. Only the root-package e2e tests mount.
- Call an `Engine` method that takes a `*Blobs` through `Engine.Do`, as the FUSE adapter does. `internal/fusefs/ops_test.go` has `engineRename` and its siblings.
- Assert the errno the kernel receives with `kernelErrno`, not by comparing error values.
- Run a new e2e test as a `TestE2ESharedMount` subtest. Give it its own mount only when its subject is mounting, a daemon restart, or a fresh mount's state. macFUSE's FSKit extension can stop loading after a series of mounts in one session.
- Register a shared-mount test that leaves the mount unchanged with `runReadOnly`, and every other one with `run`. When e2e tests mount through FSKit on macOS, `run` skips the test, because the restore that undoes its changes moves HEAD. A `runReadOnly` test that changes the mount fails.
- Call `skipIfFSKitHEADMove` in a test that moves HEAD on its own mount. After a HEAD move, FSKit can pad a short read with bytes that are not file data ([macfuse#1231](https://github.com/macfuse/macfuse/issues/1231)).
- Before a test's first mount, call `skipIfOverMountBudget` with the number of mounts the test starts. A run that mounts through FSKit starts at most 8 mounts. `newMountedE2ERepo` checks one mount.

## Validation

- Start with the narrowest relevant test: `go test ./internal/<pkg>` or `go test -run TestName ./internal/<pkg>`.
- For non-trivial code changes, reproduce CI in order: `go build -o /dev/null ./cmd/artifact-fs`, `go vet ./...` with `GOOS=linux` and with `GOOS=darwin`, then `go test -race ./...`.
- Build only the CLI with `go build ./cmd/artifact-fs`.
- Enable the pre-commit checks with `git config core.hooksPath .githooks`.
- Benchmarks are opt-in: `AFS_RUN_BENCH=1 go test -run TestBenchRepos -v`.
- FUSE e2e tests are opt-in. Run them with the same command on macOS and Linux: `AFS_RUN_E2E_TESTS=1 go test -run TestE2E -count=1 -timeout 20m -v .`. Keep `-timeout 20m`: the default 10m can expire mid-suite, and a timeout panic skips every unmount cleanup.
- On Linux, e2e tests need `/dev/fuse` plus either root or `fusermount3` from the `fuse3` package. Unmount a leftover mount with `fusermount3 -u <path>`, or `fusermount3 -uz <path>` when it hangs.
- On macOS, e2e tests mount through FSKit. Set `ARTIFACT_FS_FUSE_BACKEND=kernel` only to test the macFUSE kernel extension; without a loaded kernel extension those tests skip. Unmount a leftover FSKit mount with `diskutil unmount <path>`. A deadlocked FSKit mount cannot be unmounted until reboot.
- E2E tests use a local bare repository by default. Set `AFS_E2E_REPO` only when intentionally testing a real remote.
- E2E benchmark coverage is separate: `AFS_RUN_E2E_BENCH=1 go test -run TestE2EBenchmarkRepos -v .`.
- Package tests live beside the code. Benchmarks and `e2e*_test.go` live in the root package.
