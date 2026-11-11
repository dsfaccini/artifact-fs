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
	"github.com/cloudflare/artifact-fs/internal/gittest"
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
	return newSyncedDaemonIn(t, shortTempDir(t))
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
	svc.mount = fakeMount
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Hour}, Enabled: true}, opts); err != nil {
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
	t.Parallel()
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
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
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

func (m *exitingMount) Backend() string                     { return "fake" }
func (m *exitingMount) InvalidateChanged() error            { return nil }
func (m *exitingMount) OpenFileGenerations() []int64        { return nil }
func (m *exitingMount) OpenFilePIDs([]int64) map[uint32]int { return nil }

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
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("connection to " + credentialRemote + " lost")}
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	// The join records the serve failure on its own goroutine, so the test
	// reads status once the join ended.
	joined := make(chan struct{})
	var joinOnce sync.Once
	svc.joinFinished = func() { joinOnce.Do(func() { close(joined) }) }
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(mfs.exit)
	<-joined
	if st := requireFailure(t, ctl, phaseServe, "lost"); st.State != repoStateDegraded {
		t.Fatalf("state = %q, want degraded", st.State)
	}
}

// The join records a lost connection with the state it leaves in one write,
// so no status shows the failure with the state from before it.
func TestStatusShowsALostConnectionWithTheStateItLeaves(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	recorded := make(chan model.RepoRuntimeState, 1)
	svc.joinRecorded = func() {
		st, err := ctl.Status(ctx, "repo")
		if err != nil {
			t.Error(err)
		}
		recorded <- st
	}
	close(mfs.exit)
	if st := <-recorded; st.LastErrorPhase != phaseServe || st.State != repoStateDegraded || st.Phase != phaseMount {
		t.Fatalf("status once the lost connection was recorded = %+v, want degraded in the mount phase with the serve failure", st)
	}
}

// startService runs svc's Start until the test ends and returns the channel
// that delivers Start's error. svc never polls the registry.
func startService(t *testing.T, svc *Service) <-chan error {
	t.Helper()
	svc.syncInterval = time.Hour
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

// syncAndFinishStops runs one registry sync on svc and waits for each stop
// it started, which runs off the sync loop.
func syncAndFinishStops(t *testing.T, svc *Service) {
	t.Helper()
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	var stops []*unmountOp
	for _, rt := range svc.running {
		if rt.stop != nil {
			stops = append(stops, rt.stop)
		}
	}
	svc.mu.Unlock()
	for _, op := range stops {
		<-op.done
	}
}

func TestUnmountReportsTheDaemonsFailure(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	svc.mountPointEntry = heldMountPoint
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("unmount " + credentialRemote + ": busy")}
	t.Cleanup(func() { close(mfs.exit) })
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
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
	// A failed unmount leaves the repo mounted.
	if st := requireFailure(t, ctl, phaseUnmount, "busy"); st.State != repoStateMounted {
		t.Fatalf("status = %+v, want state %s", st, repoStateMounted)
	}
}

// A stop whose drain timed out has unmounted the repo, so status reports the
// repo unmounted in the write that records the failure.
func TestUnmountReportsATimedOutDrainUnmounted(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	// The FUSE server outlives its unmount, which holds the drain.
	mfs := &exitingMount{exit: make(chan struct{})}
	t.Cleanup(func() { close(mfs.exit) })
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	// The drain's deadline passes while the marker exists. Removing it lets
	// the stop of Close drain once the server exits.
	expired := filepath.Join(t.TempDir(), "drain-expired")
	svc.drainContext = func() (context.Context, context.CancelFunc) {
		return gittest.ExpireOnFile(context.Background(), expired)
	}
	done := startService(t, svc)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.State == repoStateMounted
	})
	if err := os.WriteFile(expired, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Unmount(ctx, "repo"); err == nil || !strings.Contains(err.Error(), "timed out draining") {
		t.Fatalf("Unmount = %v, want the drain timeout", err)
	}
	st := requireFailure(t, ctl, phaseUnmount, "timed out draining")
	if st.State != repoStateUnmounted || st.Phase != phaseUnmount {
		t.Fatalf("status = %+v, want state %s in phase %s", st, repoStateUnmounted, phaseUnmount)
	}
	if err := os.Remove(expired); err != nil {
		t.Fatal(err)
	}
}

