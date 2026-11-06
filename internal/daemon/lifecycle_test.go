package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
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
	// Runs before the daemon's cleanup, which stops every repo.
	t.Cleanup(func() { close(hanging.release) })
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
// mount with source at path until something unmounts path.
type recordingMountTable struct {
	mu     sync.Mutex
	path   string
	source string
	held   bool
	// unmountErr fails every unmount, which leaves the mount listed.
	unmountErr error
	calls      []string
}

func (m *recordingMountTable) mountPointSource(path string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if path == m.path && m.held {
		return m.source, true
	}
	return "", false
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

// A mount that a daemon process which stopped without unmounting left at a
// repo's mount point is unmounted before the daemon mounts the repo, so the
// new mount never lands on top of it. Any other mount stays.
func TestSyncUnmountsAMountAnEarlierDaemonLeft(t *testing.T) {
	for _, tt := range []struct {
		name       string
		source     string
		unmountErr error
		wantCalls  func(path string) []string
		wantState  string
	}{
		{
			name:      "left by a daemon",
			source:    fusefs.FSName("repo"),
			wantCalls: func(path string) []string { return []string{"unmount " + path, "mount"} },
			wantState: repoStateMounted,
		},
		{
			name:       "unmount fails",
			source:     fusefs.FSName("repo"),
			unmountErr: errors.New("busy"),
			wantCalls:  func(path string) []string { return []string{"unmount " + path} },
			wantState:  repoStateUnmounted,
		},
		{
			name:      "another file system",
			source:    "/dev/disk3s1",
			wantCalls: func(string) []string { return []string{"mount"} },
			wantState: repoStateMounted,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, ctl := newSyncedDaemon(t)
			ctx := context.Background()
			cfg, err := svc.registry.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			table := &recordingMountTable{path: cfg.MountPath, source: tt.source, held: true, unmountErr: tt.unmountErr}
			svc.mountPointSource = table.mountPointSource
			svc.unmountPath = table.unmountPath
			svc.mount = table.mount
			if err := svc.syncRepos(ctx); err != nil {
				t.Fatal(err)
			}
			if got, want := table.recorded(), tt.wantCalls(cfg.MountPath); !slices.Equal(got, want) {
				t.Fatalf("calls = %q, want %q", got, want)
			}
			st, err := ctl.Status(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tt.wantState {
				t.Fatalf("state = %q, want %q", st.State, tt.wantState)
			}
			if tt.unmountErr != nil {
				requireFailure(t, ctl, phaseMount, "busy")
			}
		})
	}
}

// A stop reads the runtime's config under s.mu, because preparation replaces
// rt.cfg from its own goroutine while the sync loop stops the runtime. The
// race detector reports the unlocked read.
func TestStopReadsTheConfigUnderTheLock(t *testing.T) {
	svc, _ := newSyncedDaemon(t)
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
