package daemon

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/cloudflare/artifact-fs/internal/watcher"
	"github.com/dsfaccini/fuse/fuseops"
)

type headReconcileFixture struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	workDir string
	oldOID  string
	nextOID string
	lastOID string
	oldGen  int64
	svc     *Service
	rt      *repoRuntime
	logs    *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

type delayedUnmountFailure struct {
	started chan struct{}
	release chan struct{}
}

func (m *delayedUnmountFailure) Backend() string                     { return "fake" }
func (m *delayedUnmountFailure) InvalidateChanged() error            { return nil }
func (m *delayedUnmountFailure) OpenFileGenerations() []int64        { return nil }
func (m *delayedUnmountFailure) OpenFilePIDs([]int64) map[uint32]int { return nil }
func (m *delayedUnmountFailure) Join(context.Context) error          { return nil }

func (m *delayedUnmountFailure) Unmount(context.Context) error {
	close(m.started)
	<-m.release
	return errors.New("busy")
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newHeadReconcileFixture(t *testing.T) *headReconcileFixture {
	t.Helper()
	return newHeadReconcileFixtureWithBackoff(t, time.Millisecond)
}

// newHeadReconcileFixtureWithBackoff starts the HEAD reconcile loop with backoff
// as its first retry delay.
func newHeadReconcileFixtureWithBackoff(t *testing.T, backoff time.Duration) *headReconcileFixture {
	t.Helper()
	f := newIdleHeadReconcileFixture(t, backoff)
	f.rt.workers.Go(func() {
		f.svc.headReconcileLoop(f.rt)
	})
	return f
}

// newIdleHeadReconcileFixture builds a headReconcileFixture whose runtime runs
// no worker yet.
func newIdleHeadReconcileFixture(t *testing.T, backoff time.Duration) *headReconcileFixture {
	t.Helper()
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	runCmd(t, "git", "init", "--initial-branch", "main", workDir)
	readme := filepath.Join(workDir, "README.md")
	commit := func(content, message string) string {
		t.Helper()
		if err := os.WriteFile(readme, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runCmd(t, "git", "-C", workDir, "add", "README.md")
		runCmd(t, "git", "-C", workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", message)
		return strings.TrimSpace(runCmdOutput(t, "git", "-C", workDir, "rev-parse", "HEAD"))
	}
	oldOID := commit("old\n", "old")
	nextOID := commit("next\n", "next")
	lastOID := commit("last\n", "last")
	runCmd(t, "git", "-C", workDir, "update-ref", "refs/heads/main", oldOID)

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			ID:            "repro",
			Name:          "repro",
			GitDir:        filepath.Join(workDir, ".git"),
			OverlayDir:    filepath.Join(root, "overlay"),
			OverlayDBPath: filepath.Join(root, "overlay", "meta.sqlite"),
			MetaDBPath:    filepath.Join(root, "snapshot.sqlite"),
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(logs), nil))
	git := gitstore.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	nodes, err := git.BuildTreeIndex(ctx, cfg, oldOID)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	oldGen, err := snap.PublishGeneration(ctx, oldOID, "main", nodes)
	if err != nil {
		cancel()
		_ = snap.Close()
		t.Fatal(err)
	}
	ov, err := overlay.New(ctx, cfg.RepoPaths)
	if err != nil {
		cancel()
		_ = snap.Close()
		t.Fatal(err)
	}
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(oldGen)
	rt := &repoRuntime{
		cfg:         cfg,
		ctx:         ctx,
		cancel:      cancel,
		snapshot:    snap,
		overlay:     ov,
		resolver:    resolver,
		headChanges: make(chan struct{}, 1),
		watcher:     watcher.New(),
		head:        servedHead{oid: oldOID, ref: "main", gen: oldGen},
		fetch:       fetchStatus{state: fetchNever},
		mount:       mountNone,
	}
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	svc := &Service{
		core:                 &core{root: root, logger: logger, git: git, registry: reg},
		running:              map[model.RepoID]*repoRuntime{cfg.ID: rt},
		unmounting:           map[model.RepoID]*unmountOp{},
		mountPointEntry:      heldMountPoint,
		records:              map[model.RepoID]*model.RepoRuntimeRecord{},
		headReconcileBackoff: backoff,
	}
	f := &headReconcileFixture{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		workDir: workDir,
		oldOID:  oldOID,
		nextOID: nextOID,
		lastOID: lastOID,
		oldGen:  oldGen,
		svc:     svc,
		rt:      rt,
		logs:    logs,
	}
	t.Cleanup(func() {
		cancel()
		rt.workers.Wait()
		rt.headMu.Lock()
		_ = rt.snapshot.Close()
		_ = rt.overlay.Close()
		rt.headMu.Unlock()
		git.Close()
	})
	return f
}

func (f *headReconcileFixture) updateMain(oid string) {
	f.t.Helper()
	runCmd(f.t, "git", "-C", f.workDir, "update-ref", "refs/heads/main", oid)
}

// moveMain points main at oid from a goroutine other than the test's.
func (f *headReconcileFixture) moveMain(oid string) {
	if out, err := exec.Command("git", "-C", f.workDir, "update-ref", "refs/heads/main", oid).CombinedOutput(); err != nil {
		f.t.Errorf("update-ref main %s: %v\n%s", oid, err, out)
	}
}

func (f *headReconcileFixture) signal() {
	f.svc.signalHEADReconcile(f.rt)
}

func (f *headReconcileFixture) waitForLog(message string) {
	f.t.Helper()
	waitFor(f.t, nil, func() bool { return strings.Contains(f.logs.String(), message) })
}

func (f *headReconcileFixture) waitForHead(oid, ref string, generation int64) {
	f.t.Helper()
	waitFor(f.t, nil, func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		return f.rt.head.oid == oid &&
			f.rt.head.ref == ref &&
			f.rt.head.gen == generation &&
			f.rt.resolver.Generation() == generation
	})
}

// steppedHEADGit resolves HEAD in git. Around its nth resolve, counting from
// one, it runs before[n-1] and after[n-1] where they are set.
type steppedHEADGit struct {
	model.GitStore
	calls         atomic.Int32
	before, after []func()
}

func (g *steppedHEADGit) ResolveHEAD(ctx context.Context, repo model.RepoConfig) (string, string, error) {
	n := int(g.calls.Add(1))
	if n <= len(g.before) && g.before[n-1] != nil {
		g.before[n-1]()
	}
	oid, ref, err := g.GitStore.ResolveHEAD(ctx, repo)
	if n <= len(g.after) && g.after[n-1] != nil {
		g.after[n-1]()
	}
	return oid, ref, err
}

// A reconcile reads HEAD after the signal that started it. When HEAD moves on
// before any poll could see the state the reconcile served, the daemon still
// ends up serving HEAD.
func TestHeadReconcileServesAHEADThatMovedDuringItsRead(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.svc.git = &steppedHEADGit{
		GitStore: f.svc.git,
		before:   []func(){func() { f.moveMain(f.nextOID) }},
		after:    []func(){func() { f.moveMain(f.lastOID) }},
	}
	f.signal()
	f.waitForHead(f.lastOID, "main", f.oldGen+2)
}

