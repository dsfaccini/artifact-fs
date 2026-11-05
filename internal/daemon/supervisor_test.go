package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// resolverMount stands in for a successful mount and keeps the resolver that
// the mount would serve.
type resolverMount struct {
	mu       sync.Mutex
	resolved *fusefs.Resolver
}

func (m *resolverMount) mount(cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error) {
	m.mu.Lock()
	m.resolved = engine.Resolver
	m.mu.Unlock()
	return fakeMount(cfg, engine, gate, logger)
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
	ctx := context.Background()
	tmp := t.TempDir()
	d := &startedDaemon{
		mounts:  &resolverMount{},
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
	if err := svc.AddRepo(ctx, model.RepoConfig{Name: "repo", RemoteURL: d.workDir, Branch: "main", RefreshInterval: time.Hour, Enabled: true}); err != nil {
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
