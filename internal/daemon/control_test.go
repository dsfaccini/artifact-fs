package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// requireControlSocket fails t unless root's control socket path fits
// sun_path, so a daemon on root takes CLI nudges.
func requireControlSocket(t *testing.T, root string) {
	t.Helper()
	if path := controlSocketPath(root); !controlSocketFits(path) {
		t.Fatalf("control socket path %s does not fit sun_path", path)
	}
}

// shortTempDir is a TempDir short enough for a state root whose control
// socket path fits sun_path. A t.TempDir() on macOS lies under /var/folders,
// and the daemon there skips the socket, so a CLI nudge never reaches it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "afs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	return tmp
}

func TestNewRefusesASecondDaemonOnOneRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	first, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	// The second daemon waits out daemonLockWait, which the bubble's clock
	// passes at once.
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		second, err := New(ctx, root, discardLogger())
		if err == nil {
			second.Close()
			first.Close()
			t.Fatal("a second daemon started on a root whose daemon is running")
		}
		if waited := time.Since(start); waited < daemonLockWait {
			t.Fatalf("second daemon gave up after %s, want it to wait %s for the lock", waited, daemonLockWait)
		}
		if !strings.Contains(err.Error(), daemonLockPath(first.root)) {
			t.Fatalf("error = %q, want it to name the daemon lock", err)
		}
	})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// A git that another test forks can share the lock until it execs, so
	// this daemon waits for the lock on the real clock.
	third, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatalf("daemon after the first stopped: %v", err)
	}
	third.Close()
}

