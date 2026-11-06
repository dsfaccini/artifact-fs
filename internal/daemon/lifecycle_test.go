package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/metatest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// hangingUnmountMount is a mount whose connection fails once fail closes, and
// whose unmount then hangs until release closes, as unmount(2) can on a dead
// FSKit volume.
type hangingUnmountMount struct {
	fail, release chan struct{}
	entered       chan struct{}
	enterOnce     sync.Once
}

func (m *hangingUnmountMount) Backend() string { return "fake" }

func (m *hangingUnmountMount) Join(ctx context.Context) error {
	select {
	case <-m.fail:
		return errors.New("readMessage: transport failed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *hangingUnmountMount) Unmount(context.Context) error {
	m.enterOnce.Do(func() { close(m.entered) })
	<-m.release
	return nil
}

func (m *hangingUnmountMount) InvalidateChanged() error { return nil }

// An unmount that hangs on one repo's dead mount stalls only that repo: the
// daemon reports the stop it cannot carry out and still mounts another repo.
func TestHangingUnmountStallsOnlyItsRepo(t *testing.T) {
	hanging := &hangingUnmountMount{fail: make(chan struct{}), release: make(chan struct{}), entered: make(chan struct{})}
	var mounts atomic.Int32
	d := startDaemonWithMount(t, func(ctx, opCtx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return hanging, nil
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger)
	}, "")
	// Runs before the daemon's cleanup, whose stop of the repo must not meet
	// the unmount still running.
	t.Cleanup(func() {
		close(hanging.release)
		waitFor(t, nil, func() bool {
			d.svc.mu.Lock()
			defer d.svc.mu.Unlock()
			return len(d.svc.unmounting) == 0
		})
	})
	close(hanging.fail)
	select {
	case <-hanging.entered:
	case err := <-d.startErr:
		t.Fatalf("Start returned before the dead mount's unmount began: %v", err)
	}

	ctx := context.Background()
	err := d.svc.ctl().Unmount(ctx, "repo")
	if err == nil || !strings.Contains(err.Error(), errUnmountInProgress.Error()) {
		t.Fatalf("Unmount = %v, want the daemon's report that the earlier unmount has not returned", err)
	}
	if err := d.svc.ctl().AddRepo(ctx, model.RepoConfig{Name: "other", RemoteURL: d.workDir, Branch: "main", RefreshInterval: time.Hour, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, d.startErr, func() bool {
		st, err := d.svc.ctl().Status(ctx, "other")
		return err == nil && st.State == repoStateMounted
	})
}

// recordingMountTable stands in for the kernel's mount table: it lists a
// mount of type fstype at path until something unmounts path.
type recordingMountTable struct {
	mu     sync.Mutex
	path   string
	fstype string
	held   bool
	// unmountErr fails every unmount, which leaves the mount listed.
	unmountErr error
	calls      []string
}

func (m *recordingMountTable) mountPointEntry(path string) (mountEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if path == m.path && m.held {
		return mountEntry{source: "macfuse://2E221B83-5BAF-4BA5-83B4-4CBE7746AB82", fstype: m.fstype}, true
	}
	return mountEntry{}, false
}

func (m *recordingMountTable) unmountPath(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "unmount "+path)
	if m.unmountErr != nil {
		return m.unmountErr
	}
	if path == m.path {
		m.held = false
	}
	return nil
}

func (m *recordingMountTable) mount(ctx, opCtx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
	m.mu.Lock()
	m.calls = append(m.calls, "mount")
	m.mu.Unlock()
	return fakeMount(ctx, opCtx, cfg, engine, gate, logger)
}

func (m *recordingMountTable) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

// ownFUSEType is the file system type the mount table lists for one of the
// daemon's mounts on this OS.
func ownFUSEType() string {
	if runtime.GOOS == "darwin" {
		return "macfuse"
	}
	return "fuse.artifact-fs"
}

// A FUSE mount at the mount point of a repo that the daemon process before
// this one ran is the mount that process left, whatever source the mount
// table names for it. The daemon unmounts it before it mounts the repo, so
// the new mount never lands on top of it. Any other mount stays: a mount
// that is not FUSE, and a FUSE mount of a repo the earlier daemon did not
// run, such as a daemon's on another state root.
func TestStartUnmountsAMountTheEarlierDaemonLeft(t *testing.T) {
	for _, tt := range []struct {
		name       string
		recorded   string
		fstype     string
		unmountErr error
		wantCalls  func(path string) []string
	}{
		{
			name:      "left by the earlier daemon",
			recorded:  repoStateMounted,
			fstype:    ownFUSEType(),
			wantCalls: func(path string) []string { return []string{"unmount " + path, "mount"} },
		},
		{
			name:      "left by the earlier daemon while degraded",
			recorded:  repoStateDegraded,
			fstype:    ownFUSEType(),
			wantCalls: func(path string) []string { return []string{"unmount " + path, "mount"} },
		},
		{
			name:       "unmount fails",
			recorded:   repoStateMounted,
			fstype:     ownFUSEType(),
			unmountErr: errors.New("busy"),
			wantCalls:  func(path string) []string { return []string{"unmount " + path} },
		},
		{
			name:      "repo the earlier daemon had unmounted",
			recorded:  repoStateUnmounted,
			fstype:    ownFUSEType(),
			wantCalls: func(string) []string { return []string{"mount"} },
		},
		{
			name:      "repo the earlier daemon did not run",
			fstype:    ownFUSEType(),
			wantCalls: func(string) []string { return []string{"mount"} },
		},
		{
			name:      "another file system",
			recorded:  repoStateMounted,
			fstype:    "apfs",
			wantCalls: func(string) []string { return []string{"mount"} },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, ctl := newSyncedDaemon(t)
			ctx := context.Background()
			cfg, err := svc.registry.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			if tt.recorded != "" {
				if err := svc.registry.PutRuntime(ctx, model.RepoRuntimeRecord{RepoID: cfg.ID, State: tt.recorded, Phase: phaseServe}); err != nil {
					t.Fatal(err)
				}
			}
			table := &recordingMountTable{path: cfg.MountPath, fstype: tt.fstype, held: true, unmountErr: tt.unmountErr}
			svc.mountPointEntry = table.mountPointEntry
			svc.unmountPath = table.unmountPath
			svc.mount = table.mount
			done := startService(t, svc)
			want := tt.wantCalls(cfg.MountPath)
			waitFor(t, done, func() bool { return len(table.recorded()) >= len(want) })
			if tt.unmountErr != nil {
				waitFor(t, done, func() bool {
					st, err := ctl.Status(ctx, "repo")
					return err == nil && st.LastErrorPhase == phaseMount
				})
				requireFailure(t, ctl, phaseMount, "busy")
			} else {
				waitFor(t, done, func() bool {
					st, err := ctl.Status(ctx, "repo")
					return err == nil && st.State == repoStateMounted
				})
			}
			if got := table.recorded(); !slices.Equal(got, want) {
				t.Fatalf("calls = %q, want %q", got, want)
			}
		})
	}
}