// A reconcile serves a HEAD no poll saw, and HEAD then returns to the state
// the watcher polled last. The next poll must still signal that return.
func TestHEADWatcherSignalsAReturnToTheStateItPolledLast(t *testing.T) {
	t.Parallel()
	f := newIdleHeadReconcileFixture(t, time.Millisecond)
	movedBack := make(chan struct{})
	f.svc.git = &steppedHEADGit{
		GitStore: f.svc.git,
		// The reconcile of the watcher's startup call reads next, which no
		// poll sees.
		before: []func(){func() { f.moveMain(f.nextOID) }},
		// Once the reconcile's check of HEAD read next, HEAD returns to old,
		// the state the watcher took at startup.
		after: []func(){nil, func() {
			f.moveMain(f.oldOID)
			close(movedBack)
		}},
	}
	polls := make(chan time.Time)
	f.svc.headPollTicks = polls
	f.rt.cfg.RemoteRefreshDisabled = true
	f.svc.startRepoBackground(f.rt)
	<-movedBack
	polls <- time.Now()
	f.waitForHead(f.oldOID, "main", f.oldGen+2)
}

// A reconcile that starts once its runtime is stopping changes nothing. The
// loop must not take that for a HEAD it still has to serve, or it reconciles
// again and again and never sees the stop cancel it.
func TestHeadReconcileLoopEndsWhenItsRuntimeStopsMidReconcile(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.rt.headMu.Lock()
	f.signal()
	// The loop took the signal and waits for headMu in reconcileHEAD.
	waitFor(t, nil, func() bool { return len(f.rt.headChanges) == 0 })
	f.svc.mu.Lock()
	f.rt.stopping = true
	f.svc.mu.Unlock()
	// Cancel first. A check of HEAD that ran before the cancel would find
	// HEAD served and end the loop with or without the guard.
	f.cancel()
	f.rt.headMu.Unlock()
	f.rt.workers.Wait()
}

func TestHeadReconcileRetriesTransientResolveFailureWithoutAnotherSignal(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	unavailable := f.rt.cfg.GitDir + ".unavailable"
	if err := os.Rename(f.rt.cfg.GitDir, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, f.rt.cfg.GitDir)
		}
	})

	f.signal()
	f.waitForLog("HEAD resolve failed")
	if err := os.Rename(unavailable, f.rt.cfg.GitDir); err != nil {
		t.Fatal(err)
	}
	f.waitForHead(f.nextOID, "main", f.oldGen+1)
}

func TestHeadReconcileRetriesTransientSnapshotPublishFailureWithoutAnotherSignal(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	snapshotPath := filepath.Join(filepath.Dir(f.rt.cfg.OverlayDir), "snapshot.sqlite")
	if err := f.rt.snapshot.Close(); err != nil {
		t.Fatal(err)
	}

	f.signal()
	f.waitForLog("snapshot publish failed")
	recovered, err := snapshot.New(f.ctx, snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	f.rt.headMu.Lock()
	f.rt.snapshot = recovered
	f.rt.resolver.Snapshot = recovered
	f.rt.headMu.Unlock()
	f.waitForHead(f.nextOID, "main", f.oldGen+1)
}

func TestHeadReconcileRetriesTransientTreeBuildFailureWithoutAnotherSignal(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	objectPath := filepath.Join(f.rt.cfg.GitDir, "objects", f.nextOID[:2], f.nextOID[2:])
	unavailable := objectPath + ".unavailable"
	if err := os.Rename(objectPath, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, objectPath)
		}
	})

	f.signal()
	f.waitForLog("tree rebuild failed")
	if err := os.Rename(unavailable, objectPath); err != nil {
		t.Fatal(err)
	}
	f.waitForHead(f.nextOID, "main", f.oldGen+1)
}

