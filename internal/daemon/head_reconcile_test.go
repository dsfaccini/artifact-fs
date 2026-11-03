package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/dsfaccini/fuse/fuseops"
)

type headReconcileFixture struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
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

func (m *delayedUnmountFailure) Backend() string            { return "fake" }
func (m *delayedUnmountFailure) Join(context.Context) error { return nil }

func (m *delayedUnmountFailure) Unmount() error {
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
		ID:            "repro",
		Name:          "repro",
		GitDir:        filepath.Join(workDir, ".git"),
		OverlayDir:    filepath.Join(root, "overlay"),
		OverlayDBPath: filepath.Join(root, "overlay", "meta.sqlite"),
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
	snap, err := snapshot.New(ctx, filepath.Join(root, "snapshot.sqlite"))
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
	ov, err := overlay.New(ctx, cfg)
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
		state:       newRuntimeState(cfg.ID, oldOID, "main", oldGen),
	}
	svc := &Service{
		logger:               logger,
		git:                  git,
		running:              map[model.RepoID]*repoRuntime{cfg.ID: rt},
		headReconcileBackoff: backoff,
	}
	f := &headReconcileFixture{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		workDir: workDir,
		oldOID:  oldOID,
		nextOID: nextOID,
		lastOID: lastOID,
		oldGen:  oldGen,
		svc:     svc,
		rt:      rt,
		logs:    logs,
	}
	go func() {
		defer close(f.done)
		svc.headReconcileLoop(rt)
	}()
	t.Cleanup(func() {
		cancel()
		<-f.done
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
		return f.rt.state.CurrentHEADOID == oid &&
			f.rt.state.CurrentHEADRef == ref &&
			f.rt.state.SnapshotGeneration == generation &&
			f.rt.resolver.Generation() == generation
	})
}

func TestHeadReconcileRetriesTransientResolveFailureWithoutAnotherSignal(t *testing.T) {
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
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	if err := f.rt.overlay.Close(); err != nil {
		t.Fatal(err)
	}

	f.signal()
	f.waitForLog("overlay reconcile failed")
	recovered, err := overlay.New(f.ctx, f.rt.cfg)
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

func TestHeadReconcileRestoresPersistedStateWhenHEADReturnsAfterOverlayFailure(t *testing.T) {
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
	recovered, err := overlay.New(f.ctx, f.rt.cfg)
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
	status := f.svc.readPersistedStatus(f.ctx, cfg)
	if status.CurrentHEADOID != f.oldOID || status.CurrentHEADRef != "main" || status.SnapshotGeneration != f.oldGen+2 {
		t.Fatalf("persisted status after rollback = %+v", status)
	}
}

func TestHeadReconcilePersistsSameOIDRefBeforeUpdatingRuntime(t *testing.T) {
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
	gotRef := f.rt.state.CurrentHEADRef
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
	<-f.done
}

func TestHeadReconcileResumesAfterUnmountFailure(t *testing.T) {
	f := newHeadReconcileFixture(t)
	mfs := &delayedUnmountFailure{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	f.svc.mu.Lock()
	f.rt.mfs = mfs
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
	f := newHeadReconcileFixture(t)
	repo := f.rt.cfg
	repo.BlobCacheDir = t.TempDir()
	h := hydrator.New(f.svc.git)
	h.Start(1, repo)
	t.Cleanup(h.Stop)
	fs := fusefs.NewArtifactFuse(repo, f.rt.resolver, &fusefs.Engine{Resolver: f.rt.resolver, Repo: repo, Overlay: f.rt.overlay, Hydrator: h})
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