// A stop reads the runtime's config under s.mu, because preparation replaces
// rt.cfg from its own goroutine while the sync loop stops the runtime. The
// race detector reports the unlocked read.
func TestStopReadsTheConfigUnderTheLock(t *testing.T) {
	svc, _ := newSyncedDaemon(t)
	svc.mountPointEntry = heldMountPoint
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("busy")}
	t.Cleanup(func() { close(mfs.exit) })
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running["repo"]
	svc.mu.Unlock()
	done := make(chan struct{})
	replaced := make(chan struct{})
	go func() {
		defer close(replaced)
		for {
			select {
			case <-done:
				return
			default:
			}
			svc.mu.Lock()
			cfg := rt.cfg
			rt.cfg = cfg
			svc.mu.Unlock()
		}
	}()
	for range 20 {
		if err := svc.unmount("repo"); err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("unmount = %v, want the busy unmount", err)
		}
	}
	close(done)
	<-replaced
}

// fakeAbandonedMount is the error of a mount whose context ended first. Its
// mount ends once ended closes.
type fakeAbandonedMount struct {
	ctxErr error
	ended  chan struct{}
}

func (e *fakeAbandonedMount) Error() string { return "mount abandoned: " + e.ctxErr.Error() }

func (e *fakeAbandonedMount) Unwrap() error { return e.ctxErr }

