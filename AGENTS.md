# AGENTS.md

Repository-specific guidance for `artifact-fs`.

## Code review rules

- Flag credentials in Git CLI arguments or unredacted logs. Pass credentials through the environment-based helper in `internal/gitstore/credentials.go`. Redact a log field with `auth.RedactLogString`, which replaces each remote it detects with `REDACTED_REMOTE`. Redact an error message with `auth.RedactString`, which replaces the credentials it detects with `REDACTED`. Detection is heuristic, so never log a value known to hold credentials.
- In production code, flag a `model.RepoConfig` in the hydrator, the overlay or `internal/fusefs`. They take `model.RepoPaths`, so `RemoteURL`, which can carry credentials, reaches only the CLI, the registry, the daemon and gitstore.
- Keep adoption's inline-credential check in `WarnAdoptedRemoteCredentials` a warning that names no remote, not a rejection. Adoption stores no URL and never clones, fetches or refreshes. Only Git's lazy fetches and the hydrator's batch fetches use the user's existing remotes. Git's lazy fetch tries each promisor remote in turn, and the hydrator's batch fetch uses the first. The promisor remote is usually `origin`. Flag any other path that persists inline credentials or lets the daemon fetch with them.
- Flag blob bytes converted to `string`. Keep `BlobToCache` binary-safe by streaming `git cat-file --batch` output to disk.
- Keep `GIT_NO_LAZY_FETCH=1` on `git cat-file --batch-check` in `batchResolveSizes`. Removing it turns size resolution in blobless clones into network round-trips.
- Run every Git command that can contact a remote in its own session with the environment `lazyFetchEnv` returns. That covers a clone, `ls-remote`, a fetch, and any command that can lazily fetch missing objects. `runNetworkGit` and `runLazyFetchGitInput` do both. Outside that session, git runs in a background process group, and a terminal prompt stops it until the operation times out. Never set `GIT_SSH_COMMAND` on such a command, or `core.sshCommand` through `-c` or `GIT_CONFIG_*`: either replaces the SSH command that an adopted worktree, a prepared git dir or a conditional include sets. `lazyFetchEnv` sets `GIT_SSH`, which Git runs only when no SSH command outranks it, to the Store's wrapper that runs `ssh -o BatchMode=yes`. The Store removes the wrapper on `Close`.
- Stop a Git command only with `terminateCommandProcessGroup`, as `configureCancelableCommand` and `batchCatFile.terminate` do. It sends the command's process group `SIGTERM`, then `SIGKILL` after `gitCommandWaitDelay`. On `SIGTERM` Git removes the lock files it created, such as `index.lock` or `objects/maintenance.lock`. A Git killed with `SIGKILL` leaves them, and every later Git that takes the lock fails or, for maintenance, skips without a word. Never delete a Git lock file in code: one that a running Git holds looks the same.
- Run every step of one rollback under one `Store.rollbackContext`. A rollback outlives its caller's cancellation. `gitstore.RollbackBound` bounds it, and `TestDrainOutlastsTheRollbackOfACanceledPreparation` keeps that bound below the daemon's `drainTimeout`. A git command that reports failure can still have landed. So a rollback leaves the index when HEAD already resolves to the new commit, and undoes a forward read-tree that moved the index.
- Add a blob cache file only with `blobcache.Stage` and `blobcache.Install`, and remove one only with `blobcache.Evict`. `Install` and `Evict` update the counters `status` reads under the cache's `stats` flock. A direct write or unlink leaves the counters wrong until the next mount or `evict-cache` recounts them.
- Normalize paths only with `model.CleanPath()`. Do not add local wrappers.
- Use the canonical `model.*` interfaces for registry, git store, snapshot, overlay, and hydrator access. Do not add subset interfaces around them.
- In production code, assert each implementation of a `model.*` interface with `var _ model.X = (*T)(nil)`. Test fakes need no assertion.
- Add a method to a `model.*` interface only when production code calls the method through the interface.
- In production code, only `internal/daemon` holds a concrete snapshot, overlay or hydrator type, because it opens and closes them. Tests may build concrete stores.

## Architecture

- `cmd/artifact-fs` is the only binary entrypoint.
- `internal/cli` runs the `daemon` command on `daemon.Service`, and every other command on `daemon.Controller`, except the `gc`, `doctor` and `prefetch` stubs. Only `daemon.Service` mounts.
- `internal/daemon` owns repo lifecycle: registry sync, snapshot publish, overlay reconcile, FUSE mount, watcher, refresh loop.
- `internal/fusefs` is the merged view and writable filesystem layer.
- `internal/gitstore` is the performance-sensitive git wrapper; most easy-to-break invariants live there.
- `internal/snapshot` and `internal/overlay` are persistent SQLite-backed stores.

