package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	ctx := context.Background()
	tmp := t.TempDir()
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
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{Name: "repo", RemoteURL: remote, Branch: "main", RefreshInterval: time.Hour, Enabled: true}); err != nil {
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
	if st.State != repoStateUnmounted || st.MountError != st.LastError || time.Until(st.NextRetryAt) < 20*time.Second {
		t.Fatalf("status = %+v, want unmounted with the mount backoff", st)
	}
}

func TestStatusRedactsAFailedFUSEMount(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	svc.mount = func(model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
		return nil, errors.New("mount " + credentialRemote + " refused")
	}
	if err := svc.syncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireFailure(t, ctl, phaseMount, "refused")
}

// exitingMount is a mount whose FUSE server exits with err when exit closes.
type exitingMount struct {
	exit chan struct{}
	err  error
}

func (m *exitingMount) Backend() string { return "fake" }

func (m *exitingMount) Join(ctx context.Context) error {
	select {
	case <-m.exit:
		return m.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *exitingMount) Unmount() error { return m.err }

func TestStatusReportsAnExitedFUSEServer(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("connection to " + credentialRemote + " lost")}
	svc.mount = func(model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
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
	mfs := &exitingMount{exit: make(chan struct{}), err: errors.New("unmount " + credentialRemote + ": busy")}
	t.Cleanup(func() { close(mfs.exit) })
	svc.mount = func(model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
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
	if st := requireFailure(t, ctl, phaseFetch, "fetch"); st.LastFetchResult != fetchResultFailed {
		t.Fatalf("fetch result = %q, want %q", st.LastFetchResult, fetchResultFailed)
	}
}