func TestStatusReportsAFailedRegistrySync(t *testing.T) {
	t.Parallel()
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
	wake(svc)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.LastErrorPhase == phaseRegistrySync
	})
	requireFailure(t, ctl, phaseRegistrySync, "Scan error")
}

// A failing retry rewrites FETCH_HEAD after the recorded failure, and status
// must not read it as a later successful fetch, also before the daemon
// records the retry.
func TestStatusReportsAFailedRefresh(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetRefresh(ctx, "repo", model.RefreshPolicy{RefreshInterval: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cfg.RemoteURL); err != nil {
		t.Fatal(err)
	}
	git := &pausedRefreshGit{GitStore: svc.git, retried: make(chan struct{})}
	svc.git = git
	done := startService(t, svc)
	select {
	case <-git.retried:
	case err := <-done:
		t.Fatalf("Start returned before the refresh retried: %v", err)
	}
	st := requireFailure(t, ctl, phaseFetch, "fetch")
	fi, err := os.Stat(filepath.Join(cfg.GitDir, "FETCH_HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().After(st.LastErrorAt) {
		t.Fatalf("FETCH_HEAD written at %v, want the retry's write after the recorded failure at %v", fi.ModTime(), st.LastErrorAt)
	}
	if st.LastFetchResult != string(fetchFailed) {
		t.Fatalf("fetch result = %q, want %q", st.LastFetchResult, fetchFailed)
	}
}

// pausedRefreshGit runs every refresh fetch, and pauses each one after the
// first, once its git has run, until its context ends. The refresh loop
// records a fetch's outcome before it retries, so retried closes once the
// first outcome is recorded and the retry's git has written FETCH_HEAD, and
// before the daemon records the retry.
type pausedRefreshGit struct {
	model.GitStore
	fetches   atomic.Int32
	retried   chan struct{}
	retryOnce sync.Once
}

func (g *pausedRefreshGit) FetchNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	err := g.GitStore.FetchNonInteractive(ctx, repo)
	if g.fetches.Add(1) == 1 {
		return err
	}
	g.retryOnce.Do(func() { close(g.retried) })
	<-ctx.Done()
	return err
}

// A fetch command records nothing, so status reads its result from
// FETCH_HEAD: a fetch that succeeded is ok, and the FETCH_HEAD a failed one
// emptied is no fetch.
func TestStatusReportsOnlyASuccessfulFetchCommand(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	fetchHead := filepath.Join(cfg.GitDir, "FETCH_HEAD")
	if err := ctl.FetchNow(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(fetchHead)
	if err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastFetchResult != string(fetchOK) || !st.LastFetchAt.Equal(fi.ModTime()) {
		t.Fatalf("last fetch = %q at %v, want %q at %v", st.LastFetchResult, st.LastFetchAt, fetchOK, fi.ModTime())
	}

	if err := os.RemoveAll(cfg.RemoteURL); err != nil {
		t.Fatal(err)
	}
	if err := ctl.FetchNow(ctx, "repo"); err == nil {
		t.Fatal("fetch from a removed remote succeeded")
	}
	if fi, err := os.Stat(fetchHead); err != nil || !fi.ModTime().After(st.LastFetchAt) {
		t.Fatalf("FETCH_HEAD after the failed fetch = %v, want it written after %v", err, st.LastFetchAt)
	}
	st, err = ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastFetchResult != string(fetchNever) || !st.LastFetchAt.IsZero() {
		t.Fatalf("last fetch after a failed fetch = %q at %v, want %q", st.LastFetchResult, st.LastFetchAt, fetchNever)
	}
}

// The daemon refreshes in the background, so a remote that asks for
// credentials fails the refresh instead of prompting.
func TestRefreshDoesNotPrompt(t *testing.T) {
	askpassRuns := gittest.IsolatePrompts(t)
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "--git-dir", cfg.GitDir, "remote", "set-url", "origin", gittest.UnauthorizedRemote(t))
	if err := ctl.SetRefresh(ctx, "repo", model.RefreshPolicy{RefreshInterval: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	done := startService(t, svc)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.LastErrorPhase == phaseFetch
	})
	requireFailure(t, ctl, phaseFetch, "terminal prompts disabled")
	gittest.RequireNoPrompt(t, askpassRuns)
}

// A failed unmount leaves its mount behind, but a later daemon on the root
// must still start.
func TestCloseReleasesTheDaemonLockAfterAFailedUnmount(t *testing.T) {
	t.Parallel()
	svc, _ := newSyncedDaemon(t)
	svc.mountPointEntry = heldMountPoint
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("busy")}
	t.Cleanup(func() { close(mfs.exit) })
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
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
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	first := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger, redact func(string) string) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return first, nil
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger, redact)
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