## Runtime contracts

- `ARTIFACT_FS_ROOT` is the state root. Each repo mounts at the path its registration stores: `add-repo --mount-root`, default `<state root>/mnt`, plus the repo name. `artifact-fs daemon --root` changes no registered repo's mount path.
- `add-repo` is one-shot by default: register repo, clone blobless, build the initial snapshot, then exit. It does not mount FUSE or start background goroutines.
- `add-repo --async` only registers prepare state. The daemon mounts a gated placeholder, prepares clone/fetch and snapshot in the background, then opens the gate and starts watcher/refresh.
- `config_version` identifies a registered config, and only a new registration changes it. Use it as the only token that a config is still current.
- `prepare_request` identifies the latest prepare request, from `add-repo` or `prepare`. Write a preparation's outcome only while its `config_version` and its `prepare_request` are both current. Write the acquisition receipt while its `config_version` is current; the receipt belongs to the config, not to the request.
- A prepare request never unmounts a repo. The daemon prepares an async repo again behind its runtime's gate. The CLI prepares a sync repo while the daemon keeps serving it.
- Refresh settings are not part of a repo's prepare identity. `set-refresh` changes neither token.
- `daemon` is long-running: it mounts registered repos and starts watcher, refresh, and hydrator workers.
- The daemon holds a flock on `<root>/config/daemon.lock`. A second daemon on one state root fails. CLI commands probe that lock to learn whether a daemon runs.
- The registry carries every request from the CLI to the daemon. The control socket only wakes the daemon.
- Only the daemon writes `repo_runtime`. It writes the record at every transition and every retried failure. `status` reads that record and the stores, never daemon memory.
- `unmount` and `remount` write an intent to `enabled` and `mount_request`, then nudge `<root>/config/daemon.sock`. They wait until `repo_runtime` echoes the request.
- One sync loop serves every repo. Bound every per-repo step it runs, so one repo cannot stall the others. A mount waits at most `mountTimeout` through `fuse.MountContext`. Start a stop with `Service.stop`, which runs it off the loop; the loop never waits for a stop.
- Unmount a repo only through `startUnmount`. It runs one unmount per repo on its own goroutine, because `unmount(2)` can hang on a dead FSKit volume. A stop runs whole as the repo's unmount, from its unmount through its drain, so it unmounts without a second `startUnmount`. Wait only for an unmount you started, and at most `unmountTimeout` plus `unmountReturnSlack`. `Service.unmount` is the exception: `Close` waits through it for any stop of the repo, at most `stopWaitTimeout`.
- Build a runtime only with `startRuntime`. Mount it only with `attachMount`, which checks every mount precondition. A dead mount whose unmount failed blocks a new mount while the mount table lists its mount point.
- Mount with the runtime's op context. Only a stop cancels it, never the end of `Start`'s context. A stop unmounts first, then cancels the op context, which cancels the ops in flight, then closes the runtime's stores and its `git cat-file --batch` processes.
- Unmount a mount the daemon did not make only when the daemon process before this one ran the repo or still claimed a leftover there, and `ownsLeftover` matches the mount, or when it is an abandoned mount the fuse library failed to unmount. Leave every other mount alone. Record each repo a clean shutdown unmounted as stopped, so the next daemon claims none of its own mounts. Every `repo_runtime` record carries the daemon's leftover claim on the repo in `claims_leftover`, so a claim the daemon has not released outlasts a restart and leaves the reported state alone. A disabled repo releases its claim through `releaseLeftoverMount`, backed off like a mount. A running daemon ends a removed repo's claim at its next registry sync.
- Back off a retried mount, HEAD reconcile or remote refresh with `retrier`. `status` shows each failure with its next attempt.
- `gitstore.Store.CloneBlobless` already populates the git index with `read-tree HEAD`; be careful about extra index resets because they can discard staged state.

## Code conventions

