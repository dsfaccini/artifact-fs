package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// resolverMount mounts through next and keeps the resolver that the mount
// would serve and the redact it would log the fuse library's errors through.
type resolverMount struct {
	next     mountFunc
	mu       sync.Mutex
	resolved *fusefs.Resolver
	redact   func(string) string
}

func (m *resolverMount) mount(ctx, opCtx context.Context, cfg model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger, redact func(string) string) (fusefs.MountedFS, error) {
	m.mu.Lock()
	m.resolved, m.redact = engine.Resolver, redact
	m.mu.Unlock()
	return m.next(ctx, opCtx, cfg, engine, gate, logger, redact)
}

func (m *resolverMount) resolver() *fusefs.Resolver {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolved
}

func (m *resolverMount) redactor() func(string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.redact
}

// startedDaemon is a daemon whose Start mounted one repo cloned from a local
// work repository.
type startedDaemon struct {
	svc     *Service
	mounts  *resolverMount
	logs    *lockedBuffer
	root    string
	workDir string
	gitDir  string
	// startErr delivers Start's error if Start returns before the test ends.
	startErr chan error
}

func startDaemonWithRepo(t *testing.T) *startedDaemon {
	t.Helper()
	return startDaemonWithMount(t, fakeMount, "")
}

// startDaemonWithMount starts a daemon that mounts the repo at mountPath
// through mount. An empty mountPath mounts it under the test's mount root.
// The daemon never polls the registry, and retries a failed HEAD reconcile
// after a millisecond.
func startDaemonWithMount(t *testing.T, mount mountFunc, mountPath string) *startedDaemon {
	t.Helper()
	ctx := context.Background()
	tmp := shortTempDir(t)
	d := &startedDaemon{
		mounts:  &resolverMount{next: mount},
		logs:    &lockedBuffer{},
		root:    filepath.Join(tmp, "artifact-fs"),
		workDir: filepath.Join(tmp, "work"),
	}
	d.gitDir = filepath.Join(d.root, "repos", "repo", "git")
	runCmd(t, "git", "init", "--initial-branch", "main", d.workDir)
	d.commit(t, "README.md", "base\n")

	svc, err := New(ctx, d.root, slog.New(slog.NewTextHandler(d.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.svc = svc
	svc.mount = d.mounts.mount
	svc.syncInterval = time.Hour
	svc.headReconcileBackoff = time.Millisecond
	// A remote refresh never runs during the test, so only the HEAD watcher can start a reconcile.
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", MountPath: mountPath}, RepoSpec: model.RepoSpec{RemoteURL: d.workDir, Branch: "main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Hour}, Enabled: true}); err != nil {
		_ = svc.Close()
		t.Fatal(err)
	}
	startCtx, cancel := context.WithCancel(ctx)
	d.startErr = make(chan error, 1)
	stopped := make(chan error, 1)
	go func() {
		err := svc.Start(startCtx)
		d.startErr <- err
		stopped <- err
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-stopped; !errors.Is(err, context.Canceled) {
			t.Errorf("Start = %v, want context canceled", err)
		}
		if err := svc.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	waitFor(t, d.startErr, func() bool {
		st, err := svc.ctl().Status(ctx, "repo")
		return err == nil && st.State == repoStateMounted && d.mounts.resolver() != nil
	})
	return d
}

// commit commits content to name in the work repository and returns the commit.
func (d *startedDaemon) commit(t *testing.T, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.workDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", d.workDir, "add", name)
	runCmd(t, "git", "-C", d.workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", name)
	return strings.TrimSpace(runCmdOutput(t, "git", "-C", d.workDir, "rev-parse", "HEAD"))
}

// moveMain fetches oid into the daemon's clone and points main, which HEAD is on, at it.
func (d *startedDaemon) moveMain(t *testing.T, oid string) {
	t.Helper()
	runCmd(t, "git", "--git-dir", d.gitDir, "fetch", "--no-tags", "origin", "main")
	runCmd(t, "git", "--git-dir", d.gitDir, "update-ref", "refs/heads/main", oid)
}

func (d *startedDaemon) status(t *testing.T) model.RepoRuntimeState {
	t.Helper()
	st, err := d.svc.ctl().Status(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// waitForServedHEAD waits until status reports oid at generation and the
// mounted resolver serves path as blob.
func (d *startedDaemon) waitForServedHEAD(t *testing.T, oid string, generation int64, path, blob string) {
	t.Helper()
	waitFor(t, d.startErr, func() bool {
		st := d.status(t)
		node, err := d.mounts.resolver().ResolvePath(path)
		return st.CurrentHEADOID == oid && st.SnapshotGeneration == generation &&
			err == nil && node.Base.ObjectOID == blob
	})
}

func (d *startedDaemon) blobOID(t *testing.T, commit, path string) string {
	t.Helper()
	return strings.TrimSpace(runCmdOutput(t, "git", "-C", d.workDir, "rev-parse", commit+":"+path))
}

func TestStartWatchesHEADAndServesReconciledSnapshot(t *testing.T) {
	t.Parallel()
	d := startDaemonWithRepo(t)
	initial := d.status(t)

	first := d.commit(t, "added.txt", "first\n")
	d.moveMain(t, first)
	d.waitForServedHEAD(t, first, initial.SnapshotGeneration+1, "added.txt", d.blobOID(t, first, "added.txt"))

	// The first move can land before the watcher takes its baseline and reach
	// the reconcile through the watcher's startup call. Only polling the
	// daemon's gitdir sees this one.
	second := d.commit(t, "added.txt", "second\n")
	d.moveMain(t, second)
	d.waitForServedHEAD(t, second, initial.SnapshotGeneration+2, "added.txt", d.blobOID(t, second, "added.txt"))
}

func TestStartRetriesHEADReconcileAfterSnapshotLock(t *testing.T) {
	t.Parallel()
	d := startDaemonWithRepo(t)
	initial := d.status(t)

	db, err := meta.OpenDB(filepath.Join(d.root, "meta", "repo.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`INSERT INTO repo_state(key,value) VALUES('test_reconcile_lock','held')`); err != nil {
		t.Fatal(err)
	}

	want := d.commit(t, "README.md", "reconciled after the lock\n")
	d.moveMain(t, want)
	waitFor(t, d.startErr, func() bool { return d.status(t).LastErrorPhase == phaseHEADReconcile })
	if st := d.status(t); st.CurrentHEADOID != initial.CurrentHEADOID || st.SnapshotGeneration != initial.SnapshotGeneration || st.NextRetryAt.IsZero() || !strings.Contains(st.LastError, "locked") {
		t.Fatalf("status while the snapshot was locked = %+v, want the old HEAD and the retried failure", st)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	d.waitForServedHEAD(t, want, initial.SnapshotGeneration+1, "README.md", d.blobOID(t, want, "README.md"))
}

// connFailureMounts first mounts a mount whose connection fails once fail is
// closed, then fake mounts. A failed connection leaves its mount in place
// until it unmounts. It records each mount and unmount in order.
type connFailureMounts struct {
	fail chan struct{}
	// release lets the remount return, so a test can observe the state before it.
	release     chan struct{}
	releaseOnce sync.Once
	mu          sync.Mutex
	calls       []string
	// unmountFails is how many unmounts of the failed mount fail first.
	unmountFails int
	// abort makes the failed connection's Join return nil, as after an
	// abort. Set it before closing fail.
	abort bool
	// remote is the repo's remote, which the failed connection's error names.
	// Set it before closing fail.
	remote string
	// joined receives when a join goroutine ends.
	joined chan struct{}
}

// startConnFailureDaemon starts a daemon whose repo, mounted at mountPath,
// mounts through a connFailureMounts.
func startConnFailureDaemon(t *testing.T, unmountFails int, mountPath string) (*startedDaemon, *connFailureMounts) {
	t.Helper()
	m := &connFailureMounts{fail: make(chan struct{}), release: make(chan struct{}), unmountFails: unmountFails, joined: make(chan struct{}, 1)}
	d := startDaemonWithMount(t, m.mount, mountPath)
	m.remote = d.workDir
	// The failed connection's join reads the hook only after fail closes.
	d.svc.joinFinished = func() {
		select {
		case m.joined <- struct{}{}:
		default:
		}
	}
	// Runs before the daemon's cleanup, which waits for a remount.
	t.Cleanup(m.releaseRemount)
	return d, m
}

func (m *connFailureMounts) releaseRemount() { m.releaseOnce.Do(func() { close(m.release) }) }

func (m *connFailureMounts) mount(ctx, opCtx context.Context, cfg model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger, redact func(string) string) (fusefs.MountedFS, error) {
	m.mu.Lock()
	remount := len(m.calls) > 0
	m.calls = append(m.calls, "mount")
	m.mu.Unlock()
	if remount {
		<-m.release
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger, redact)
	}
	return &failedConnectionMount{m: m}, nil
}

func (m *connFailureMounts) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

type failedConnectionMount struct {
	m *connFailureMounts
}

func (f *failedConnectionMount) Backend() string                     { return "fake" }
func (f *failedConnectionMount) InvalidateChanged() error            { return nil }
func (f *failedConnectionMount) OpenFileGenerations() []int64        { return nil }
func (f *failedConnectionMount) OpenFilePIDs([]int64) map[uint32]int { return nil }

func (f *failedConnectionMount) Join(ctx context.Context) error {
	select {
	case <-f.m.fail:
		if f.m.abort {
			return nil
		}
		return fmt.Errorf("readMessage: transport to %s failed", f.m.remote)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *failedConnectionMount) Unmount(context.Context) error {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.m.unmountFails > 0 {
		f.m.unmountFails--
		f.m.calls = append(f.m.calls, "unmount failed")
		return errors.New("busy")
	}
	f.m.calls = append(f.m.calls, "unmount")
	return nil
}

// failConnection fails the mounted repo's connection and wakes the sync loop
// once the connection's join goroutine ended and the unmount it started
// returned. A sync before that would race the join for the failed mount.
func failConnection(t *testing.T, d *startedDaemon, m *connFailureMounts) {
	t.Helper()
	close(m.fail)
	select {
	case <-m.joined:
	case err := <-d.startErr:
		t.Fatalf("Start returned before the failed connection's join ended: %v", err)
	}
	waitFor(t, d.startErr, func() bool {
		d.svc.mu.Lock()
		defer d.svc.mu.Unlock()
		return len(d.svc.unmounting) == 0
	})
	wake(d.svc)
}

// failConnectionUntilRemount fails the mounted repo's connection and returns
// the mount calls up to the remount, with the remount still in progress.
func failConnectionUntilRemount(t *testing.T, d *startedDaemon, m *connFailureMounts) []string {
	t.Helper()
	failConnection(t, d, m)
	waitFor(t, d.startErr, func() bool { return slices.Contains(m.recorded()[1:], "mount") })
	return m.recorded()
}

func TestStartUnmountsFailedConnectionBeforeRemount(t *testing.T) {
	t.Parallel()
	d, m := startConnFailureDaemon(t, 0, "")

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "unmount", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	st := d.status(t)
	if want := "readMessage: transport to REDACTED_REMOTE failed"; st.State != repoStateDegraded || st.MountError != want {
		t.Fatalf("state = %q, mount_error = %q; want %q, %q", st.State, st.MountError, repoStateDegraded, want)
	}
	for line := range strings.SplitSeq(d.logs.String(), "\n") {
		if strings.Contains(line, "fuse connection failed") && (strings.Contains(line, d.workDir) || !strings.Contains(line, "REDACTED_REMOTE")) {
			t.Fatalf("connection failure log is not redacted: %s", line)
		}
	}
}

func TestStartRemountsAfterFailedUnmountWhenNothingIsMounted(t *testing.T) {
	t.Parallel()
	// Nothing is mounted under the test's mount root, so the failed unmount leaves no mount behind.
	d, m := startConnFailureDaemon(t, 1, "")

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "unmount failed", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestStartRemountsWithoutUnmountWhenConnectionEndsUnmounted(t *testing.T) {
	t.Parallel()
	// Nothing is mounted under the test's mount root, as after an external unmount.
	d, m := startConnFailureDaemon(t, 0, "")
	m.abort = true

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestStartRetriesFailedConnectionUnmountBeforeRemount(t *testing.T) {
	t.Parallel()
	// "/" is always in the mount table, so the failed unmount leaves a mount behind.
	d, m := startConnFailureDaemon(t, 1, "/")

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "unmount failed", "unmount", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestStartUnmountsAbortedConnectionBeforeRemount(t *testing.T) {
	t.Parallel()
	// "/" is always in the mount table, so the aborted connection's mount stays in place.
	d, m := startConnFailureDaemon(t, 0, "/")
	m.abort = true

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "unmount", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	if st := d.status(t); st.State != repoStateDegraded || st.MountError != errConnectionEndedMounted.Error() {
		t.Fatalf("state = %q, mount_error = %q; want %q, %q", st.State, st.MountError, repoStateDegraded, errConnectionEndedMounted)
	}
}

func TestRemoveRepoUnmountsFailedConnectionLeftMounted(t *testing.T) {
	t.Parallel()
	d, m := startConnFailureDaemon(t, 2, "/")
	// The daemon unmounts once after the failure and once more in the
	// supervisor's retry, which then backs off.
	failConnection(t, d, m)
	waitFor(t, d.startErr, func() bool { return len(m.recorded()) >= 3 })

	// A remount after the retry's backoff returns at once, so it fails the
	// call sequence below instead of blocking the daemon's sync loop.
	m.releaseRemount()
	// remove-repo runs in its own process, so the daemon's next registry sync stops the runtime.
	ctx := context.Background()
	cliSvc, err := NewController(ctx, d.root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	err = cliSvc.RemoveRepo(ctx, "repo")
	if closeErr := cliSvc.Close(); err != nil || closeErr != nil {
		t.Fatalf("CLI remove-repo: %v, close: %v", err, closeErr)
	}
	wake(d.svc)
	waitFor(t, d.startErr, func() bool {
		d.svc.mu.Lock()
		defer d.svc.mu.Unlock()
		return len(d.svc.running) == 0
	})
	if want := []string{"mount", "unmount failed", "unmount failed", "unmount"}; !slices.Equal(m.recorded(), want) {
		t.Fatalf("calls = %q, want %q", m.recorded(), want)
	}
}

func TestMountPointHeld(t *testing.T) {
	// /dev is a mount point on macOS and Linux. The mount table lists it only
	// by its absolute resolved path.
	tmp := t.TempDir()
	rootLink, devLink := filepath.Join(tmp, "root"), filepath.Join(tmp, "dev")
	for link, target := range map[string]string{rootLink: "/", devLink: "/dev"} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(tmp, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"parent symlink", filepath.Join(rootLink, "dev"), true},
		{"final symlink", devLink, true},
		{"relative", "dev", true},
		{"unresolvable parent", filepath.Join(tmp, "gone", "repo"), true},
		{"unmounted", filepath.Join(tmp, "repo"), false},
		{"unreadable mount point", filepath.Join(file, "repo"), true},
	}
	if runtime.GOOS == "darwin" {
		tests = append(tests, struct {
			name string
			path string
			want bool
		}{"other case", "/DEV", true})
	}
	t.Chdir("/")
	// The mount table must not depend on mount(8) being on PATH, as under launchd.
	t.Setenv("PATH", "")
	if !isMounted("/dev") {
		t.Error("isMounted(/dev) = false with an empty PATH")
	}
	for _, tt := range tests {
		if got := mountPointHeld(tt.path); got != tt.want {
			t.Errorf("%s: mountPointHeld(%q) = %t, want %t", tt.name, tt.path, got, tt.want)
		}
	}
}

// The daemon mounts with a redact that removes the repo's remote, so a fuse
// library error line that names it, such as a failed lazy fetch's, never logs
// it. The remote here is a local path, which only the remote itself matches.
func TestStartMountRedactsTheRemoteFromFuseErrors(t *testing.T) {
	t.Parallel()
	d := startDaemonWithRepo(t)
	line := fmt.Sprintf("Op 0x00000002 *fuseops.ReadFileOp] -> Error: %q", "fetch "+d.workDir+": exit status 128")
	if got := d.mounts.redactor()(line); strings.Contains(got, d.workDir) || !strings.Contains(got, "REDACTED_REMOTE") {
		t.Fatalf("redact(%q) = %q, want the remote redacted", line, got)
	}
}

// A retried mount that found the prepare lock held writes its status config
// at a later sync, once the lock is free. For a verified source that write
// pins core.worktree to the mount point, and nothing else writes the pin.
func TestRetriedMountWritesTheStatusConfigOnceThePrepareLockFrees(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	commit := commitFile(t, remote, "README.md", "verified\n")
	runCmd(t, "git", "-C", remote, "config", "uploadpack.allowFilter", "true")
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.refreshTicks = make(chan time.Time)
	busy := make(chan struct{}, 1)
	svc.lockBusy = func(lockName string) {
		if lockName != "verified.prepare.lock" {
			return
		}
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, repo model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger, redact func(string) string) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return nil, errors.New("the first mount fails")
		}
		return fakeMount(ctx, opCtx, repo, engine, gate, logger, redact)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "verified"}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + remote, Branch: "refs/heads/main", RequiredCommit: commit, HistoryDepth: 1}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Hour}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg, err = svc.registry.GetRepo(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	pin := func() string {
		t.Helper()
		out, err := exec.Command("git", "--git-dir", cfg.GitDir, "config", "--get", "core.worktree").Output()
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return ""
		}
		if err != nil {
			t.Fatalf("read core.worktree: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 1 {
		t.Fatalf("the mount check mounted %d times, want 1", mounts.Load())
	}
	held, release, unlocked := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		unlocked <- svc.withRepoPrepareLock(ctx, cfg.Name, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	synced := make(chan error, 1)
	go func() { synced <- svc.syncRepos(ctx) }()
	select {
	case err := <-synced:
		if err != nil {
			t.Fatal(err)
		}
	case <-busy:
		close(release)
		t.Fatal("the retried mount waited for the prepare lock that another step held")
	}
	if mounts.Load() != 2 {
		t.Fatalf("the sync after a failed mount mounted %d times in all, want 2", mounts.Load())
	}
	if got := pin(); got != "" {
		t.Fatalf("core.worktree = %q after a retried mount while another step held the prepare lock, want it unset", got)
	}
	close(release)
	if err := <-unlocked; err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 2 {
		t.Fatalf("the mount served, yet the next sync mounted again: %d mounts in all, want 2", mounts.Load())
	}
	if got := pin(); got != cfg.MountPath {
		t.Fatalf("core.worktree = %q at the sync after the prepare lock was free, want %q", got, cfg.MountPath)
	}
}

// pausingCLIGit pauses the first FetchNonInteractive or ResolveHEAD after it
// is armed, until release closes. A fetch command runs the one, and a CLI
// preparation the other, under the repo's prepare lock.
type pausingCLIGit struct {
	model.GitStore
	armed   atomic.Bool
	held    chan struct{}
	release chan struct{}
}

func (g *pausingCLIGit) pause() {
	if g.armed.CompareAndSwap(true, false) {
		close(g.held)
		<-g.release
	}
}

func (g *pausingCLIGit) FetchNonInteractive(ctx context.Context, cfg model.RepoConfig) error {
	g.pause()
	return g.GitStore.FetchNonInteractive(ctx, cfg)
}

func (g *pausingCLIGit) ResolveHEAD(ctx context.Context, cfg model.RepoConfig) (string, string, error) {
	g.pause()
	return g.GitStore.ResolveHEAD(ctx, cfg)
}

// lockedMountCheckSteps are the CLI steps that can take a repo's prepare lock
// after a sync pass listed the repo ready, and before the pass reaches it.
var lockedMountCheckSteps = []string{"fetch command", "cli prepare", "re-registering sync add-repo"}

// lockedMountCheck is a sync pass over the sync repos a, b and c, which no
// runtime serves yet, while a CLI step holds b's prepare lock. The step takes
// the lock from a hook in a's mount, after the pass listed b, and holds it
// until release. passed closes when the pass returns, with its error in
// passErr, and stepped when the step returns, with its error in stepErr.
// waited receives once the daemon finds b's prepare lock held and waits.
type lockedMountCheck struct {
	svc     *Service
	passed  chan struct{}
	passErr error
	stepped chan struct{}
	stepErr error
	waited  chan struct{}
	release func()
}

func startLockedMountCheck(t *testing.T, step string) *lockedMountCheck {
	t.Helper()
	ctx := t.Context()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	commitFile(t, remote, "README.md", "main\n")
	root := filepath.Join(tmp, "artifact-fs")
	svc, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	repo := func(name string) model.RepoConfig {
		return model.RepoConfig{RepoPaths: model.RepoPaths{Name: name}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Hour}, Enabled: true}
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := svc.ctl().AddRepoWithOptions(ctx, repo(name), AddRepoOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	ctl, err := NewController(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	cliGit := &pausingCLIGit{GitStore: ctl.git, held: make(chan struct{}), release: make(chan struct{})}
	ctl.git = cliGit
	m := &lockedMountCheck{svc: svc, passed: make(chan struct{}), stepped: make(chan struct{}), waited: make(chan struct{}, 1), release: sync.OnceFunc(func() { close(cliGit.release) })}
	svc.lockBusy = func(lockName string) {
		if lockName != "b.prepare.lock" {
			return
		}
		select {
		case m.waited <- struct{}{}:
		default:
		}
	}
	var stepping atomic.Bool
	svc.mount = func(mctx, opCtx context.Context, p model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger, redact func(string) string) (fusefs.MountedFS, error) {
		if p.Name == "a" {
			stepping.Store(true)
			go func() {
				defer close(m.stepped)
				switch step {
				case "fetch command":
					cliGit.armed.Store(true)
					m.stepErr = ctl.FetchNow(ctx, "b")
				case "cli prepare":
					cliGit.armed.Store(true)
					_, m.stepErr = ctl.Prepare(ctx, "b")
				case "re-registering sync add-repo":
					if m.stepErr = ctl.RemoveRepo(ctx, "b"); m.stepErr != nil {
						return
					}
					cliGit.armed.Store(true)
					m.stepErr = ctl.AddRepoWithOptions(ctx, repo("b"), AddRepoOptions{})
				}
			}()
			select {
			case <-cliGit.held:
			case <-m.stepped:
				t.Errorf("the %s returned %v before it held b's prepare lock", step, m.stepErr)
			}
		}
		return fakeMount(mctx, opCtx, p, engine, gate, logger, redact)
	}
	go func() {
		m.passErr = svc.syncRepos(ctx)
		close(m.passed)
	}()
	t.Cleanup(func() {
		m.release()
		<-m.passed
		if stepping.Load() {
			<-m.stepped
		}
	})
	return m
}

// record returns the repo_runtime record of the repo name, which must exist.
func (m *lockedMountCheck) record(t *testing.T, name string) model.RepoRuntimeRecord {
	t.Helper()
	rec, ok, err := m.svc.registry.GetRuntime(t.Context(), model.RepoID(name))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("%s has no repo_runtime record", name)
	}
	return rec
}

// One sync loop serves every repo, and a CLI step can hold a repo's prepare
// lock for up to the prepare timeout. So the mount check of b skips b while
// the step holds b's lock, and the pass mounts c.
func TestMountCheckSkipsARepoWhosePrepareLockIsHeld(t *testing.T) {
	t.Parallel()
	for _, step := range lockedMountCheckSteps {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			m := startLockedMountCheck(t, step)
			select {
			case <-m.passed:
			case <-m.waited:
				t.Fatalf("the mount check of b waited for b's prepare lock while the %s held it", step)
			}
			if m.passErr != nil {
				t.Fatal(m.passErr)
			}
			if rec := m.record(t, "c"); rec.State != repoStateMounted {
				t.Fatalf("c's repo_runtime after the pass = %+v, want it mounted", rec)
			}
		})
	}
}

// A skipped mount check records no failure, so nothing backs off b's mount,
// and the next sync after the step ends mounts b. A CLI preparation or a
// re-registration changes the prepare request or the config the pass listed,
// so a mount check that waited for the lock would fail b as superseded.
func TestSkippedMountCheckMountsTheRepoAtTheNextSync(t *testing.T) {
	t.Parallel()
	for _, step := range lockedMountCheckSteps {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			m := startLockedMountCheck(t, step)
			// The pass reached b while the step held b's lock.
			select {
			case <-m.passed:
			case <-m.waited:
			}
			m.release()
			<-m.stepped
			if m.stepErr != nil {
				t.Fatalf("the %s: %v", step, m.stepErr)
			}
			<-m.passed
			if m.passErr != nil {
				t.Fatal(m.passErr)
			}
			if rec := m.record(t, "b"); rec.LastError != "" {
				t.Fatalf("b's repo_runtime after the pass records the failure %q in phase %q, want none", rec.LastError, rec.LastErrorPhase)
			}
			if err := m.svc.syncRepos(t.Context()); err != nil {
				t.Fatal(err)
			}
			if rec := m.record(t, "b"); rec.State != repoStateMounted || rec.LastError != "" {
				t.Fatalf("b's repo_runtime after the next sync = %+v, want it mounted with no failure", rec)
			}
		})
	}
}
