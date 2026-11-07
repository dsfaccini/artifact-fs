package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/metatest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// credentialRemote stands in for a remote whose URL holds a credential that
// status must never show.
const credentialRemote = "https://user:hunter2secret@example.com/org/repo.git"

// newSyncedDaemon is a daemon, not started, with one repo cloned from a local
// repository, and a controller on its root.
func newSyncedDaemon(t *testing.T) (*Service, *Controller) {
	t.Helper()
	return newSyncedDaemonIn(t, t.TempDir())
}

// newSyncedDaemonIn is newSyncedDaemon with its state root and repositories
// under tmp.
func newSyncedDaemonIn(t *testing.T, tmp string) (*Service, *Controller) {
	t.Helper()
	return newDaemonWithRepo(t, tmp, AddRepoOptions{})
}

// newDaemonWithRepo is a daemon, not started, with one repo added with opts
// from a local repository, and a controller on its root. Its state root and
// repositories are under tmp.
func newDaemonWithRepo(t *testing.T, tmp string, opts AddRepoOptions) (*Service, *Controller) {
	t.Helper()
	ctx := context.Background()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	if err := os.WriteFile(filepath.Join(remote, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", remote, "add", "README.md")
	runCmd(t, "git", "-C", remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "main")
	root := filepath.Join(tmp, "artifact-fs")
	svc, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetMountRoot(filepath.Join(tmp, "mnt"))
	svc.mount = fakeMount
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{Name: "repo", RemoteURL: remote, Branch: "main", RefreshInterval: time.Hour, Enabled: true}, opts); err != nil {
		t.Fatal(err)
	}
	ctl, err := NewController(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	return svc, ctl
}

func requireFailure(t *testing.T, ctl *Controller, phase string, wantInError string) model.RepoRuntimeState {
	t.Helper()
	st, err := ctl.Status(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastErrorPhase != phase || st.NextRetryAt.IsZero() || st.Attempts == 0 || !strings.Contains(st.LastError, wantInError) {
		t.Fatalf("status = %+v, want a %s failure naming %q with a next retry", st, phase, wantInError)
	}
	if strings.Contains(st.LastError, "hunter2secret") {
		t.Fatalf("status leaked a credential: %q", st.LastError)
	}
	return st
}

// A store that a newer build migrated fails the mount before the FUSE mount,
// and the daemon retries it.
func TestStatusReportsAFailureBeforeTheFUSEMount(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	cfg, err := svc.registry.GetRepo(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	metatest.Exec(t, cfg.OverlayDBPath, `PRAGMA user_version = 99`)
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := requireFailure(t, ctl, phaseMount, "upgrade artifact-fs")
	if st.State != repoStateUnmounted || st.Phase != phaseMount || st.MountError != st.LastError || time.Until(st.NextRetryAt) < 20*time.Second {
		t.Fatalf("status = %+v, want unmounted in the mount phase with the mount backoff", st)
	}
}

func TestStatusRedactsAFailedFUSEMount(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return nil, errors.New("mount " + credentialRemote + " refused")
	}
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The daemon retries the mount and serves nothing meanwhile.
	if st := requireFailure(t, ctl, phaseMount, "refused"); st.Phase != phaseMount {
		t.Fatalf("phase = %q, want %q", st.Phase, phaseMount)
	}
}

// exitingMount is a mount whose FUSE server exits with err when exit closes.
type exitingMount struct {
	exit chan struct{}
	err  error
}

func (m *exitingMount) Backend() string          { return "fake" }
func (m *exitingMount) InvalidateChanged() error { return nil }

func (m *exitingMount) Join(ctx context.Context) error {
	select {
	case <-m.exit:
		return m.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *exitingMount) Unmount(context.Context) error { return m.err }

func TestStatusReportsAnExitedFUSEServer(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("connection to " + credentialRemote + " lost")}
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(mfs.exit)
	waitFor(t, nil, func() bool {
		st, err := ctl.Status(context.Background(), "repo")
		return err == nil && st.LastErrorPhase == phaseServe
	})
	if st := requireFailure(t, ctl, phaseServe, "lost"); st.State != repoStateDegraded {
		t.Fatalf("state = %q, want degraded", st.State)
	}
}

// startService runs svc's Start until the test ends and returns the channel
// that delivers Start's error.
func startService(t *testing.T, svc *Service) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		done <- svc.Start(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return done
}

func TestUnmountReportsTheDaemonsFailure(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	svc.mountPointEntry = heldMountPoint
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("unmount " + credentialRemote + ": busy")}
	t.Cleanup(func() { close(mfs.exit) })
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	done := startService(t, svc)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(context.Background(), "repo")
		return err == nil && st.State == repoStateMounted
	})
	err := ctl.Unmount(context.Background(), "repo")
	if err == nil || !strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "hunter2secret") {
		t.Fatalf("Unmount = %v, want the daemon's redacted unmount failure", err)
	}
	requireFailure(t, ctl, phaseUnmount, "busy")
}