- Keep merged directory logic in `Resolver.readdirTypedAt`. `ReaddirSnapshot` and `ReaddirTyped` only choose the generation.
- Resolve a path as its parent's listing shows it. `Resolver.resolvePath` resolves a directory that only overlay entries below it imply, as `mergeChildren` lists it.
- Call `requireAbsent` in `Engine.Create`, `Engine.Mkdir` and `Engine.Symlink` before they make the name. The kernel can send CREATE, MKDIR or SYMLINK for a name that appeared after its lookup missed, such as after a HEAD change. `Engine.Rename` replaces its target, so it does not call `requireAbsent`.
- Run every preparation through `core.prepare`. Express a new caller's differences as a `preparation`, not as another pipeline.
- Normalize a repo config only in `normalizeRepoConfig`, before the registry stores it. Readers trust the stored config: never re-default or re-resolve a field after reading it.
- Parse a ref only with `model.BranchName` and `model.CanonicalRef`. Keep two hand parses in `internal/cli`, because those helpers would change their meaning. Keep the deprecated `add-repo --branch` alias dropping one leading `refs/heads/` and storing the rest under `refs/heads/`. `--branch x` and `--branch refs/heads/x` both store `refs/heads/x`, and `--branch origin/x` stores `refs/heads/origin/x`. Keep `list-repos` shortening only a `refs/heads/` ref and showing every other stored ref in full.
- Keep the watcher polling `HEAD` and the ref it names, loose or in `packed-refs`. Read a linked worktree's `refs/bisect/`, `refs/worktree/` and `refs/rewritten/` refs from its own git dir. Read every other ref from the common dir. Preserve the branch-switch, packed-ref and linked-worktree behavior that `internal/watcher/watcher_test.go` and `internal/watcher/linked_test.go` cover.
- The overlay stores deletes as SQLite entries with `kind='delete'`; there is no on-disk whiteout file layer.
- SQLite is `modernc.org/sqlite` in WAL mode via `internal/meta`.
- Change a store's schema only by appending a migration to its `migrations` list. `meta.Migrate` records each applied migration in `PRAGMA user_version` and never reruns it. An edit to a shipped migration never reaches an existing database. With each new migration, add a fixture at its `user_version` to that store's `migrate_test.go`. That fixture fails the tests when a later edit changes a shipped migration.
- The FUSE adapter calls only `Engine` methods, one per operation. Do not reach into `engine.Overlay`, `engine.Hydrator`, or the `Resolver` from the adapter.
- Hold no lock while a blob hydrates. An `Engine` method that needs base content takes a `*Blobs` and returns a `notHydratedError` when a blob is missing. `Engine.Do` then hydrates with no lock held and reruns the method.
- Open every blob an `Engine` method copies before it changes anything. A directory rename is the one exception: it opens each blob as it copies it, so it may already have promoted some directories and copied some files, all with base content, and the rerun completes it.
- Take the FUSE locks in this order: `handleOps`, `DirHandle.mu`, the resolver's `transition`, `ArtifactFuse.mu`, `FileHandle.mu`.
- Release each FUSE lock with `defer`, or hold it only around code that cannot panic. The fuse library recovers a panic in an op and keeps serving, so a lock left held blocks every later op.
- Wrap an operation error's cause with `%w`, and let `toErrno` map it for the kernel. Do not return `syscall.EIO` for a failure that carries an errno.
- Switch a runtime's generation only through `Service.switchGeneration`. It sends the kernel the invalidations the switch needs through `MountedFS.InvalidateChanged`, which also unmaps each path the switch removed, on every backend.
- Never wait on a kernel invalidation inside an op or while holding a lock an op takes. The kernel can hold a directory lock while it waits for that op's reply, and the invalidation waits for the same lock.
- Keep a path's inode number while the path exists. Git compares `st_ino`, so a new number marks a file changed. A FORGET leaves the path's `pathToInode` mapping in place, dormant, for the next lookup. Unmap a path only when it stops existing: an unlink, rmdir or rename over it through the mount, or a HEAD change that removes it.
- Unmap a path that a HEAD change removed with `unmapPathLocked`. It keeps an inode the kernel still holds in `unmapped`. When the path comes back as `InodeRef.returnsAs` accepts, map it back to that inode with `mapLocked`. Otherwise retire the inode with `retireLocked`, so its handles never reach the path again. Retire it also when another inode takes the path, and when an op through the mount removes it.
- Decide an unmapped inode the same way before a reply records a new identity on it, as `allocInode` and `setReported` do, and before an unlink or a rename reaches its path, as `settleUnmappedBelow` does. Otherwise the switch pass compares the path with an identity the inode never held, and maps the path back or retires the inode wrongly.
- Run `CreateFile`, `LookUpInode` and `ReadDirPlus` through `sharedUnlessUnmapped`, and make each return `errUnmappedPath` while it holds handleOps shared and a path it replies for holds an unmapped inode. `LookUpInode` does so only once it found the path, and `CreateFile` also when its path maps to an inode. Each can retire that inode, so it must not run while a write through the inode reaches the path.
- Answer `GetInodeAttributes` for a retired inode, or for one whose path is away, with `heldAttrs`. Linux fetches an open file's attributes before a read once they expire, and fails the read with any error that getattr returns.
- Mark a handle `pinned` only with `markPinned`, after an unlink or a rename over its path committed. A write through a detached handle, or a setattr of a retired inode, reaches the handle's file only when the handle is pinned. `pinOpenFiles` points every handle open on the path at the one copy, also a handle that holds an overlay file a reconcile removed, and reopens for writing a handle that holds the copy read-only. `Engine.pinOpenFiles` runs only while a handle is open on the path, and first calls `GrantOwnerAccess`, which lets a backing file an earlier version left read-only open for writing. `setPinnedAttributes` applies that setattr once per copy and keeps the copy's permission bits in `InodeRef.Mode`, where `heldAttrs` reads them.
- Record every entry reply with `allocEntry`, or with `createEntry` for a path the op created, and every attribute reply with `setReported`. `InvalidateChanged` compares what the kernel received, and any identity a reply replaced since the last pass, with the merged view. `allocEntry` drops the name's negative entry record. The exceptions are `GetInodeAttributes` for the root and the synthesized `.git` file, which `InvalidateChanged` skips, and a `GetInodeAttributes` or `SetInodeAttributes` reply from `heldAttrs`, which describes content no path shows.
- Prune snapshot generations only through `pruneGenerations` on the daemon's `core`. An inode reads the generation of its last reported attributes, so a retired generation stays readable for `defaultGenerationRetention`.