func (e *fakeAbandonedMount) Wait(ctx context.Context) error {
	select {
	case <-e.ended:
		return nil
	default:
	}
	select {
	case <-e.ended:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A mount that does not finish within the mount timeout no longer holds up
// the sync loop. The daemon records it as a mount failure, and mounts the
// repo again only once the abandoned mount has ended.
func TestSyncAbandonsAStalledMountAndWaitsForItToEnd(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	svc.mountTimeout = time.Millisecond
	abandoned := &fakeAbandonedMount{ended: make(chan struct{})}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			// The mount service stalls until the daemon stops waiting.
			<-ctx.Done()
			abandoned.ctxErr = ctx.Err()
			return nil, abandoned
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger)
	}
	ctx := context.Background()
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	requireFailure(t, ctl, phaseMount, "mount abandoned")
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mounts.Load(); got != 1 {
		t.Fatalf("mounted %d times while the abandoned mount had not ended, want 1", got)
	}
	close(abandoned.ended)
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 2 || st.State != repoStateMounted || st.MountError != "" {
		t.Fatalf("after the abandoned mount ended: %d mounts, status %+v; want a second mount that serves", mounts.Load(), st)
	}
}

// Every FUSE op's context derives from the runtime's, so stopping the
// runtime cancels the ops in flight and its server drains promptly.
func TestStopCancelsTheOpsOfItsMount(t *testing.T) {
	svc, _ := newSyncedDaemon(t)
	ctx := context.Background()
	var opCtx context.Context
	svc.mount = func(ctx, oc context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
		opCtx = oc
		return fakeMount(ctx, oc, cfg, engine, gate, logger)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if opCtx == nil || opCtx.Err() != nil {
		t.Fatalf("op context of the serving mount = %v, want one that is live", opCtx)
	}
	if err := svc.registry.RequestMount(ctx, "repo", false, "unmount-request"); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(opCtx.Err(), context.Canceled) {
		t.Fatalf("op context after the stop: %v, want canceled", opCtx.Err())
	}
}

// batchProcesses counts this process's git cat-file --batch children.
func batchProcesses(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "ppid=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	n := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		ppid, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && ppid == pid && strings.Contains(args, "cat-file --batch") {
			n++
		}
	}
	return n
}

// The git cat-file --batch processes that read a repo's blobs end with its
// runtime, so none outlives a removed repo.
func TestRemovedRepoLeavesNoBatchProcess(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	var engine *fusefs.Engine
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoConfig, e *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
		engine = e
		return fakeMount(ctx, opCtx, cfg, e, gate, logger)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	node, err := engine.Resolver.ResolvePath("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Hydrator.ReadBlob(ctx, engine.Repo, node.Base, 1<<20); err != nil {
		t.Fatal(err)
	}
	if batchProcesses(t) == 0 {
		t.Fatal("reading a blob started no git cat-file --batch process")
	}
	if err := ctl.RemoveRepo(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, nil, func() bool { return batchProcesses(t) == 0 })
}