func TestStatusReportsAFailedRegistrySync(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	done := startService(t, svc)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.State == repoStateMounted
	})
	// A row the registry cannot read fails every sync but leaves repo readable.
	db, err := meta.OpenDB(filepath.Join(svc.root, "config", "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url_redacted, branch, refresh_interval_seconds, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, created_at_ns, updated_at_ns) VALUES ('bad', 'bad', '', '', '', '', 0, '', '', '', '', '', ?, 0, 0)`, credentialRemote); err != nil {
		t.Fatal(err)
	}
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.LastErrorPhase == phaseRegistrySync
	})
	requireFailure(t, ctl, phaseRegistrySync, "Scan error")
}

func TestStatusReportsAFailedRefresh(t *testing.T) {
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
	done := startService(t, svc)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.LastErrorPhase == phaseFetch
	})
	if st := requireFailure(t, ctl, phaseFetch, "fetch"); st.LastFetchResult != string(fetchFailed) {
		t.Fatalf("fetch result = %q, want %q", st.LastFetchResult, fetchFailed)
	}
}

// A failed unmount leaves its mount behind, but a later daemon on the root
// must still start.
func TestCloseReleasesTheDaemonLockAfterAFailedUnmount(t *testing.T) {
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
	if err := svc.Close(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("Close = %v, want the unmount failure", err)
	}
	next, err := New(context.Background(), svc.root, discardLogger())
	if err != nil {
		t.Fatalf("daemon after a Close that failed to unmount: %v", err)
	}
	next.Close()
}