## Writing tests

- Write a test to catch a regression, never to raise coverage. Do not add coverage targets.
- Make every regression test fail on the code before the fix. Revert the fix and watch the test fail.
- Drive behavior through the entry point real callers use, not an internal helper.
- Use real Git repositories in `t.TempDir()`. Fake the `git` binary only for what a local repository cannot produce: an HTTP error, a hang, or a pause at one step.
- Assert the observable outcome: what `git status` reports, the resolved entry, or the persisted state. A nil error or a log line is not an outcome.
- Assert a result that only the code path under test produces. An unchanged index passes whichever check skips the bootstrap. `TestBootstrapCleanStatusIndexSkipsDeleteOnlyOverlay` therefore first bootstraps the same fixture without the overlay entry.
- Call `gittest.IsolateConfig` from `TestMain` in every package whose tests run git. Prove it with a `TestGitIgnoresMachineConfig` that calls `gittest.RequireConfigIsolated`. The machine's global and system git config must not change a result. The root package keeps the machine config when `AFS_E2E_REPO` is set, so a real remote can authenticate through the ambient credential helper.
- The daemon sets `core.fsmonitor` to `<git dir>/hooks/artifact-fs-fsmonitor`, a `/bin/sh` script that execs `os.Executable()`. In a test, `os.Executable()` is the test binary. The root package's and `internal/daemon`'s `TestMain` answer `fsmonitor-hook`. Call `gittest.DisableRaceExitSleep` from each of those `TestMain`s. A race-built binary sleeps one second after it exits 0, so every git command that runs the hook would wait that second. Prove the call with a race-tagged `TestFSMonitorHookSkipsRaceExitSleep` that calls `gittest.RequireHookSkipsRaceExitSleep`.
- In a package whose `TestMain` does not answer `fsmonitor-hook`, unset `core.fsmonitor` after a test installs the hook with `ConfigureFSMonitorHook` or `ConfigureStatusOptimization`. Unset it before any git command reads the index. Otherwise git runs the package's test binary as the hook, and that binary runs the package's tests again.
- In package tests, wait for an event, not for a fixed duration. Poll for the marker the fake git writes, and fail with the operation's error if it returns first. Only `go test -timeout` bounds the wait. E2E tests keep bounded waits, because a timeout panic skips their unmount cleanup.
- Bound every fake process that a regression would leave running, for example with `sleep 60`. A timeout panic skips cleanup, so an unbounded fake outlives the test. Detect the regression from the order of the fake's markers, not from how long it ran.
- Expire a deadline under test with `gittest.ExpireOnFile` once the fake git reaches the step under test. A short timeout can expire in an earlier step on a loaded machine.
- Pin an ordering of two operations with a hook at the step under test. Never loop hoping to hit the ordering. A loop pays for every iteration on every run and can still miss the window. `TestReadAcrossAFlushShowsCompletedWrites` is the precedent: the hooks in `internal/overlay/store.go` run a flush between a read's two reads, and a read between a flush's commit and its forget.
- Where a `model.*` interface seam exists, put the hook in a test type that implements the interface, as `hookedOverlay`, `pausingSnapshot` and `gatedHydrator` do. Where no seam exists, make the hook an unexported func variable or field that production code leaves nil. Never put a hook behind a build tag.
- A `-race` probe may loop. It runs two goroutines over shared state a bounded number of times, so the race detector sees an access outside the lock. Its assertions hold under every ordering; only the race detector fails it. `TestStopReadsTheConfigUnderTheLock` and `TestReadsCopyPendingWritesUnderTheLock` are the precedents.
- `TestConcurrentFirstOpensApplyEachMigrationOnce` in the `migrate_test.go` of `internal/overlay`, `internal/registry` and `internal/snapshot` still loops hoping to hit an ordering. Do not copy it.
- Assert that something has not happened yet inside `synctest.Test`, after `synctest.Wait`. Drive ticker-based code there too. `synctest.Wait` does not treat a goroutine blocked on a `sync.Mutex` or a subprocess as idle, so those tests cannot use it.
- Never mount in a package test. Only the root-package e2e tests mount.
- Call an `Engine` method that takes a `*Blobs` through `Engine.Do`, as the FUSE adapter does. `internal/fusefs/ops_test.go` has `engineRename` and its siblings.
- Assert the errno the kernel receives with `kernelErrno`, not by comparing error values.
- Run a new e2e test as a `TestE2ESharedMount` subtest. Give it its own mount only when its subject is mounting, a daemon restart, or a fresh mount's state. Give a test its own mount also when it changes the mount and must run on FSKit, where `run` skips it. A test of an FSKit-only check that leaves the mount unchanged uses `runReadOnly` on the shared mount. macFUSE's FSKit extension can stop loading after a series of mounts in one session.
- Register a shared-mount test that leaves the mount unchanged with `runReadOnly`, and every other one with `run`. When e2e tests mount through FSKit on macOS, `run` skips the test, because the restore that undoes its changes moves HEAD. A `runReadOnly` test that changes the mount fails.
- Call `skipIfFSKitHEADMove` in a test that moves HEAD on its own mount. After a HEAD move, FSKit can pad a short read with bytes that are not file data ([macfuse#1231](https://github.com/macfuse/macfuse/issues/1231)).
- Before a test's first mount, call `skipIfOverMountBudget` with the number of mounts the test starts. A run that mounts through FSKit starts at most 8 mounts. `newMountedE2ERepo` checks one mount.

## Validation

- Start with the narrowest relevant test: `go test ./internal/<pkg>` or `go test -run TestName ./internal/<pkg>`.
- For non-trivial code changes, reproduce CI in order: `go run golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@v0.52.0 -fix ./...` with no resulting diff, `go build -o /dev/null ./cmd/artifact-fs`, `go vet ./...` with `GOOS=linux` and with `GOOS=darwin`, then `go test -race -count=1 ./...`.
- Build only the CLI with `go build ./cmd/artifact-fs`.
- Enable the pre-commit checks with `git config core.hooksPath .githooks`.
- Benchmarks are opt-in: `AFS_RUN_BENCH=1 go test -run TestBenchRepos -v`.
- FUSE e2e tests are opt-in. Run them with the same command on macOS and Linux: `AFS_RUN_E2E_TESTS=1 go test -run 'TestE2E|TestFUSEMountSmoke' -count=1 -timeout 20m -v .`. Keep `-timeout 20m`: the default 10m can expire mid-suite, and a timeout panic skips every unmount cleanup.
- On Linux, e2e tests need `/dev/fuse` plus either root or `fusermount3` from the `fuse3` package. Unmount a leftover mount with `fusermount3 -u <path>`, or `fusermount3 -uz <path>` when it hangs.
- On macOS, e2e tests mount through FSKit. Set `ARTIFACT_FS_FUSE_BACKEND=kernel` only to test the macFUSE kernel extension; without a loaded kernel extension those tests skip. Unmount a leftover FSKit mount with `diskutil unmount <path>`. A deadlocked FSKit mount cannot be unmounted until reboot.
- E2E tests use a local bare repository by default. Set `AFS_E2E_REPO` only when intentionally testing a real remote.
- E2E benchmark coverage is separate: `AFS_RUN_E2E_BENCH=1 go test -run TestE2EBenchmarkRepos -v .`.
- Package tests and `Benchmark*` functions live beside the code. `TestBenchRepos`, `TestE2EBenchmarkRepos` and every `e2e*_test.go` live in the root package.