// A mount retry that serves again leaves the repo degraded while its remote
// refreshes fail: the mount and the refresh each report their own state.
func TestMountRetryKeepsAFailedRefreshDegraded(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetRefresh(ctx, "repo", 20*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cfg.RemoteURL); err != nil {
		t.Fatal(err)
	}
	first := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return first, nil
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, nil, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.LastFetchResult == string(fetchFailed)
	})
	// No refresh runs after this one, so only the mount changes the state.
	if err := ctl.SetRefresh(ctx, "repo", 0, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running["repo"]
	svc.mu.Unlock()
	close(first.exit)
	waitFor(t, nil, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return rt.mount == mountNone && svc.unmounting["repo"] == nil
	})
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 2 || st.Phase != phaseServe || st.State != repoStateDegraded {
		t.Fatalf("after the mount retry served: %d mounts, status %+v; want the mount serving and the repo degraded by the failed refresh", mounts.Load(), st)
	}
}

// An async repo whose preparation failed stays failed in status when its
// mount's connection ends.
func TestFailedPreparationStaysFailedWhenItsMountEnds(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetMountRoot(filepath.Join(tmp, "mnt"))
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	ctl := svc.ctl()
	missing := filepath.Join(tmp, "missing-remote")
	if err := ctl.AddRepoWithOptions(ctx, model.RepoConfig{Name: "repo", RemoteURL: missing, Branch: "main", Enabled: true}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitForPrepareState(t, svc, "repo", model.PrepareStateFailed)
	waitFor(t, nil, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.State == string(model.PrepareStateFailed)
	})
	close(mfs.exit)
	waitFor(t, nil, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.LastErrorPhase == phaseServe
	})
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != string(model.PrepareStateFailed) || st.Phase != phaseMount {
		t.Fatalf("status after the failed preparation's mount ended = %+v, want failed in the mount phase", st)
	}
}