func TestHeadReconcileRetriesTransientOverlayFailureWithoutRepublishing(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	if err := f.rt.overlay.Close(); err != nil {
		t.Fatal(err)
	}

	f.signal()
	f.waitForLog("overlay reconcile failed")
	recovered, err := overlay.New(f.ctx, f.rt.cfg.RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	f.rt.headMu.Lock()
	f.rt.overlay = recovered
	f.rt.resolver.Overlay = recovered
	f.rt.headMu.Unlock()
	f.waitForHead(f.nextOID, "main", f.oldGen+1)

	_, _, storedGen, err := f.rt.snapshot.ReadState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedGen != f.oldGen+1 {
		t.Fatalf("snapshot generation = %d, want reused generation %d", storedGen, f.oldGen+1)
	}
}

// After a failed overlay transition the snapshot store holds the new
// generation while the mount still serves the old one.
func TestStatusReportsPublishedAndServedGenerationsAfterOverlayFailure(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	if err := f.svc.registry.AddRepo(f.ctx, f.rt.cfg); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireDaemonLock(f.svc.root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// The daemon records a runtime once it mounts.
	f.svc.recordRuntimeState(f.rt, "")
	f.updateMain(f.nextOID)
	if err := f.rt.overlay.Close(); err != nil {
		t.Fatal(err)
	}
	f.signal()
	var st model.RepoRuntimeState
	waitFor(t, nil, func() bool {
		st, err = f.svc.ctl().Status(f.ctx, f.rt.cfg.Name)
		return err == nil && st.LastErrorPhase == phaseHEADReconcile
	})
	if st.PublishedHEADOID != f.nextOID || st.PublishedGeneration != f.oldGen+1 ||
		st.ServedHEADOID != f.oldOID || st.ServedGeneration != f.oldGen ||
		st.CurrentHEADOID != f.oldOID || st.NextRetryAt.IsZero() || st.Attempts == 0 {
		t.Fatalf("status after a failed overlay transition = %+v, want %s/%d published and %s/%d served with a retry", st, f.nextOID, f.oldGen+1, f.oldOID, f.oldGen)
	}
}

// engine is the FUSE adapter's engine on f's runtime, without a mount, with
// a hydrator for the base blobs a copy-on-write reads.
func (f *headReconcileFixture) engine() *fusefs.Engine {
	repo := f.rt.cfg.RepoPaths
	repo.BlobCacheDir = f.t.TempDir()
	h := hydrator.New(f.svc.git)
	h.Start(1, repo)
	f.t.Cleanup(h.Stop)
	return &fusefs.Engine{Resolver: f.rt.resolver, Repo: repo, Overlay: f.rt.overlay, Hydrator: h}
}

func (f *headReconcileFixture) unlink(e *fusefs.Engine, path string) {
	f.t.Helper()
	if err := e.Do(f.ctx, nil, func(ctx context.Context, b *fusefs.Blobs) error { return e.Unlink(ctx, b, path, nil) }); err != nil {
		f.t.Fatal(err)
	}
}

// overlayDirty registers f's repo, if it is not yet, and reads its status.
func (f *headReconcileFixture) overlayDirty() bool {
	f.t.Helper()
	if _, err := f.svc.registry.GetRepo(f.ctx, f.rt.cfg.Name); err != nil {
		if err := f.svc.registry.AddRepo(f.ctx, f.rt.cfg); err != nil {
			f.t.Fatal(err)
		}
	}
	st, err := f.svc.ctl().Status(f.ctx, f.rt.cfg.Name)
	if err != nil {
		f.t.Fatal(err)
	}
	return st.DirtyOverlay
}

// commitToMain commits the work tree's changes on main and returns the commit.
func (f *headReconcileFixture) commitToMain(message string) string {
	f.t.Helper()
	runCmd(f.t, "git", "-C", f.workDir, "add", "-A")
	runCmd(f.t, "git", "-C", f.workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", message)
	return strings.TrimSpace(runCmdOutput(f.t, "git", "-C", f.workDir, "rev-parse", "HEAD"))
}

// Deleting a tracked file is dirty. A HEAD change that removes the file
// makes the delete stale, and the reconcile prunes it.
func TestStatusReportsATrackedDeleteDirtyUntilReconcilePrunesIt(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.unlink(f.engine(), "README.md")
	if !f.overlayDirty() {
		t.Fatal("status of a delete-only overlay reports it clean")
	}
	runCmd(t, "git", "-C", f.workDir, "rm", "-q", "-f", "README.md")
	if err := os.WriteFile(filepath.Join(f.workDir, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	without := f.commitToMain("drop README")
	f.signal()
	f.waitForHead(without, "main", f.oldGen+1)
	if f.overlayDirty() {
		t.Fatal("status reports the overlay dirty after the reconcile pruned its stale delete")
	}
}

// With a daemon running, status reads a delete against the generation the
// mount serves. After a failed overlay transition, the published generation
// lacks the deleted path while the served one still has it.
func TestStatusReadsDeletesAgainstTheServedGeneration(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	if err := f.svc.registry.AddRepo(f.ctx, f.rt.cfg); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireDaemonLock(f.svc.root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	f.svc.recordRuntimeState(f.rt, "")
	f.unlink(f.engine(), "README.md")
	runCmd(t, "git", "-C", f.workDir, "rm", "-q", "-f", "README.md")
	if err := os.WriteFile(filepath.Join(f.workDir, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.commitToMain("drop README")
	if err := f.rt.overlay.Close(); err != nil {
		t.Fatal(err)
	}
	f.signal()
	var st model.RepoRuntimeState
	waitFor(t, nil, func() bool {
		st, err = f.svc.ctl().Status(f.ctx, f.rt.cfg.Name)
		return err == nil && st.LastErrorPhase == phaseHEADReconcile
	})
	if st.ServedGeneration != f.oldGen || st.PublishedGeneration != f.oldGen+1 {
		t.Fatalf("served/published generation = %d/%d, want %d/%d", st.ServedGeneration, st.PublishedGeneration, f.oldGen, f.oldGen+1)
	}
	if !st.DirtyOverlay {
		t.Fatal("status reports the overlay clean while it hides a file the served generation has")
	}
}

// A path the base never had, made and removed again, leaves the overlay
// clean, as an editor's swap file or an rm -rf of build output does.
func TestStatusReportsCleanAfterAnUntrackedPathComesAndGoes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		op   func(f *headReconcileFixture, e *fusefs.Engine) error
	}{
		{"file", func(f *headReconcileFixture, e *fusefs.Engine) error {
			if err := e.Create(f.ctx, "scratch.swp", 0o644); err != nil {
				return err
			}
			f.unlink(e, "scratch.swp")
			return nil
		}},
		{"directory", func(f *headReconcileFixture, e *fusefs.Engine) error {
			if _, err := e.Mkdir(f.ctx, "build", 0o755); err != nil {
				return err
			}
			return e.Rmdir(f.ctx, "build")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newHeadReconcileFixture(t)
			if err := tt.op(f, f.engine()); err != nil {
				t.Fatal(err)
			}
			if f.overlayDirty() {
				t.Fatal("status reports the overlay dirty after an untracked path came and went")
			}
		})
	}
}

// A directory made, used as a rename's stop and removed again is gone: the
// delete entries that rename chain leaves below it do not keep it listed.
func TestRmdirOfARenameChainStopRemovesTheDirectory(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	e := f.engine()
	rename := func(from, to string) {
		t.Helper()
		if err := e.Do(f.ctx, nil, func(ctx context.Context, b *fusefs.Blobs) error { return e.Rename(ctx, b, from, to, nil) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Mkdir(f.ctx, "build", 0o755); err != nil {
		t.Fatal(err)
	}
	rename("README.md", "build/r.md")
	rename("build/r.md", "out.md")
	if err := e.Rmdir(f.ctx, "build"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rt.resolver.ResolvePath("build"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("build after rmdir: %v, want it gone", err)
	}
	if _, err := e.Mkdir(f.ctx, "build", 0o755); err != nil {
		t.Fatalf("mkdir build after rmdir: %v", err)
	}
}

// HEAD can move to a commit that has a path before the served generation
// does. A file written and deleted there stays deleted once the generation
// catches up, as git status reports it.
func TestDeleteBeforeTheServedGenerationCatchesUpStaysDeleted(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	if err := os.WriteFile(filepath.Join(f.workDir, "p.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	with := f.commitToMain("add p.txt")
	e := f.engine()
	if err := e.Create(f.ctx, "p.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Do(f.ctx, nil, func(ctx context.Context, b *fusefs.Blobs) error {
		_, err := e.Write(ctx, b, "p.txt", 0, []byte("written\n"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.unlink(e, "p.txt")
	f.signal()
	f.waitForHead(with, "main", f.oldGen+1)
	if _, err := f.rt.resolver.ResolvePath("p.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("p.txt after the generation caught up: %v, want it deleted", err)
	}
	if !f.overlayDirty() {
		t.Fatal("status reports the overlay clean while it hides a committed file")
	}
}

// Status stops at the first delete the served generation has, and finds
// none among many deletes of paths it lacks.
func TestStatusReportsManyUntrackedDeletesClean(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	// Ten thousand unlinks through the engine would make this test slow, so
	// it writes the delete entries they leave, build/0.o to build/9999.o, in
	// one statement.
	db, err := meta.OpenDB(f.rt.cfg.OverlayDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(f.ctx, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 9999)
		INSERT INTO overlay_entries(path, kind, mode, mtime_unix_ns, ctime_unix_ns) SELECT 'build/' || i || '.o', ?, 0, 0, 0 FROM n`, model.OverlayKindDelete); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM overlay_entries WHERE kind = ? AND path GLOB 'build/*.o'`, model.OverlayKindDelete).Scan(&count); err != nil || count != 10000 {
		t.Fatalf("delete entries = %d, %v; want 10000", count, err)
	}
	if f.overlayDirty() {
		t.Fatal("status reports deletes of paths the served generation lacks as dirty")
	}
	f.unlink(f.engine(), "README.md")
	if !f.overlayDirty() {
		t.Fatal("status reports the overlay clean after a delete of a path the served generation has")
	}
}

func TestHeadReconcileRestoresPersistedStateWhenHEADReturnsAfterOverlayFailure(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	if err := f.rt.overlay.Close(); err != nil {
		t.Fatal(err)
	}
	f.signal()
	f.waitForLog("overlay reconcile failed")

	storedOID, _, storedGen, err := f.rt.snapshot.ReadState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedOID != f.nextOID || storedGen != f.oldGen+1 {
		t.Fatalf("snapshot before rollback = %s/%d, want %s/%d", storedOID, storedGen, f.nextOID, f.oldGen+1)
	}
	f.waitForHead(f.oldOID, "main", f.oldGen)
	recovered, err := overlay.New(f.ctx, f.rt.cfg.RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	f.updateMain(f.oldOID)
	f.rt.headMu.Lock()
	f.rt.overlay = recovered
	f.rt.resolver.Overlay = recovered
	f.rt.headMu.Unlock()
	f.signal()
	f.waitForHead(f.oldOID, "main", f.oldGen+2)

	cfg := f.rt.cfg
	cfg.MetaDBPath = filepath.Join(filepath.Dir(cfg.OverlayDir), "snapshot.sqlite")
	status := persistedStatus(f.ctx, cfg, model.RepoRuntimeRecord{}, false)
	if status.CurrentHEADOID != f.oldOID || status.CurrentHEADRef != "main" || status.SnapshotGeneration != f.oldGen+2 {
		t.Fatalf("persisted status after rollback = %+v", status)
	}
}

func TestHeadReconcilePersistsSameOIDRefBeforeUpdatingRuntime(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	runCmd(t, "git", "-C", f.workDir, "branch", "alias", f.oldOID)
	runCmd(t, "git", "-C", f.workDir, "symbolic-ref", "HEAD", "refs/heads/alias")
	snapshotPath := filepath.Join(filepath.Dir(f.rt.cfg.OverlayDir), "snapshot.sqlite")
	if err := f.rt.snapshot.Close(); err != nil {
		t.Fatal(err)
	}

	f.signal()
	f.waitForLog("snapshot publish failed")
	f.svc.mu.Lock()
	gotRef := f.rt.head.ref
	f.svc.mu.Unlock()
	if gotRef != "main" {
		t.Fatalf("runtime ref advanced before persistence: got %q", gotRef)
	}
	recovered, err := snapshot.New(f.ctx, snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	f.rt.headMu.Lock()
	f.rt.snapshot = recovered
	f.rt.resolver.Snapshot = recovered
	f.rt.headMu.Unlock()
	f.waitForHead(f.oldOID, "alias", f.oldGen)

	_, storedRef, _, err := recovered.ReadState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedRef != "alias" {
		t.Fatalf("persisted ref = %q, want alias", storedRef)
	}
}

func TestHeadReconcileCoalescesSignalsAndPublishesLatestHEAD(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	unavailable := f.rt.cfg.GitDir + ".unavailable"
	if err := os.Rename(f.rt.cfg.GitDir, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, f.rt.cfg.GitDir)
		}
	})
	runCmd(t, "git", "--git-dir", unavailable, "update-ref", "refs/heads/main", f.nextOID)
	f.signal()
	f.waitForLog("HEAD resolve failed")
	runCmd(t, "git", "--git-dir", unavailable, "update-ref", "refs/heads/main", f.lastOID)
	if err := os.Rename(unavailable, f.rt.cfg.GitDir); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		f.signal()
	}
	f.waitForHead(f.lastOID, "main", f.oldGen+1)

	_, _, storedGen, err := f.rt.snapshot.ReadState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedGen != f.oldGen+1 {
		t.Fatalf("snapshot generation = %d, want one coalesced publication at %d", storedGen, f.oldGen+1)
	}
}

func TestHeadReconcileBackoffStopsOnCancellation(t *testing.T) {
	t.Parallel()
	// Only cancellation can end an hour-long backoff before go test -timeout.
	f := newHeadReconcileFixtureWithBackoff(t, time.Hour)
	unavailable := f.rt.cfg.GitDir + ".unavailable"
	if err := os.Rename(f.rt.cfg.GitDir, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, f.rt.cfg.GitDir)
		}
	})
	f.signal()
	f.waitForLog("HEAD resolve failed")

	f.cancel()
	f.rt.workers.Wait()
}

func TestHeadReconcileResumesAfterUnmountFailure(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	mfs := &delayedUnmountFailure{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	f.svc.mu.Lock()
	f.rt.mfs, f.rt.mount = mfs, mountServing
	f.svc.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		errCh <- f.svc.unmount(f.rt.cfg.ID)
	}()
	select {
	case <-mfs.started:
	case err := <-errCh:
		t.Fatalf("unmount returned before reaching the mount: %v", err)
	}

	f.updateMain(f.nextOID)
	f.signal()
	close(mfs.release)
	if err := <-errCh; err == nil {
		t.Fatal("expected unmount failure")
	}
	f.waitForHead(f.nextOID, "main", f.oldGen+1)
}

func TestHeadReconcileServesReadsMatchingReportedSize(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	repo := f.rt.cfg
	repo.BlobCacheDir = t.TempDir()
	h := hydrator.New(f.svc.git)
	h.Start(1, repo.RepoPaths)
	t.Cleanup(h.Stop)
	fs := fusefs.NewArtifactFuse(repo.RepoPaths, &fusefs.Engine{Resolver: f.rt.resolver, Repo: repo.RepoPaths, Overlay: f.rt.overlay, Hydrator: h})
	open := func(inode fuseops.InodeID) fuseops.HandleID {
		t.Helper()
		op := &fuseops.OpenFileOp{Inode: inode}
		if err := fs.OpenFile(f.ctx, op); err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		t.Cleanup(func() {
			_ = fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
		})
		return op.Handle
	}
	read := func(inode fuseops.InodeID, handle fuseops.HandleID) string {
		t.Helper()
		op := &fuseops.ReadFileOp{Inode: inode, Handle: handle, Size: 64}
		if err := fs.ReadFile(f.ctx, op); err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		return string(bytes.Join(op.Data, nil))
	}

	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "README.md"}
	if err := fs.LookUpInode(f.ctx, lookup); err != nil {
		t.Fatalf("LookUpInode: %v", err)
	}
	inode := lookup.Entry.Child
	openedBefore := open(inode)
	f.updateMain(f.nextOID)
	f.signal()
	f.waitForHead(f.nextOID, "main", f.oldGen+1)

	reported := lookup.Entry.Attributes.Size
	if got := read(inode, openedBefore); got != "old\n" || uint64(len(got)) != reported {
		t.Errorf("read through handle opened before HEAD moved = %q, want the old content the lookup sized at %d bytes", got, reported)
	}
	if got := read(inode, open(inode)); got != "old\n" || uint64(len(got)) != reported {
		t.Errorf("read through handle opened after HEAD moved = %q, want the old content the lookup sized at %d bytes", got, reported)
	}

	getattr := &fuseops.GetInodeAttributesOp{Inode: inode}
	if err := fs.GetInodeAttributes(f.ctx, getattr); err != nil {
		t.Fatalf("GetInodeAttributes: %v", err)
	}
	reported = getattr.Attributes.Size
	if got := read(inode, openedBefore); got != "next\n" || uint64(len(got)) != reported {
		t.Errorf("read through handle opened before getattr = %q, want the new content getattr sized at %d bytes", got, reported)
	}
	if got := read(inode, open(inode)); got != "next\n" || uint64(len(got)) != reported {
		t.Errorf("read through handle opened after getattr = %q, want the new content getattr sized at %d bytes", got, reported)
	}
}

// invalidatingMount records, each time the daemon asks the mount to
// invalidate the kernel's caches, the generation the resolver serves and
// whether an op would wait at gate. The kernel can hold a directory's lock
// while a lookup there waits at the gate, and its entry invalidation waits
// for that lock, so an invalidation sent then blocks until the lookup ends.
// during, when not nil, runs first.
type invalidatingMount struct {
	*fakeMountedFS
	resolver *fusefs.Resolver
	gate     *fusefs.ReadyGate
	during   func()
	passes   chan switchPass
}

type switchPass struct {
	gen  int64
	held bool
}

func (m *invalidatingMount) InvalidateChanged() error {
	if m.during != nil {
		m.during()
	}
	m.passes <- switchPass{gen: m.resolver.Generation(), held: holdsOps(m.gate)}
	return nil
}

// holdsOps reports whether an op that reaches gate now would wait.
func holdsOps(gate *fusefs.ReadyGate) bool {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return errors.Is(gate.Wait(ctx), context.Canceled)
}

func newInvalidatingMount(resolver *fusefs.Resolver, gate *fusefs.ReadyGate) *invalidatingMount {
	return &invalidatingMount{fakeMountedFS: &fakeMountedFS{unmounted: make(chan struct{})}, resolver: resolver, gate: gate, passes: make(chan switchPass, 4)}
}

func TestHeadReconcileInvalidatesKernelCacheAfterSwitch(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	mount := newInvalidatingMount(f.rt.resolver, nil)
	f.svc.mu.Lock()
	f.rt.mfs, f.rt.mount = mount, mountServing
	f.svc.mu.Unlock()

	f.updateMain(f.nextOID)
	f.signal()
	if pass := <-mount.passes; pass.gen != f.oldGen+1 {
		t.Fatalf("mount invalidated while the resolver served generation %d, want the new generation %d", pass.gen, f.oldGen+1)
	}
}

// A HEAD reconcile can switch while the gate holds ops: an async repo's
// preparation moves the branch, and the watcher sees it before the
// preparation opens the gate. The reconcile's pass then waits for the gate.
func TestHeadReconcileInvalidatesOnlyOnceTheGateOpens(t *testing.T) {
	t.Parallel()
	f := newIdleHeadReconcileFixture(t, time.Millisecond)
	gate := fusefs.NewReadyGate(false)
	mount := newInvalidatingMount(f.rt.resolver, gate)
	f.svc.mu.Lock()
	f.rt.gate, f.rt.mfs, f.rt.mount = gate, mount, mountServing
	f.svc.mu.Unlock()

	f.updateMain(f.nextOID)
	if err := f.svc.reconcileHEAD(f.ctx, f.rt); err != nil {
		t.Fatal(err)
	}
	gate.MarkReady()
	if pass := <-mount.passes; pass.held || pass.gen != f.oldGen+1 {
		t.Fatalf("mount invalidated at generation %d, with ops held at the gate: %v; want generation %d with the gate open", pass.gen, pass.held, f.oldGen+1)
	}
}

// servedAsyncRepo is an async repo of a local remote that a daemon prepared
// once and serves through an invalidatingMount. Its HEAD watcher never polls.
type servedAsyncRepo struct {
	svc    *Service
	cfg    model.RepoConfig
	remote string
	rt     *repoRuntime
	gate   *fusefs.ReadyGate
	// passes receives every pass after the first preparation's.
	passes chan switchPass
}

// newServedAsyncRepo builds a servedAsyncRepo. setup, when not nil, sets the
// daemon's hooks before the repo registers. The mount runs during, when not
// nil, at the start of every pass.
func newServedAsyncRepo(t *testing.T, setup func(*Service), during func()) *servedAsyncRepo {
	t.Helper()
	ctx := t.Context()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	commitFile(t, remote, "README.md", "one\n")
	runCmd(t, "git", "-C", remote, "config", "uploadpack.allowFilter", "true")
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	passes := make(chan switchPass, 4)
	svc.headPollTicks = make(chan time.Time)
	svc.mount = func(_, _ context.Context, _ model.RepoPaths, engine *fusefs.Engine, gate *fusefs.ReadyGate, _ *slog.Logger, _ func(string) string) (fusefs.MountedFS, error) {
		mount := newInvalidatingMount(engine.Resolver, gate)
		mount.passes, mount.during = passes, during
		return mount, nil
	}
	if setup != nil {
		setup(svc)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + remote, Branch: "main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Hour}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.prepareWorkers.Wait()
	ready, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	rt := runningRuntime(svc, ready.ID)
	if ready.PrepareState != model.PrepareStateReady || rt == nil {
		t.Fatalf("after the first preparation: prepare state %q, runtime %v; want ready and running", ready.PrepareState, rt)
	}
	select {
	case <-passes:
	default:
	}
	svc.mu.Lock()
	gate := rt.gate
	svc.mu.Unlock()
	return &servedAsyncRepo{svc: svc, cfg: ready, remote: remote, rt: rt, gate: gate, passes: passes}
}

// requestPrepare requests a preparation of r's repo, as `artifact-fs prepare`
// does, and runs the sync its nudge wakes.
func (r *servedAsyncRepo) requestPrepare(ctx context.Context, request string) error {
	if err := r.svc.registry.BeginPrepare(ctx, r.cfg, model.PrepareStatePreparing, request); err != nil {
		return err
	}
	return r.svc.syncRepos(ctx)
}

// commitTimestampGit runs before ahead of each CommitTimestamp.
type commitTimestampGit struct {
	model.GitStore
	before func()
}

func (g *commitTimestampGit) CommitTimestamp(ctx context.Context, repo model.RepoConfig, oid string) (int64, error) {
	g.before()
	return g.GitStore.CommitTimestamp(ctx, repo, oid)
}

// A prepare request can reset the gate while a switch pass runs, here a HEAD
// reconcile's. The reset holds ops only once the pass ends: a lookup it held
// could block the pass's entry invalidations. The end of the request's
// preparation during the pass replaces the reset.
func TestGateResetDuringASwitchPassTakesEffectWhenThePassEnds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// end is how the request's preparation ends during the pass. With
		// none, it waits before its activation until the test saw the gate.
		end model.PrepareState
	}{
		{name: "reset"},
		{name: "reset then ready", end: model.PrepareStateReady},
		{name: "reset then failed", end: model.PrepareStateFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			var r *servedAsyncRepo
			var armed, requested atomic.Bool
			release := make(chan struct{})
			var releaseOnce sync.Once
			r = newServedAsyncRepo(t, func(svc *Service) {
				svc.readyRecorded = func() {
					if tt.end == "" && requested.Load() {
						<-release
					}
				}
			}, func() {
				if !armed.CompareAndSwap(true, false) {
					return
				}
				requested.Store(true)
				if err := r.requestPrepare(ctx, "next"); err != nil {
					t.Errorf("prepare request during the pass: %v", err)
					return
				}
				switch tt.end {
				case model.PrepareStateReady:
					// The activation's pass follows its open of the gate.
					<-r.passes
				case model.PrepareStateFailed:
					for {
						st, err := r.svc.ctl().Status(ctx, r.cfg.Name)
						if err != nil {
							t.Errorf("status during the pass: %v", err)
							return
						}
						if st.State == string(model.PrepareStateFailed) {
							return
						}
						time.Sleep(time.Millisecond)
					}
				}
			})
			defer releaseOnce.Do(func() { close(release) })

			next := commitFile(t, r.remote, "README.md", "two\n")
			runCmd(t, "git", "--git-dir", r.cfg.GitDir, "fetch", "-q", "origin", "main")
			runCmd(t, "git", "--git-dir", r.cfg.GitDir, "update-ref", "HEAD", next)
			if tt.end == model.PrepareStateFailed {
				if err := os.RemoveAll(r.remote); err != nil {
					t.Fatal(err)
				}
			}
			armed.Store(true)
			if err := r.svc.reconcileHEAD(ctx, r.rt); err != nil {
				t.Fatal(err)
			}
			if armed.Load() {
				t.Fatal("the HEAD reconcile sent no pass")
			}
			if pass := <-r.passes; pass.held {
				t.Fatalf("HEAD reconcile invalidated at generation %d with ops held at the gate, want the reset to hold them only once the pass ends", pass.gen)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			err := r.gate.Wait(canceled)
			switch tt.end {
			case "":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("op at the gate after the pass = %v, want it held for the request's preparation", err)
				}
			case model.PrepareStateReady:
				if err != nil {
					t.Fatalf("op at the gate after the pass = %v, want it through: the preparation ended ready during the pass", err)
				}
			case model.PrepareStateFailed:
				if err == nil || errors.Is(err, context.Canceled) {
					t.Fatalf("op at the gate after the pass = %v, want the failure of the preparation that ended during the pass", err)
				}
			}
			releaseOnce.Do(func() { close(release) })
			r.svc.prepareWorkers.Wait()
		})
	}
}

// A prepare of an async repo can land after the repo's first preparation made
// it ready: Controller.Prepare reads the repo as preparing, and BeginPrepare
// checks only the config. The kernel then caches entries from the open gate,
// and the late preparation moves HEAD to the remote's new tip. Its activation
// must invalidate the kernel's caches once the resolver serves the new
// generation, and only once it opened the gate: a lookup the gate held would
// block the invalidations until the lookup ended.
func TestLateAsyncPrepareInvalidatesKernelCacheAfterSwitch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var r *servedAsyncRepo
	// statesAtPass receives, during each pass of the late preparation, the
	// state status reports.
	var late atomic.Bool
	statesAtPass := make(chan string, 4)
	r = newServedAsyncRepo(t, func(svc *Service) {
		// A pass that waits for the gate waits here also until status no
		// longer reports the late preparation, so it cannot beat the
		// activation's record.
		svc.passDeferred = func() {
			for late.Load() {
				if st, err := svc.ctl().Status(ctx, r.cfg.Name); err != nil || st.State != string(model.PrepareStatePreparing) {
					return
				}
			}
		}
	}, func() {
		if !late.Load() {
			return
		}
		// Any record during the pass, here the refresh loop's after a fetch,
		// reports what the runtime holds then.
		start := time.Now()
		r.rt.refresh <- time.Millisecond
		defer func() { r.rt.refresh <- time.Hour }()
		for {
			rec, _, err := r.svc.registry.GetRuntime(ctx, r.cfg.ID)
			if err != nil {
				t.Errorf("runtime record during the pass: %v", err)
				statesAtPass <- ""
				return
			}
			if rec.LastFetchAt.After(start) {
				statesAtPass <- rec.State
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	r.svc.mu.Lock()
	firstGen := r.rt.head.gen
	r.svc.mu.Unlock()

	next := commitFile(t, r.remote, "README.md", "two\n")
	late.Store(true)
	// reconcileHEAD runs under headMu, so only the activation can switch the
	// resolver. The activation's prune waits for headMu.
	r.rt.headMu.Lock()
	if err := r.requestPrepare(ctx, "late"); err != nil {
		r.rt.headMu.Unlock()
		t.Fatal(err)
	}
	pass := <-r.passes
	r.rt.headMu.Unlock()
	if err := r.gate.Wait(ctx); err != nil {
		t.Fatalf("gate after the late preparation = %v, want open", err)
	}
	r.svc.prepareWorkers.Wait()

	r.svc.mu.Lock()
	served := r.rt.head
	r.svc.mu.Unlock()
	if runningRuntime(r.svc, r.cfg.ID) != r.rt || served.oid != next || served.gen == firstGen {
		t.Fatalf("after the late preparation the runtime serves %s at generation %d, want the same runtime serving %s at a generation after %d", served.oid, served.gen, next, firstGen)
	}
	if pass.held || pass.gen != served.gen {
		t.Fatalf("mount invalidated at generation %d, with ops held at the gate: %v; want generation %d, the generation the activation switched to, with the gate open", pass.gen, pass.held, served.gen)
	}
	// Once status no longer reports the preparation, the kernel holds nothing
	// the switch changed.
	if state := <-statesAtPass; state != string(model.PrepareStatePreparing) {
		t.Fatalf("status during the activation's pass = %q, want %q: the activation records the end of the preparation only after the pass", state, model.PrepareStatePreparing)
	}
}

// A newer prepare request can supersede an activation once its resolver
// switched: drivePrepare resets the runtime for the request before it cancels
// the preparation the request replaces. The superseded activation leaves the
// gate shut, so its pass waits for the newer preparation to open it. Sent at
// once, the pass would wait for a lookup the gate holds, which waits for the
// newer preparation, which waits for the repo's prepare lock that the
// superseded one holds.
func TestSupersededActivationSendsItsPassOnlyOnceTheGateOpens(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var r *servedAsyncRepo
	var supersede atomic.Bool
	r = newServedAsyncRepo(t, func(svc *Service) {
		svc.git = &commitTimestampGit{GitStore: svc.git, before: func() {
			if !supersede.CompareAndSwap(true, false) {
				return
			}
			if err := svc.registry.BeginPrepare(ctx, r.cfg, model.PrepareStatePreparing, "newer"); err != nil {
				t.Errorf("newer prepare request: %v", err)
				return
			}
			repo, err := svc.registry.GetRepo(ctx, r.cfg.Name)
			if err != nil {
				t.Errorf("read the newer request: %v", err)
				return
			}
			svc.resetRunningPrepareState(repo)
		}}
	}, nil)

	commitFile(t, r.remote, "README.md", "two\n")
	supersede.Store(true)
	if err := r.requestPrepare(ctx, "late"); err != nil {
		t.Fatal(err)
	}
	r.svc.prepareWorkers.Wait()
	if supersede.Load() {
		t.Fatal("the late preparation never activated")
	}
	if !holdsOps(r.gate) {
		t.Fatal("gate after the superseded activation is open, want it held for the newer request")
	}
	// The next sync starts the newer request's preparation, which opens the
	// gate.
	if err := r.svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if pass := <-r.passes; pass.held {
			t.Fatalf("mount invalidated at generation %d with ops held at the gate, want every pass with the gate open", pass.gen)
		}
	}
	r.svc.prepareWorkers.Wait()
}

// A HEAD reconcile can switch further during an activation's pass: the
// gate is open, and HEAD moves again. The activation then keeps the served
// HEAD the reconcile recorded, which the resolver serves, so status reports
// the commit and generation the mount serves.
func TestActivationKeepsTheServedHEADOfAReconcileDuringItsPass(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var r *servedAsyncRepo
	var armed atomic.Bool
	r = newServedAsyncRepo(t, nil, func() {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		prev, err := exec.Command("git", "--git-dir", r.cfg.GitDir, "rev-parse", "HEAD~1").Output()
		if err != nil {
			t.Errorf("resolve HEAD~1 during the pass: %v", err)
			return
		}
		if out, err := exec.Command("git", "--git-dir", r.cfg.GitDir, "update-ref", "HEAD", strings.TrimSpace(string(prev))).CombinedOutput(); err != nil {
			t.Errorf("move HEAD during the pass: %v\n%s", err, out)
			return
		}
		if err := r.svc.reconcileHEAD(ctx, r.rt); err != nil {
			t.Errorf("HEAD reconcile during the pass: %v", err)
		}
	})

	commitFile(t, r.remote, "README.md", "two\n")
	armed.Store(true)
	if err := r.requestPrepare(ctx, "late"); err != nil {
		t.Fatal(err)
	}
	r.svc.prepareWorkers.Wait()
	if armed.Load() {
		t.Fatal("the late activation sent no pass")
	}
	head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", r.cfg.GitDir, "rev-parse", "HEAD"))
	rec, _, err := r.svc.registry.GetRuntime(ctx, r.cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gen := r.rt.resolver.Generation(); rec.ServedCommit != head || rec.ServedGeneration != gen {
		t.Fatalf("status after the activation serves %s at generation %d; want HEAD %s at generation %d, which the resolver serves", rec.ServedCommit, rec.ServedGeneration, head, gen)
	}
}

// A newer prepare request can land as an activation opens the gate. The
// activation opens it in the s.mu hold that checks its request, so the
// request's reset lands before the check, which supersedes the activation,
// or after the open. A reset between the two would find the gate shut, and
// the open would then let ops through while the newer preparation runs. The
// request starts once the activation opened the gate, and lands once the
// activation releases s.mu.
func TestPrepareRequestAsAnActivationOpensTheGateHoldsOps(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var r *servedAsyncRepo
	var late, newer atomic.Bool
	requested := make(chan struct{})
	heldAtReady := make(chan bool, 1)
	request := func() {
		defer close(requested)
		if err := r.requestPrepare(ctx, "newer"); err != nil {
			t.Errorf("newer prepare request: %v", err)
		}
	}
	r = newServedAsyncRepo(t, func(svc *Service) {
		svc.gateOpened = func() {
			if !late.CompareAndSwap(true, false) {
				return
			}
			newer.Store(true)
			if svc.mu.TryLock() {
				svc.mu.Unlock()
				t.Error("the activation opened the gate outside the s.mu hold that checked its request")
			}
			if holdsOps(r.gate) {
				t.Error("the gate holds ops once the activation opened it")
			}
			go request()
		}
		svc.readyRecorded = func() {
			if newer.Load() {
				heldAtReady <- holdsOps(r.gate)
			}
		}
	}, nil)

	commitFile(t, r.remote, "README.md", "two\n")
	late.Store(true)
	if err := r.requestPrepare(ctx, "late"); err != nil {
		t.Fatal(err)
	}
	held := <-heldAtReady
	<-requested
	r.svc.prepareWorkers.Wait()
	if !held {
		t.Fatal("gate during the newer request's preparation is open, want it held until that preparation activates")
	}
}

// An inode whose attributes came from a generation keeps reading it after two
// switches inside the retention window. Once the window lapses, the next
// switch prunes it.
func TestHeadReconcileKeepsRetiredGenerationReadable(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		retention time.Duration
		want      string
	}{
		{name: "within retention", retention: time.Hour, want: "old\n"},
		{name: "after retention", retention: time.Nanosecond, want: "last\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newHeadReconcileFixture(t)
			f.svc.generationRetention = tt.retention
			repo := f.rt.cfg
			repo.BlobCacheDir = t.TempDir()
			h := hydrator.New(f.svc.git)
			h.Start(1, repo.RepoPaths)
			t.Cleanup(h.Stop)
			fs := fusefs.NewArtifactFuse(repo.RepoPaths, &fusefs.Engine{Resolver: f.rt.resolver, Repo: repo.RepoPaths, Overlay: f.rt.overlay, Hydrator: h})
			lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "README.md"}
			if err := fs.LookUpInode(f.ctx, lookup); err != nil {
				t.Fatalf("LookUpInode: %v", err)
			}

			f.moveHEAD(f.nextOID, f.oldGen+1)
			f.moveHEAD(f.lastOID, f.oldGen+2)

			open := &fuseops.OpenFileOp{Inode: lookup.Entry.Child}
			if err := fs.OpenFile(f.ctx, open); err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			t.Cleanup(func() {
				_ = fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
			})
			read := &fuseops.ReadFileOp{Inode: lookup.Entry.Child, Handle: open.Handle, Size: 64}
			if err := fs.ReadFile(f.ctx, read); err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if got := string(bytes.Join(read.Data, nil)); got != tt.want {
				t.Fatalf("read through the inode looked up before both switches = %q, want %q", got, tt.want)
			}
		})
	}
}

// moveHEAD moves main to oid, then waits until the runtime serves it as
// generation gen and the reconcile, with its prune, has ended.
func (f *headReconcileFixture) moveHEAD(oid string, gen int64) {
	f.t.Helper()
	f.updateMain(oid)
	f.signal()
	f.waitForHead(oid, "main", gen)
	f.rt.headMu.Lock()
	f.rt.headMu.Unlock()
}

// emptyCommit commits the empty tree as a child of parent.
func (f *headReconcileFixture) emptyCommit(parent, message string) string {
	f.t.Helper()
	tree := strings.TrimSpace(runCmdOutput(f.t, "git", "-C", f.workDir, "hash-object", "-w", "-t", "tree", "/dev/null"))
	return strings.TrimSpace(runCmdOutput(f.t, "git", "-C", f.workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit-tree", tree, "-p", parent, "-m", message))
}

// servingMount stands in for a serving mount of fs on backend, the fake
// backend when empty.
type servingMount struct {
	*fakeMountedFS
	fs      *fusefs.ArtifactFuse
	backend string
}

func (m *servingMount) Backend() string              { return cmp.Or(m.backend, m.fakeMountedFS.Backend()) }
func (m *servingMount) OpenFileGenerations() []int64 { return m.fs.OpenFileGenerations() }
func (m *servingMount) OpenFilePIDs(gens []int64) map[uint32]int {
	return m.fs.OpenFilePIDs(gens)
}

// openFileFixture serves its runtime through fs, which the test drives with
// FUSE ops. Its retention window lapses at once.
type openFileFixture struct {
	*headReconcileFixture
	fs    *fusefs.ArtifactFuse
	mount *servingMount
}

func newOpenFileFixture(t *testing.T) *openFileFixture {
	t.Helper()
	f := newHeadReconcileFixture(t)
	f.svc.generationRetention = time.Nanosecond
	repo := f.rt.cfg
	repo.BlobCacheDir = t.TempDir()
	h := hydrator.New(f.svc.git)
	h.Start(1, repo.RepoPaths)
	t.Cleanup(h.Stop)
	fs := fusefs.NewArtifactFuse(repo.RepoPaths, &fusefs.Engine{Resolver: f.rt.resolver, Repo: repo.RepoPaths, Overlay: f.rt.overlay, Hydrator: h})
	mount := &servingMount{fakeMountedFS: &fakeMountedFS{unmounted: make(chan struct{})}, fs: fs}
	f.svc.mu.Lock()
	f.rt.mfs, f.rt.mount = mount, mountServing
	f.svc.mu.Unlock()
	return &openFileFixture{headReconcileFixture: f, fs: fs, mount: mount}
}

func (f *openFileFixture) lookupReadme() fuseops.InodeID {
	f.t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "README.md"}
	if err := f.fs.LookUpInode(f.ctx, op); err != nil {
		f.t.Fatalf("LookUpInode: %v", err)
	}
	return op.Entry.Child
}

func (f *openFileFixture) open(inode fuseops.InodeID) (fuseops.HandleID, error) {
	op := &fuseops.OpenFileOp{Inode: inode}
	if err := f.fs.OpenFile(f.ctx, op); err != nil {
		return 0, err
	}
	f.t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
	})
	return op.Handle, nil
}

func (f *openFileFixture) read(inode fuseops.InodeID, handle fuseops.HandleID) (string, error) {
	op := &fuseops.ReadFileOp{Inode: inode, Handle: handle, Size: 64}
	if err := f.fs.ReadFile(f.ctx, op); err != nil {
		return "", err
	}
	return string(bytes.Join(op.Data, nil)), nil
}

// A HEAD change removes the path of a file that stays open. The kernel still
// holds the file's inode, so it can open the inode again, as FSKit does
// through a name it still caches. The prune at a later HEAD change must keep
// the generation the inode reads while the file is open, so the new open
// reads the file. It must still prune each other generation outside the
// retention window: a file that never closes keeps one generation.
func TestHeadReconcileKeepsTheGenerationOfAnOpenFile(t *testing.T) {
	t.Parallel()
	f := newOpenFileFixture(t)
	inode := f.lookupReadme()
	if _, err := f.open(inode); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	gone := f.emptyCommit(f.oldOID, "gone")
	stillGone := f.emptyCommit(gone, "still gone")
	f.moveHEAD(gone, f.oldGen+1)
	f.moveHEAD(stillGone, f.oldGen+2)

	again, err := f.open(inode)
	if err != nil {
		t.Fatalf("open of the held inode after the prune = %v, want the file", err)
	}
	if got, err := f.read(inode, again); err != nil || got != "old\n" {
		t.Fatalf("read through the new handle = %q, %v, want %q", got, err, "old\n")
	}

	if _, ok := lookupNode(t, f.rt.snapshot, f.oldGen+1, "."); !ok {
		t.Fatalf("generation %d is gone before the next HEAD change", f.oldGen+1)
	}
	f.moveHEAD(f.emptyCommit(stillGone, "gone again"), f.oldGen+3)
	for gen, want := range map[int64]bool{f.oldGen: true, f.oldGen + 1: false, f.oldGen + 2: true, f.oldGen + 3: true} {
		if _, ok := lookupNode(t, f.rt.snapshot, gen, "."); ok != want {
			t.Errorf("generation %d kept = %t, want %t", gen, ok, want)
		}
	}
}

// A handle keeps a descriptor of the blob it opened while its inode reads the
// blob's generation. Once a getattr reports a newer generation, the handle
// opens the blob there at its next read. The prune at a later HEAD change must
// keep that generation while the handle is open, so the read matches the size
// the getattr reported, whether the path then changed or vanished.
func TestHeadReconcileKeepsTheGenerationAnOpenFileReopensAt(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		later func(*openFileFixture) []string
	}{
		{name: "blob changed", later: func(f *openFileFixture) []string { return []string{f.lastOID, f.oldOID} }},
		{name: "path removed", later: func(f *openFileFixture) []string {
			gone := f.emptyCommit(f.nextOID, "gone")
			return []string{gone, f.emptyCommit(gone, "still gone")}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newOpenFileFixture(t)
			inode := f.lookupReadme()
			handle, err := f.open(inode)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			f.moveHEAD(f.nextOID, f.oldGen+1)
			getattr := &fuseops.GetInodeAttributesOp{Inode: inode}
			if err := f.fs.GetInodeAttributes(f.ctx, getattr); err != nil {
				t.Fatalf("GetInodeAttributes: %v", err)
			}
			for i, oid := range tt.later(f) {
				f.moveHEAD(oid, f.oldGen+2+int64(i))
			}

			got, err := f.read(inode, handle)
			if err != nil || got != "next\n" || uint64(len(got)) != getattr.Attributes.Size {
				t.Fatalf("read through the handle = %q, %v, want %q, the content getattr sized at %d bytes", got, err, "next\n", getattr.Attributes.Size)
			}
		})
	}
}