// A connection that fails as its runtime starts keeps its failure in status.
func TestStatusKeepsAConnectionFailureAtStart(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	close(mfs.exit)
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	// The runtime finishes starting only after the join recorded the
	// failed connection.
	joined := make(chan struct{})
	var joinOnce sync.Once
	svc.joinFinished = func() { joinOnce.Do(func() { close(joined) }) }
	svc.runtimeStarted = func() { <-joined }
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

// statusAtConfigureGit reads status each time the daemon configures Git for
// a mount.
type statusAtConfigureGit struct {
	model.GitStore
	status func()
}

func (g *statusAtConfigureGit) ConfigureStatusOptimization(ctx context.Context, repo model.RepoConfig, stateRoot string) error {
	g.status()
	return g.GitStore.ConfigureStatusOptimization(ctx, repo, stateRoot)
}

// Status reports a mount once the mount serves. The mount point serves
// while the daemon configures Git for it.
func TestStatusReportsAMountWhileTheDaemonConfiguresGit(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	var reads []model.RepoRuntimeState
	svc.git = &statusAtConfigureGit{GitStore: svc.git, status: func() {
		st, err := ctl.Status(ctx, "repo")
		if err != nil {
			t.Error(err)
		}
		reads = append(reads, st)
	}}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 {
		t.Fatalf("the daemon configured Git %d times for one mount, want once", len(reads))
	}
	if st := reads[0]; st.State != repoStateMounted || st.Phase != phaseServe || st.ServedGeneration == 0 {
		t.Fatalf("status while the daemon configures Git = %+v, want mounted and serving", st)
	}
}

// An async repo's mount that serves ends the mount failure before it, so
// mount_error clears.
func TestAsyncMountClearsAnEarlierMountFailure(t *testing.T) {
	t.Parallel()
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
	if st := requireFailure(t, ctl, phaseMount, "upgrade artifact-fs"); st.State != repoStateUnmounted {
		t.Fatalf("state after a failed placeholder mount = %q, want %s", st.State, repoStateUnmounted)
	}
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
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	first := &exitingMount{exit: make(chan struct{}), err: errors.New("connection lost")}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger, redact func(string) string) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return first, nil
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger, redact)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running["repo"]
	svc.mu.Unlock()
	joined := make(chan struct{})
	var joinOnce sync.Once
	svc.joinFinished = func() { joinOnce.Do(func() { close(joined) }) }
	close(first.exit)
	// The failure is recorded and the failed mount unmounted.
	<-joined
	waitFor(t, nil, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return rt.mount == mountNone
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
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	mfs := &exitingMount{exit: make(chan struct{})}
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
		return mfs, nil
	}
	joined := make(chan struct{})
	var joinOnce sync.Once
	svc.joinFinished = func() { joinOnce.Do(func() { close(joined) }) }
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(mfs.exit)
	<-joined
	if st := requireFailure(t, ctl, phaseServe, errFUSEServerExited.Error()); st.MountError != errFUSEServerExited.Error() {
		t.Fatalf("mount_error = %q, want %q", st.MountError, errFUSEServerExited)
	}
}

// A retry that mounts between the join goroutine releasing a cleanly exited
// FUSE mount and recording the exit clears that exit.
func TestRetryInACleanExitWindowClearsTheExit(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	ctx := context.Background()
	first := &exitingMount{exit: make(chan struct{})}
	var mounts atomic.Int32
	svc.mount = func(ctx, opCtx context.Context, cfg model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger, redact func(string) string) (fusefs.MountedFS, error) {
		if mounts.Add(1) == 1 {
			return first, nil
		}
		return fakeMount(ctx, opCtx, cfg, engine, gate, logger, redact)
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