// A mount retry of an adopted worktree checks again that its mount point is
// empty, so it never hides native files written while it was unmounted.
func TestAdoptedMountRetryRefusesANonEmptyMountPoint(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
	runCmd(t, "git", "init", "--initial-branch=main", source)
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", source, "add", "README.md")
	runCmd(t, "git", "-C", source, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
	runCmd(t, "git", "-C", source, "worktree", "add", "--no-checkout", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	runCmd(t, "git", "--git-dir", gitDir, "read-tree", "HEAD")
	if err := os.Rename(filepath.Join(worktree, ".git"), filepath.Join(dir, "topic.gitfile")); err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, filepath.Join(dir, "state"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{Name: "adopted", GitDir: gitDir, MountPath: worktree, AdoptGitDir: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	first := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return first, nil
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running["adopted"]
	svc.mu.Unlock()
	close(first.exit)
	waitFor(t, nil, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return rt.mount == mountNone && svc.unmounting["adopted"] == nil
	})
	// A native file written while nothing is mounted.
	if err := os.WriteFile(filepath.Join(worktree, "native.txt"), []byte("native\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := svc.ctl().Status(ctx, "adopted")
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 1 || st.LastErrorPhase != phaseMount || !strings.Contains(st.LastError, "adopted mountpoint must be empty") {
		t.Fatalf("after a native file appeared: %d mounts, status %+v; want no second mount and the mount failure", mounts.Load(), st)
	}
}

// failedCleanupAbandonedMount is an abandoned mount that completed late and
// whose force-unmount failed, so its mount stays at the mount point.
type failedCleanupAbandonedMount struct{ ctxErr error }

func (e *failedCleanupAbandonedMount) Error() string { return "mount abandoned: " + e.ctxErr.Error() }

func (e *failedCleanupAbandonedMount) Unwrap() error { return e.ctxErr }

func (e *failedCleanupAbandonedMount) Wait(context.Context) error {
	return errors.New("unmounting the abandoned mount: resource busy")
}

// An abandoned mount whose cleanup failed stays at the mount point, and the
// mount table names it macfuse://<UUID> on FSKit. The daemon unmounts the
// mount point by path before it mounts again, and mounts nothing while that
// unmount fails.
func TestAbandonedMountLeftInPlaceIsUnmountedBeforeTheNextMount(t *testing.T) {
	for _, tt := range []struct {
		name       string
		unmountErr error
		wantCalls  func(path string) []string
	}{
		{name: "unmount succeeds", wantCalls: func(path string) []string { return []string{"mount", "unmount " + path, "mount"} }},
		{name: "unmount fails", unmountErr: errors.New("busy"), wantCalls: func(path string) []string { return []string{"mount", "unmount " + path} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, ctl := newSyncedDaemon(t)
			ctx := context.Background()
			cfg, err := svc.registry.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			table := &recordingMountTable{path: cfg.MountPath, fstype: ownFUSEType(), unmountErr: tt.unmountErr}
			svc.mountPointEntry = table.mountPointEntry
			svc.unmountPath = table.unmountPath
			svc.mountTimeout = time.Millisecond
			var mounts atomic.Int32
			svc.mount = func(ctx, opCtx context.Context, c model.RepoConfig, e *fusefs.Engine, g *fusefs.ReadyGate, l *slog.Logger) (fusefs.MountedFS, error) {
				if mounts.Add(1) > 1 {
					return table.mount(ctx, opCtx, c, e, g, l)
				}
				table.mu.Lock()
				table.calls = append(table.calls, "mount")
				table.mu.Unlock()
				<-ctx.Done()
				// The late mount lands and its cleanup fails: the table lists it.
				table.mu.Lock()
				table.held = true
				table.mu.Unlock()
				return nil, &failedCleanupAbandonedMount{ctxErr: ctx.Err()}
			}
			for range 2 {
				if err := svc.syncRepos(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := table.recorded(), tt.wantCalls(cfg.MountPath); !slices.Equal(got, want) {
				t.Fatalf("calls = %q, want %q", got, want)
			}
			if tt.unmountErr != nil {
				requireFailure(t, ctl, phaseMount, "busy")
			}
		})
	}
}

// opCtxAtUnmount is a live mount that records whether its op context was
// already cancelled when the daemon unmounted it.
type opCtxAtUnmount struct {
	*fakeMountedFS
	opCtx context.Context
	// cancelled holds the op context's state at the unmount; nil before it.
	cancelled atomic.Pointer[bool]
}

func (m *opCtxAtUnmount) Unmount(ctx context.Context) error {
	cancelled := m.opCtx.Err() != nil
	m.cancelled.Store(&cancelled)
	return m.fakeMountedFS.Unmount(ctx)
}

// A shutdown cancels Start's context before Close stops each repo. The ops
// in flight stay live until the unmount, as for any other stop.
func TestShutdownUnmountsBeforeCancellingOps(t *testing.T) {
	svc, _ := newSyncedDaemon(t)
	ctx := context.Background()
	var mount atomic.Pointer[opCtxAtUnmount]
	svc.mount = func(_, opCtx context.Context, _ model.RepoConfig, _ *fusefs.Engine, _ *fusefs.ReadyGate, _ *slog.Logger) (fusefs.MountedFS, error) {
		m := &opCtxAtUnmount{fakeMountedFS: &fakeMountedFS{unmounted: make(chan struct{})}, opCtx: opCtx}
		mount.Store(m)
		return m, nil
	}
	startCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Start(startCtx) }()
	waitFor(t, done, func() bool { return mount.Load() != nil })
	// SIGINT and SIGTERM cancel Start's context; the daemon command then
	// runs Close.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want context canceled", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	m := mount.Load()
	if cancelled := m.cancelled.Load(); cancelled == nil || *cancelled {
		t.Fatalf("op context cancelled at the shutdown's unmount = %v, want live (nil means no unmount)", cancelled)
	}
	if m.opCtx.Err() == nil {
		t.Fatal("op context still live after the shutdown")
	}
}

// The refresh backoff doubles from twice the interval up to fetchRetryMax,
// or up to an interval longer than that.
func TestRefreshRetrierBacksOffFromTheInterval(t *testing.T) {
	for _, tt := range []struct {
		interval time.Duration
		want     []time.Duration
	}{
		{30 * time.Second, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute}},
		{4 * time.Minute, []time.Duration{8 * time.Minute, 10 * time.Minute, 10 * time.Minute}},
		{6 * time.Minute, []time.Duration{10 * time.Minute, 10 * time.Minute}},
		{20 * time.Minute, []time.Duration{20 * time.Minute, 20 * time.Minute}},
	} {
		r := newRefreshRetrier(tt.interval)
		now := time.Unix(0, 0)
		var got []time.Duration
		for range tt.want {
			got = append(got, r.fail(now).Sub(now))
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("refresh every %s: delays %v, want %v", tt.interval, got, tt.want)
		}
	}
}

// While the dead mount's unmount has not returned, status shows the mount
// waiting for it, retried at the next sync.
func TestStatusShowsAMountWaitingForAnUnmount(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	hanging := &hangingUnmountMount{fail: make(chan struct{}), release: make(chan struct{}), entered: make(chan struct{})}
	t.Cleanup(func() { close(hanging.release) })
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return hanging, nil
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	close(hanging.fail)
	<-hanging.entered
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st := requireFailure(t, ctl, phaseMount, errUnmountInProgress.Error())
	if !st.NextRetryAt.After(st.LastErrorAt) || st.NextRetryAt.Sub(st.LastErrorAt) > registrySyncInterval {
		t.Fatalf("next retry %s after the failure, want the next sync", st.NextRetryAt.Sub(st.LastErrorAt))
	}
}

// externallyUnmounted is a live mount that someone else unmounts while the
// daemon's own unmount runs: the daemon's unmount(2) then fails with EINVAL,
// as it does for a path that no longer holds a mount, and Join returns.
type externallyUnmounted struct {
	*fakeMountedFS
	once   sync.Once
	joined chan struct{}
}

func (m *externallyUnmounted) Unmount(context.Context) error {
	m.once.Do(func() {
		close(m.unmounted)
		// The join sees the connection end while the stop runs.
		<-m.joined
	})
	return &os.PathError{Op: "unmount", Path: "/mnt/repo", Err: syscall.EINVAL}
}

// A stop whose unmount fails because the mount is already gone finishes:
// the failure counts only while the mount table lists the mount point.
func TestStopFinishesAfterAnExternalUnmount(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	joined := make(chan struct{}, 8)
	svc.joinFinished = func() { joined <- struct{}{} }
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return &externallyUnmounted{fakeMountedFS: &fakeMountedFS{unmounted: make(chan struct{})}, joined: joined}, nil
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.RequestMount(ctx, "repo", false, "unmount-request"); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	_, running := svc.running["repo"]
	svc.mu.Unlock()
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if running || st.State != repoStateUnmounted || st.LastError != "" {
		t.Fatalf("after the stop: running %t, status %+v; want the repo stopped with no failure", running, st)
	}
}

// A runtime that starts ends the mount backoff that failures before it built
// up, so its own mount retries back off from the start.
func TestRuntimeStartResetsTheMountBackoff(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return nil, errors.New("refused")
	}
	// An overlay store a newer build migrated fails the mount before any
	// runtime starts.
	metatest.Exec(t, cfg.OverlayDBPath, `PRAGMA user_version = 99`)
	for range 2 {
		if err := svc.syncRepos(ctx); err != nil {
			t.Fatal(err)
		}
		// The backoff has passed.
		svc.mu.Lock()
		svc.mountRetries[cfg.ID].next = time.Time{}
		svc.mu.Unlock()
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(cfg.OverlayDBPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	// The runtime starts, and its first mount fails; the next one backs off.
	for range 2 {
		if err := svc.syncRepos(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st := requireFailure(t, ctl, phaseMount, "refused")
	if delay := st.NextRetryAt.Sub(st.LastErrorAt); delay > mountRetryMin+time.Second {
		t.Fatalf("mount retry delay after the runtime started = %s, want %s", delay, mountRetryMin)
	}
}