// A mount that serves again ends the connection failure before it, so
// mount_error clears.
func TestRemountClearsAConnectionFailure(t *testing.T) {
	// The socket path must fit sun_path, so the remount does not wait for a poll.
	tmp, err := os.MkdirTemp("/tmp", "afs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	svc, ctl := newSyncedDaemonIn(t, tmp)
	svc.syncInterval = time.Hour
	first := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return first, nil
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger)
	}
	done := startService(t, svc)
	ctx := context.Background()
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.State == repoStateMounted
	})
	close(first.exit)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.State == repoStateDegraded && st.LastErrorPhase == phaseServe
	})
	if err := ctl.Unmount(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Remount(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateMounted || st.MountError != "" || st.LastErrorPhase != "" {
		t.Fatalf("status after a remount that serves = %+v, want mounted with no mount error", st)
	}
}

// An async repo whose placeholder fails to mount reports the failure.
func TestStatusReportsAFailedPlaceholderMount(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ctl := svc.ctl()
	if err := ctl.AddRepoWithOptions(ctx, model.RepoConfig{Name: "repo", ID: "repo", RemoteURL: "https://github.com/example/repo.git", Branch: "master", Enabled: true}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	// A snapshot store a newer build migrated fails the placeholder mount.
	metatest.Exec(t, cfg.MetaDBPath, `PRAGMA user_version = 99`)
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateUnmounted || st.LastErrorPhase != phaseMount || !strings.Contains(st.LastError, "upgrade artifact-fs") || st.NextRetryAt.IsZero() {
		t.Fatalf("status after a failed placeholder mount = %+v, want unmounted with the mount failure", st)
	}
}

// A connection that fails as its runtime starts keeps its failure in status.
func TestStatusKeepsAConnectionFailureAtStart(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	close(mfs.exit)
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	// The daemon records the started runtime only after the connection failed.
	svc.runtimeStarted = func() {
		waitFor(t, nil, func() bool {
			svc.mu.Lock()
			defer svc.mu.Unlock()
			rec := svc.records["repo"]
			return rec != nil && rec.State == repoStateDegraded
		})
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateDegraded || st.LastErrorPhase != phaseServe || st.MountError != "connection lost" {
		t.Fatalf("status after the connection failed at start = %+v, want degraded with the serve failure", st)
	}
}

// An async repo's mount that serves ends the mount failure before it, so
// mount_error clears.
func TestAsyncMountClearsAnEarlierMountFailure(t *testing.T) {
	svc, ctl := newDaemonWithRepo(t, t.TempDir(), AddRepoOptions{Async: true})
	ctx := context.Background()
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	svc.fillPaths(&cfg)
	if err := os.MkdirAll(filepath.Dir(cfg.MetaDBPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// A snapshot store a newer build migrated fails the placeholder mount.
	metatest.Exec(t, cfg.MetaDBPath, `PRAGMA user_version = 99`)
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	requireFailure(t, ctl, phaseMount, "upgrade artifact-fs")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(cfg.MetaDBPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.MountError != "" || st.LastErrorPhase != "" {
		t.Fatalf("status after an async mount that serves = %+v, want no mount error", st)
	}
}

// The daemon's retry of a runtime whose connection failed ends that failure
// once the new mount serves, so mount_error clears.
func TestRetryClearsAConnectionFailure(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
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
	rt := svc.running["repo"]
	svc.mu.Unlock()
	close(first.exit)
	// The failed mount is unmounted and its failure recorded.
	waitFor(t, nil, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		rec := svc.records["repo"]
		return rt.mount == mountNone && rec != nil && rec.State == repoStateDegraded
	})
	requireFailure(t, ctl, phaseServe, "connection lost")
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateMounted || st.MountError != "" || st.LastErrorPhase != "" {
		t.Fatalf("status after a retry that serves = %+v, want mounted with no mount error", st)
	}
}

// A FUSE server that exits cleanly while the daemon serves its repo is a
// serve failure.
func TestStatusReportsACleanFUSEExit(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	mfs := &exitingMount{exit: make(chan struct{})}
	svc.mount = func(context.Context, context.Context, model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(mfs.exit)
	waitFor(t, nil, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		rec := svc.records["repo"]
		return rec != nil && rec.State == repoStateDegraded
	})
	if st := requireFailure(t, ctl, phaseServe, errFUSEServerExited.Error()); st.MountError != errFUSEServerExited.Error() {
		t.Fatalf("mount_error = %q, want %q", st.MountError, errFUSEServerExited)
	}
}

// A retry that mounts between the join goroutine releasing a cleanly exited
// FUSE mount and recording the exit clears that exit.
func TestRetryInACleanExitWindowClearsTheExit(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	first := &exitingMount{exit: make(chan struct{})}
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
	rt := svc.running["repo"]
	svc.mu.Unlock()
	var released atomic.Bool
	var retryErr error
	retried := make(chan struct{})
	joined := make(chan struct{})
	var joinOnce sync.Once
	svc.mountReleased = func() {
		released.Store(true)
		go func() {
			retryErr = svc.syncRepos(ctx)
			close(retried)
		}()
		waitFor(t, nil, func() bool {
			select {
			case <-retried:
				return true
			default:
			}
			svc.mu.Lock()
			defer svc.mu.Unlock()
			return rt.mfs != nil
		})
		// The retry mounted. When the join goroutine does not hold recordMu,
		// the retry records first, so the exit is recorded after it.
		if svc.recordMu.TryLock() {
			svc.recordMu.Unlock()
			<-retried
		}
	}
	svc.joinFinished = func() { joinOnce.Do(func() { close(joined) }) }
	close(first.exit)
	<-joined
	if !released.Load() {
		t.Fatal("the join goroutine ended without releasing a cleanly exited mount")
	}
	<-retried
	if retryErr != nil {
		t.Fatal(retryErr)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateMounted || st.LastError != "" {
		t.Fatalf("status after a retry that serves = %+v, want mounted with no failure", st)
	}
}
