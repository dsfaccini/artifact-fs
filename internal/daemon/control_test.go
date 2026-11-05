package daemon

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNewRefusesASecondDaemonOnOneRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(ctx, root, discardLogger())
	if err == nil {
		second.Close()
		first.Close()
		t.Fatal("a second daemon started on a root whose daemon is running")
	}
	if !strings.Contains(err.Error(), daemonLockPath(first.root)) {
		t.Fatalf("error = %q, want it to name the daemon lock", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatalf("daemon after the first stopped: %v", err)
	}
	third.Close()
}

// A record counts only while a daemon holds the lock: a daemon that died left
// its last record behind.
func TestStatusTrustsTheRecordOnlyWhileADaemonRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ctl, err := NewController(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	cfg := model.RepoConfig{ID: "repo", Name: "repo", Branch: "refs/heads/main", Enabled: true, ConfigVersion: "v1"}
	ctl.fillPaths(&cfg)
	if err := ctl.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := ctl.registry.PutRuntime(ctx, model.RepoRuntimeRecord{RepoID: "repo", State: repoStateMounted, ServedGeneration: 3, ServedCommit: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateUnmounted || st.ServedGeneration != 0 {
		t.Fatalf("status without a daemon = %+v, want unmounted and nothing served", st)
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
	if st.State != repoStateMounted || st.ServedGeneration != 3 || st.CurrentHEADOID != strings.Repeat("a", 40) {
		t.Fatalf("status with a daemon = %+v, want the recorded mount", st)
	}
}

func TestUnmountAndRemountApplyThroughTheRunningDaemon(t *testing.T) {
	d := startDaemonWithRepo(t)
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
	if st.State != repoStateUnmounted || st.ServedGeneration != 0 {
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
	if st.State != repoStateMounted || st.ServedGeneration == 0 {
		t.Fatalf("status after remount = %+v", st)
	}
	if d.mounts.resolver() == served {
		t.Fatal("remount kept the earlier mount")
	}
}

// A daemon that never polls carries out an unmount only through the nudge.
func TestUnmountNudge(t *testing.T) {
	svc, ctl := newSyncedDaemon(t)
	if !controlSocketFits(controlSocketPath(svc.root)) {
		t.Skipf("control socket path %s does not fit sun_path", controlSocketPath(svc.root))
	}
	svc.syncInterval = time.Hour
	done := startService(t, svc)
	ctx := context.Background()
	waitFor(t, done, func() bool {
		st, err := ctl.Status(ctx, "repo")
		return err == nil && st.State == repoStateMounted
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctl.Unmount(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
}