// A record counts only while a daemon holds the lock: a daemon that died left
// its last record behind.
func TestStatusTrustsTheRecordOnlyWhileADaemonRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	ctl, err := NewController(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo"}, RepoSpec: model.RepoSpec{Branch: "refs/heads/main"}, Enabled: true, ConfigVersion: "v1"}
	ctl.fillPaths(&cfg)
	if err := ctl.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := ctl.registry.PutRuntime(ctx, model.RepoRuntimeRecord{RepoID: "repo", State: repoStateMounted, Phase: phaseServe, ServedGeneration: 3, ServedCommit: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateUnmounted || st.Phase != "" || st.ServedGeneration != 0 {
		t.Fatalf("status without a daemon = %+v, want unmounted, no phase and nothing served", st)
	}
	lock, err := acquireDaemonLock(ctl.root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	st, err = ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateMounted || st.Phase != phaseServe || st.ServedGeneration != 3 || st.CurrentHEADOID != strings.Repeat("a", 40) {
		t.Fatalf("status with a daemon = %+v, want the recorded mount", st)
	}
}

func TestUnmountAndRemountApplyThroughTheRunningDaemon(t *testing.T) {
	t.Parallel()
	d := startDaemonWithRepo(t)
	// The daemon never polls, so only the nudge carries out the requests.
	requireControlSocket(t, d.root)
	ctx := context.Background()
	ctl, err := NewController(ctx, d.root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	served := d.mounts.resolver()

	if err := ctl.Unmount(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateUnmounted || st.Phase != phaseUnmount || st.ServedGeneration != 0 {
		t.Fatalf("status after unmount = %+v", st)
	}
	d.svc.mu.Lock()
	_, running := d.svc.running["repo"]
	d.svc.mu.Unlock()
	if running {
		t.Fatal("the daemon still runs the repo after unmount")
	}

	if err := ctl.Remount(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	st, err = ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateMounted || st.Phase != phaseServe || st.ServedGeneration == 0 {
		t.Fatalf("status after remount = %+v", st)
	}
	if d.mounts.resolver() == served {
		t.Fatal("remount kept the earlier mount")
	}
}

// A daemon that never polls carries out an unmount only through the nudge.
func TestUnmountNudge(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	requireControlSocket(t, svc.root)
	done := startService(t, svc)
	ctx := context.Background()
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.State == repoStateMounted
	})
	if err := ctl.Unmount(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
}

// Remounting a repo the daemon serves replaces its mount.
func TestRemountReplacesAServedMount(t *testing.T) {
	t.Parallel()
	d := startDaemonWithRepo(t)
	ctx := context.Background()
	ctl, err := NewController(ctx, d.root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	served := d.mounts.resolver()
	d.svc.mu.Lock()
	before := d.svc.running["repo"]
	d.svc.mu.Unlock()

	if err := ctl.Remount(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	cfg, err := ctl.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := ctl.registry.GetRuntime(ctx, "repo")
	if err != nil || !ok {
		t.Fatalf("runtime record: %v, %v", ok, err)
	}
	if cfg.MountRequest == "" || rec.MountRequest != cfg.MountRequest || rec.State != repoStateMounted {
		t.Fatalf("record after remount = %+v, want mounted under request %q", rec, cfg.MountRequest)
	}
	d.svc.mu.Lock()
	after := d.svc.running["repo"]
	d.svc.mu.Unlock()
	if after == nil || after == before || d.mounts.resolver() == served {
		t.Fatal("remount kept the earlier mount")
	}
}

// heldDaemonLock takes the daemon lock on ctl's root, as a daemon that never
// syncs would, until the test ends.
func heldDaemonLock(t *testing.T, ctl *Controller) {
	t.Helper()
	lock, err := acquireDaemonLock(ctl.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
}

func newRegisteredController(t *testing.T) *Controller {
	t.Helper()
	ctx := context.Background()
	ctl, err := NewController(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo"}, RepoSpec: model.RepoSpec{Branch: "refs/heads/main"}, Enabled: true, ConfigVersion: "v1"}
	ctl.fillPaths(&cfg)
	if err := ctl.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return ctl
}

// A request the daemon has not carried out when the wait ends stays in the
// registry, and the error says so.
func TestUnmountReportsATimeoutThatLeavesTheRequestPending(t *testing.T) {
	t.Parallel()
	ctl := newRegisteredController(t)
	heldDaemonLock(t, ctl)
	// The wait starts after the intent is written, so expiring it at once
	// skips no earlier step.
	ctl.requestTimeout = time.Nanosecond
	err := ctl.Unmount(context.Background(), "repo")
	if err == nil || !strings.Contains(err.Error(), "did not carry out the request") || !strings.Contains(err.Error(), "stays pending") {
		t.Fatalf("Unmount = %v, want the timeout and the pending request", err)
	}
	cfg, err := ctl.registry.GetRepo(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.MountRequest == "" {
		t.Fatalf("registry after the timeout = enabled %t, request %q; want the pending unmount", cfg.Enabled, cfg.MountRequest)
	}
}

func TestUnmountReportsASupersedingRequest(t *testing.T) {
	t.Parallel()
	ctl := newRegisteredController(t)
	heldDaemonLock(t, ctl)
	ctx := context.Background()
	errCh := make(chan error, 1)
	go func() { errCh <- ctl.Unmount(ctx, "repo") }()
	waitFor(t, errCh, func() bool {
		cfg, err := ctl.registry.GetRepo(ctx, "repo")
		return err == nil && cfg.MountRequest != ""
	})
	if err := ctl.registry.RequestMount(ctx, "repo", true, "later"); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("Unmount = %v, want the superseding request", err)
	}
}

// The control socket only wakes the daemon early, so a socket path the daemon
// cannot take over leaves it on the registry poll.
func TestStartRunsWithoutTheControlSocket(t *testing.T) {
	t.Parallel()
	svc, ctl := newSyncedDaemon(t)
	// Otherwise the daemon skips the socket before the step under test.
	requireControlSocket(t, svc.root)
	// A directory that is not empty cannot be removed to make room for the socket.
	if err := os.MkdirAll(filepath.Join(controlSocketPath(svc.root), "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	done := startService(t, svc)
	waitFor(t, done, func() bool {
		st, err := ctl.Status(context.Background(), "repo")
		return err == nil && st.State == repoStateMounted
	})
}
