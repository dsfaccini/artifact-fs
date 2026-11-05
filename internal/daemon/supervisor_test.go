package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// would serve.
type resolverMount struct {
	next     mountFunc
	mu       sync.Mutex
	resolved *fusefs.Resolver
}

func (m *resolverMount) mount(cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
	m.mu.Lock()
	m.resolved = engine.Resolver
	m.mu.Unlock()
	return m.next(cfg, engine, gate, logger)
}

func (m *resolverMount) resolver() *fusefs.Resolver {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolved
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
func startDaemonWithMount(t *testing.T, mount mountFunc, mountPath string) *startedDaemon {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
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
	svc.SetMountRoot(filepath.Join(tmp, "mnt"))
	svc.mount = d.mounts.mount
	// A remote refresh never runs during the test, so only the HEAD watcher can start a reconcile.
	if err := svc.AddRepo(ctx, model.RepoConfig{Name: "repo", RemoteURL: d.workDir, Branch: "main", MountPath: mountPath, RefreshInterval: time.Hour, Enabled: true}); err != nil {
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
		st, err := svc.Status(ctx, "repo")
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
	st, err := d.svc.Status(context.Background(), "repo")
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
	waitFor(t, d.startErr, func() bool { return strings.Contains(d.logs.String(), "snapshot publish failed") })
	if st := d.status(t); st.CurrentHEADOID != initial.CurrentHEADOID || st.SnapshotGeneration != initial.SnapshotGeneration {
		t.Fatalf("HEAD advanced while the snapshot was locked: %+v", st)
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
	release chan struct{}
	mu      sync.Mutex
	calls   []string
	// unmountFails is how many unmounts of the failed mount fail first.
	unmountFails int
	// abort makes the failed connection's Join return nil, as after an
	// abort. Set it before closing fail.
	abort bool
}

// startConnFailureDaemon starts a daemon whose repo, mounted at mountPath,
// mounts through a connFailureMounts.
func startConnFailureDaemon(t *testing.T, unmountFails int, mountPath string) (*startedDaemon, *connFailureMounts) {
	t.Helper()
	m := &connFailureMounts{fail: make(chan struct{}), release: make(chan struct{}), unmountFails: unmountFails}
	d := startDaemonWithMount(t, m.mount, mountPath)
	// Runs before the daemon's cleanup, which waits for a remount.
	t.Cleanup(func() { close(m.release) })
	return d, m
}

func (m *connFailureMounts) mount(cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
	m.mu.Lock()
	remount := len(m.calls) > 0
	m.calls = append(m.calls, "mount")
	m.mu.Unlock()
	if remount {
		<-m.release
		return fakeMount(cfg, engine, gate, logger)
	}
	return &failedConnectionMount{m: m, err: fmt.Errorf("readMessage: transport to %s failed", cfg.RemoteURL)}, nil
}

func (m *connFailureMounts) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

type failedConnectionMount struct {
	m   *connFailureMounts
	err error
}

func (f *failedConnectionMount) Backend() string { return "fake" }

func (f *failedConnectionMount) Join(ctx context.Context) error {
	select {
	case <-f.m.fail:
		if f.m.abort {
			return nil
		}
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *failedConnectionMount) Unmount() error {
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

// failConnectionUntilRemount fails the mounted repo's connection and returns
// the mount calls up to the remount, with the remount still in progress.
func failConnectionUntilRemount(t *testing.T, d *startedDaemon, m *connFailureMounts) []string {
	t.Helper()
	close(m.fail)
	waitFor(t, d.startErr, func() bool { return slices.Contains(m.recorded()[1:], "mount") })
	return m.recorded()
}

func TestStartUnmountsFailedConnectionBeforeRemount(t *testing.T) {
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
	// Nothing is mounted under the test's mount root, so the failed unmount leaves no mount behind.
	d, m := startConnFailureDaemon(t, 1, "")

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "unmount failed", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestStartRemountsWithoutUnmountWhenConnectionEndsUnmounted(t *testing.T) {
	// Nothing is mounted under the test's mount root, as after an external unmount.
	d, m := startConnFailureDaemon(t, 0, "")
	m.abort = true

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestStartRetriesFailedConnectionUnmountBeforeRemount(t *testing.T) {
	// "/" is always in the mount table, so the failed unmount leaves a mount behind.
	d, m := startConnFailureDaemon(t, 1, "/")

	calls := failConnectionUntilRemount(t, d, m)
	if want := []string{"mount", "unmount failed", "unmount", "mount"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestStartUnmountsAbortedConnectionBeforeRemount(t *testing.T) {
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
	d, m := startConnFailureDaemon(t, 2, "/")
	close(m.fail)
	// The daemon unmounts once after the failure and once more in the
	// supervisor's retry, which then backs off.
	waitFor(t, d.startErr, func() bool { return len(m.recorded()) == 3 })

	// remove-repo runs in its own process, so the daemon's registry poll stops the runtime.
	ctx := context.Background()
	cliSvc, err := New(ctx, d.root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	err = cliSvc.RemoveRepo(ctx, "repo")
	if closeErr := cliSvc.Close(); err != nil || closeErr != nil {
		t.Fatalf("CLI remove-repo: %v, close: %v", err, closeErr)
	}
	waitFor(t, d.startErr, func() bool {
		d.svc.mu.Lock()
		defer d.svc.mu.Unlock()
		return len(d.svc.running) == 0
	})
	if want := []string{"mount", "unmount failed", "unmount failed", "unmount"}; !slices.Equal(m.recorded(), want) {
		t.Fatalf("calls = %q, want %q", m.recorded(), want)
	}
}

func TestMountPointHeldResolvesParentSymlinks(t *testing.T) {
	// /dev is a mount point on macOS and Linux. The mount table lists it only by its resolved path.
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "dev")
	if isMounted(path) || !mountPointHeld(path) {
		t.Fatalf("isMounted(%q) = %t, mountPointHeld = %t; want false, true", path, isMounted(path), mountPointHeld(path))
	}
}
