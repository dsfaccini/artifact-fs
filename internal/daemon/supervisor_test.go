package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
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

func (f *failedConnectionMount) Backend() string              { return "fake" }
func (f *failedConnectionMount) InvalidateChanged() error     { return nil }
func (f *failedConnectionMount) OpenFileGenerations() []int64 { return nil }

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
