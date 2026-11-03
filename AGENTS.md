# AGENTS.md

Repository-specific guidance for `artifact-fs`.

## Code review rules

- Flag credentials in Git CLI arguments or unredacted logs. Pass credentials through the environment-based helper in `internal/gitstore/credentials.go`, and redact log output with `auth.RedactString`.
- Flag blob bytes converted to `string`. Keep `BlobToCache` binary-safe by streaming `git cat-file --batch` output to disk.
- Keep `GIT_NO_LAZY_FETCH=1` on `git cat-file --batch-check` in `batchResolveSizes`. Removing it turns size resolution in blobless clones into network round-trips.
- Normalize paths only with `model.CleanPath()`. Do not add local wrappers.
- Use the canonical `model.*` interfaces for snapshot, overlay, hydrator, and gitstore access. Do not add subset interfaces around them.

## Architecture

- `cmd/artifact-fs` is the only binary entrypoint.
- `internal/cli` wires commands onto `daemon.Service`.
- `internal/daemon` owns repo lifecycle: registry sync, snapshot publish, overlay reconcile, FUSE mount, watcher, refresh loop.
- `internal/fusefs` is the merged view and writable filesystem layer.
- `internal/gitstore` is the performance-sensitive git wrapper; most easy-to-break invariants live there.
- `internal/snapshot` and `internal/overlay` are persistent SQLite-backed stores.

## Runtime contracts

- `ARTIFACT_FS_ROOT` is the state root. `artifact-fs daemon --root` is the mount root. They are different things.
- `add-repo` is one-shot by default: register repo, clone blobless, build the initial snapshot, then exit. It does not mount FUSE or start background goroutines.
- `add-repo --async` only registers prepare state. The daemon mounts a gated placeholder, prepares clone/fetch and snapshot in the background, then opens the gate and starts watcher/refresh.
- `daemon` is long-running: it mounts registered repos and starts watcher, refresh, and hydrator workers.
- `git.CloneBlobless` already populates the git index with `read-tree HEAD`; be careful about extra index resets because they can discard staged state.

## Code conventions

- `Readdir()` stays thin; merged directory logic belongs in `ReaddirTyped()`.
- The watcher polls `HEAD` plus the current HEAD ref path. If you change it, preserve branch-switch and packed-ref behavior covered by `internal/watcher/watcher_test.go`.
- The overlay stores deletes as SQLite entries with `kind='delete'`; there is no on-disk whiteout file layer.
- SQLite is `modernc.org/sqlite` in WAL mode via `internal/meta`.

## Writing tests

- Write a test to catch a regression, never to raise coverage. Do not add coverage targets.
- Make every regression test fail on the code before the fix. Revert the fix and watch the test fail.
- Drive behavior through the entry point real callers use, not an internal helper.
- Use real Git repositories in `t.TempDir()`. Fake the `git` binary only to inject failures a local repository cannot produce, such as HTTP errors.
- Assert the observable outcome: what `git status` reports, the resolved entry, or the persisted state. A nil error or a log line is not an outcome.
- Assert a result that only the code path under test produces. `TestBootstrapCleanStatusIndexSkipsDeleteOnlyOverlay` checks only that the index is unchanged, so it passes whichever check skips the bootstrap.
- Call `gittest.IsolateConfig` from `TestMain` in every package whose tests run git. Prove it with a `TestGitIgnoresMachineConfig` that calls `gittest.RequireConfigIsolated`. The machine's global and system git config must not change a result.
- Wait for an event, not for a fixed duration. Poll for the marker the fake git writes, and fail with the operation's error if it returns first. Only `go test -timeout` bounds the wait.
- Expire a deadline under test with `gittest.ExpireOnFile` once the fake git reaches the step under test. A short timeout can expire in an earlier step on a loaded machine.
- Assert that something has not happened yet inside `synctest.Test`, after `synctest.Wait`. Drive ticker-based code there too. `synctest.Wait` does not treat a goroutine blocked on a `sync.Mutex` or a subprocess as idle, so those tests cannot use it.
- Never mount in a package test. Only the root-package e2e tests mount.
- Run a new e2e test as a `TestE2ESharedMount` subtest. Give it its own mount only when its subject is mounting, a daemon restart, or a fresh mount's state. macFUSE's FSKit extension can stop loading after a series of mounts in one session.

## Validation

- Start with the narrowest relevant test: `go test ./internal/<pkg>` or `go test -run TestName ./internal/<pkg>`.
- For non-trivial code changes, reproduce CI in order: `go build -o /dev/null ./cmd/artifact-fs`, `go vet ./...` with `GOOS=linux`, with `GOOS=darwin`, and with `GOOS=darwin` plus `-tags fskit`, then `go test -race ./...` and `go test -race -tags fskit ./internal/fusefs`.
- Build only the CLI with `go build -tags fskit ./cmd/artifact-fs`. The `fskit` tag adds the macOS FSKit transport and changes nothing on Linux.
- Enable the pre-commit checks with `git config core.hooksPath .githooks`.
- Benchmarks are opt-in: `AFS_RUN_BENCH=1 go test -run TestBenchRepos -v`.
- FUSE e2e tests are opt-in. Run them with the same command on macOS and Linux: `AFS_RUN_E2E_TESTS=1 go test -tags fskit -run TestE2E -count=1 -timeout 20m -v .`. Keep `-timeout 20m`: the default 10m can expire mid-suite, and a timeout panic skips every unmount cleanup.
- On Linux, e2e tests need `/dev/fuse` plus either root or `fusermount3` from the `fuse3` package. Unmount a leftover mount with `fusermount3 -u <path>`, or `fusermount3 -uz <path>` when it hangs.
- On macOS, e2e tests mount through FSKit. Without `-tags fskit`, they skip unless the macFUSE kernel extension is loaded. Set `ARTIFACT_FS_FUSE_BACKEND=kernel` only to test the macFUSE kernel extension; without a loaded kernel extension those tests skip. Unmount a leftover FSKit mount with `diskutil unmount <path>`. A deadlocked FSKit mount cannot be unmounted until reboot.
- E2E tests use a local bare repository by default. Set `AFS_E2E_REPO` only when intentionally testing a real remote.
- E2E benchmark coverage is separate: `AFS_RUN_E2E_BENCH=1 go test -tags fskit -run TestE2EBenchmarkRepos -v .`.
- Package tests live beside the code. Benchmarks and `e2e*_test.go` live in the root package.
