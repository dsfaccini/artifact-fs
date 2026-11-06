//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/dsfaccini/fuse/fuseops"
)

type localOverlayFuseFixture struct {
	store     *overlaystore.Store
	resolver  *Resolver
	engine    *Engine
	fs        *ArtifactFuse
	srcInode  fuseops.InodeID
	nestInode fuseops.InodeID
	fileInode fuseops.InodeID
}

func newLocalOverlayFuseFixture(t *testing.T) localOverlayFuseFixture {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	repo := model.RepoConfig{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	store, err := overlaystore.New(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, path := range []string{"src", "src/nested"} {
		if err := store.Mkdir(ctx, path, 0o755); err != nil {
			t.Fatalf("Mkdir(%q): %v", path, err)
		}
	}
	if _, err := store.CreateFile(ctx, "src/nested/value.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteFile(ctx, "src/nested/value.txt", 0, []byte("overlay content")); err != nil {
		t.Fatal(err)
	}
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{}, kids: map[string][]model.BaseNode{}}, Overlay: store}
	resolver.SetGeneration(1)
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: store}
	fs := NewArtifactFuse(repo, engine)
	lookup := func(parent fuseops.InodeID, name string) fuseops.InodeID {
		op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
		if err := fs.LookUpInode(ctx, op); err != nil {
			t.Fatalf("LookUpInode(%q): %v", name, err)
		}
		return op.Entry.Child
	}
	srcInode := lookup(fuseops.RootInodeID, "src")
	nestInode := lookup(srcInode, "nested")
	fileInode := lookup(nestInode, "value.txt")
	return localOverlayFuseFixture{
		store: store, resolver: resolver, engine: engine, fs: fs,
		srcInode: srcInode, nestInode: nestInode, fileInode: fileInode,
	}
}

func TestCanceledContextOpensOverlayFile(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	open := &fuseops.OpenFileOp{Inode: f.fileInode}
	if err := f.fs.OpenFile(ctx, open); err != nil {
		t.Fatalf("OpenFile with canceled request context: %v", err)
	}
	if open.Handle == 0 {
		t.Fatal("OpenFile returned an empty handle")
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	})
	read := &fuseops.ReadFileOp{Handle: open.Handle, Size: 64}
	if err := f.fs.ReadFile(context.Background(), read); err != nil {
		t.Fatalf("ReadFile after OpenFile: %v", err)
	}
	if got := string(read.Data[0]); got != "overlay content" {
		t.Fatalf("file handle content = %q, want overlay content", got)
	}
}

func TestReadFileFillsLibraryBuffer(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	open := &fuseops.OpenFileOp{Inode: f.fileInode}
	if err := f.fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	})
	read := &fuseops.ReadFileOp{Handle: open.Handle, Offset: 8, Size: 64, Dst: make([]byte, 64)}
	if err := f.fs.ReadFile(context.Background(), read); err != nil {
		t.Fatal(err)
	}
	if read.Data != nil {
		t.Fatalf("ReadFile set Data = %q, want the read in Dst", read.Data)
	}
	if got := string(read.Dst[:read.BytesRead]); got != "content" {
		t.Fatalf("Dst[:BytesRead] = %q, want content", got)
	}
}

func TestCanceledContextOpensOverlayDirectory(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	open := &fuseops.OpenDirOp{Inode: f.nestInode}
	if err := f.fs.OpenDir(ctx, open); err != nil {
		t.Fatalf("OpenDir with canceled request context: %v", err)
	}
	if open.Handle == 0 {
		t.Fatal("OpenDir returned an empty handle")
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: open.Handle})
	})
	read := &fuseops.ReadDirOp{Handle: open.Handle, Dst: make([]byte, 4096)}
	if err := f.fs.ReadDir(ctx, read); err != nil {
		t.Fatalf("ReadDir with canceled request context: %v", err)
	}
	dir, err := f.fs.dirHandle(open.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if len(dir.entries) != 1 || dir.entries[0].Name != "value.txt" {
		t.Fatalf("directory entries = %+v, want [value.txt]", dir.entries)
	}
}

func TestCanceledContextRenamesOverlayDirectory(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fileOpen := &fuseops.OpenFileOp{Inode: f.fileInode}
	if err := f.fs.OpenFile(context.Background(), fileOpen); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: fileOpen.Handle})
	})
	dirOpen := &fuseops.OpenDirOp{Inode: f.nestInode}
	if err := f.fs.OpenDir(context.Background(), dirOpen); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: dirOpen.Handle})
	})
	op := &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src", NewParent: fuseops.RootInodeID, NewName: "dst"}
	if err := f.fs.Rename(ctx, op); err != nil {
		t.Fatalf("Rename with canceled request context: %v", err)
	}
	if _, err := f.resolver.ResolvePath("src"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source resolve error = %v, want not exist", err)
	}
	if _, err := f.resolver.ResolvePath("dst/nested/value.txt"); err != nil {
		t.Fatalf("destination resolve: %v", err)
	}
	if ref, err := f.fs.requireInode(f.srcInode, syscall.ESTALE); err != nil || ref.Path != "dst" {
		t.Fatalf("moved source inode = %+v, err = %v", ref, err)
	}
	if ref, err := f.fs.requireInode(f.fileInode, syscall.ESTALE); err != nil || ref.Path != "dst/nested/value.txt" {
		t.Fatalf("moved file inode = %+v, err = %v", ref, err)
	}
	fileHandle, err := f.fs.fileHandle(fileOpen.Handle)
	if err != nil {
		t.Fatal(err)
	}
	fileHandle.mu.Lock()
	handlePath := fileHandle.path
	fileHandle.mu.Unlock()
	if handlePath != "dst/nested/value.txt" {
		t.Fatalf("moved file handle path = %q", handlePath)
	}
	dirHandle, err := f.fs.dirHandle(dirOpen.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if dirHandle.inode.Path != "dst/nested" {
		t.Fatalf("moved directory handle path = %q", dirHandle.inode.Path)
	}
	read := &fuseops.ReadFileOp{Handle: fileOpen.Handle, Size: 64}
	if err := f.fs.ReadFile(context.Background(), read); err != nil {
		t.Fatal(err)
	}
	if got := string(read.Data[0]); got != "overlay content" {
		t.Fatalf("moved file handle content = %q, want overlay content", got)
	}
}

type cancelAtRenameTreeOverlay struct {
	model.OverlayStore
	cancel context.CancelFunc
	called bool
}

func (o *cancelAtRenameTreeOverlay) RenameTree(ctx context.Context, oldPath, newPath string, sourceBasePaths, destinationBasePaths []string) error {
	o.called = true
	o.cancel()
	return o.OverlayStore.RenameTree(ctx, oldPath, newPath, sourceBasePaths, destinationBasePaths)
}

type failingLookupOverlay struct {
	model.OverlayStore
	err error
}

func (o *failingLookupOverlay) Lookup(context.Context, string) (model.OverlayEntry, bool, error) {
	return model.OverlayEntry{}, false, o.err
}

func TestCanceledDuringRenameTreeKeepsNamespaceAndHandlesInSync(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	overlay := &cancelAtRenameTreeOverlay{OverlayStore: f.store, cancel: cancel}
	f.resolver.Overlay = overlay
	f.engine.Overlay = overlay
	fileOpen := &fuseops.OpenFileOp{Inode: f.fileInode}
	if err := f.fs.OpenFile(context.Background(), fileOpen); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: fileOpen.Handle})
	})
	dirOpen := &fuseops.OpenDirOp{Inode: f.nestInode}
	if err := f.fs.OpenDir(context.Background(), dirOpen); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: dirOpen.Handle})
	})
	op := &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src", NewParent: fuseops.RootInodeID, NewName: "dst"}
	if err := f.fs.Rename(ctx, op); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if !overlay.called || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("RenameTree called=%t, ctx.Err()=%v", overlay.called, ctx.Err())
	}
	if _, err := f.resolver.ResolvePath("src"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source resolve error = %v, want not exist", err)
	}
	if _, err := f.resolver.ResolvePath("dst/nested/value.txt"); err != nil {
		t.Fatalf("destination resolve: %v", err)
	}
	if ref, err := f.fs.requireInode(f.srcInode, syscall.ESTALE); err != nil || ref.Path != "dst" {
		t.Fatalf("source inode = %+v, err = %v", ref, err)
	}
	if ref, err := f.fs.requireInode(f.fileInode, syscall.ESTALE); err != nil || ref.Path != "dst/nested/value.txt" {
		t.Fatalf("file inode = %+v, err = %v", ref, err)
	}
	fileHandle, err := f.fs.fileHandle(fileOpen.Handle)
	if err != nil {
		t.Fatal(err)
	}
	fileHandle.mu.Lock()
	handlePath := fileHandle.path
	fileHandle.mu.Unlock()
	if handlePath != "dst/nested/value.txt" {
		t.Fatalf("file handle path = %q", handlePath)
	}
	dirHandle, err := f.fs.dirHandle(dirOpen.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if dirHandle.inode.Path != "dst/nested" {
		t.Fatalf("directory handle path = %q", dirHandle.inode.Path)
	}
}

func TestCanceledHydrationInterruptsRenameBeforeMutation(t *testing.T) {
	tmp := t.TempDir()
	repo := model.RepoConfig{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	store, err := overlaystore.New(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 4}
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{base.Path: base}}, Overlay: store}
	resolver.SetGeneration(1)
	hydrator := &blockingRenameHydrator{
		fakeBatchHydrator: &fakeBatchHydrator{},
		started:           make(chan struct{}),
		proceed:           make(chan struct{}),
	}
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: store, Hydrator: hydrator}
	fs := NewArtifactFuse(repo, engine)
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: base.Path}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- fs.Rename(ctx, &fuseops.RenameOp{
			OldParent: fuseops.RootInodeID, OldName: "file.txt",
			NewParent: fuseops.RootInodeID, NewName: "renamed.txt",
		})
	}()
	<-hydrator.started
	cancel()
	if err := <-done; kernelErrno(err) != syscall.EINTR {
		t.Fatalf("Rename after canceled hydration = %v, want EINTR", err)
	}
	if _, err := resolver.ResolvePath("file.txt"); err != nil {
		t.Fatalf("source resolve after interrupted rename: %v", err)
	}
	if _, err := resolver.ResolvePath("renamed.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination resolve after interrupted rename = %v, want not exist", err)
	}
	entries, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("interrupted rename left overlay entries: %+v", entries)
	}
	if got := fs.pathToInode["file.txt"]; got != lookup.Entry.Child {
		t.Fatalf("source inode mapping = %d, want %d", got, lookup.Entry.Child)
	}
	if _, ok := fs.pathToInode["renamed.txt"]; ok {
		t.Fatal("interrupted rename allocated a destination inode mapping")
	}
}

func TestCanceledDestinationHydrationInterruptsRenameBeforeMutation(t *testing.T) {
	tmp := t.TempDir()
	repo := model.RepoConfig{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	store, err := overlaystore.New(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sourceBlob := filepath.Join(tmp, "source-blob")
	if err := os.WriteFile(sourceBlob, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "source-blob", SizeState: "known", SizeBytes: 4}
	destination := model.BaseNode{RepoID: repo.ID, Path: "renamed.txt", Type: "file", Mode: 0o100644, ObjectOID: "destination-blob", SizeState: "known", SizeBytes: 4}
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{source.Path: source, destination.Path: destination}}, Overlay: store}
	resolver.SetGeneration(1)
	destinationBlob := filepath.Join(tmp, "destination-blob")
	if err := os.WriteFile(destinationBlob, []byte("dest"), 0o644); err != nil {
		t.Fatal(err)
	}
	hydrator := &fakeBatchHydrator{pathsByOID: map[string]string{source.ObjectOID: sourceBlob, destination.ObjectOID: destinationBlob}}
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: store, Hydrator: hydrator}
	fs := NewArtifactFuse(repo, engine)
	// A handle open on the destination makes Rename copy the destination into the overlay.
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: destination.Path}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	open := &fuseops.OpenFileOp{Inode: lookup.Entry.Child}
	if err := fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	})
	// The source blob is cached; the destination fetch observes the canceled request.
	hydrator.errorsByOID = map[string]error{destination.ObjectOID: context.Canceled}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = fs.Rename(ctx, &fuseops.RenameOp{
		OldParent: fuseops.RootInodeID, OldName: source.Path,
		NewParent: fuseops.RootInodeID, NewName: destination.Path,
	})
	if kernelErrno(err) != syscall.EINTR {
		t.Fatalf("Rename after canceled destination hydration = %v, want EINTR", err)
	}
	for _, path := range []string{source.Path, destination.Path} {
		if n, err := resolver.ResolvePath(path); err != nil || n.FromOverlay {
			t.Fatalf("%s after interrupted rename = (%+v, %v), want base entry", path, n, err)
		}
	}
	entries, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("interrupted rename left overlay entries: %+v", entries)
	}
}

func TestCanceledHydrationInterruptsOpenFile(t *testing.T) {
	tmp := t.TempDir()
	repo := model.RepoConfig{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	store, err := overlaystore.New(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 4}
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{base.Path: base}}, Overlay: store}
	resolver.SetGeneration(1)
	hydrator := &blockingRenameHydrator{
		fakeBatchHydrator: &fakeBatchHydrator{},
		started:           make(chan struct{}),
		proceed:           make(chan struct{}),
	}
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: store, Hydrator: hydrator}
	fs := NewArtifactFuse(repo, engine)
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: base.Path}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: lookup.Entry.Child})
	if kernelErrno(err) != syscall.EINTR {
		t.Fatalf("OpenFile after canceled hydration = %v, want EINTR", err)
	}
	select {
	case <-hydrator.started:
	default:
		t.Fatal("OpenFile did not reach hydration")
	}
	if len(fs.fileHandles) != 0 {
		t.Fatalf("canceled OpenFile allocated handles: %+v", fs.fileHandles)
	}
}

func TestCanceledContextDoesNotHideOverlayStorageFailure(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	storageErr := errors.New("overlay database unavailable")
	failing := &failingLookupOverlay{OverlayStore: f.store, err: storageErr}
	f.resolver.Overlay = failing
	f.engine.Overlay = failing
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: f.fileInode})
	if kernelErrno(err) != syscall.EIO || !errors.Is(err, storageErr) {
		t.Fatalf("OpenFile storage failure = %v, want EIO wrapping the storage error", err)
	}
}

func TestInodeAttrsPreservesSeparateTimes(t *testing.T) {
	mtime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctime := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)

	attr := inodeAttrs(0o644, 12, "file", mtime, ctime)
	if !attr.Atime.Equal(mtime) {
		t.Fatalf("atime = %v, want %v", attr.Atime, mtime)
	}
	if !attr.Mtime.Equal(mtime) {
		t.Fatalf("mtime = %v, want %v", attr.Mtime, mtime)
	}
	if !attr.Ctime.Equal(ctime) {
		t.Fatalf("ctime = %v, want %v", attr.Ctime, ctime)
	}
}

func TestInodeAttrsPreservesExplicitZeroDirMode(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	attr := inodeAttrs(0, 4096, "dir", now, now)
	if attr.Mode.Perm() != 0 {
		t.Fatalf("mode perms = %#o, want 0", attr.Mode.Perm())
	}
	if !attr.Mode.IsDir() {
		t.Fatalf("expected directory mode, got %#o", attr.Mode)
	}
}

func TestGitFileAttrsUsesOneTimestamp(t *testing.T) {
	fs := NewArtifactFuse(model.RepoConfig{GitDir: "/tmp/repo/.git"}, nil)

	attr := fs.gitFileAttrs()
	if attr.Mtime.IsZero() || attr.Atime.IsZero() || attr.Ctime.IsZero() {
		t.Fatalf("expected non-zero times: atime=%v mtime=%v ctime=%v", attr.Atime, attr.Mtime, attr.Ctime)
	}
	if !attr.Atime.Equal(attr.Mtime) || !attr.Ctime.Equal(attr.Mtime) {
		t.Fatalf("expected .git attrs to use one timestamp: atime=%v mtime=%v ctime=%v", attr.Atime, attr.Mtime, attr.Ctime)
	}
}

func TestRootInodeAttributesDoNotRequireResolver(t *testing.T) {
	fs := NewArtifactFuse(model.RepoConfig{Name: "repo", GitDir: "/tmp/repo.git"}, nil)
	op := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}

	if err := fs.GetInodeAttributes(context.Background(), op); err != nil {
		t.Fatalf("GetInodeAttributes(root): %v", err)
	}
	if !op.Attributes.Mode.IsDir() {
		t.Fatalf("root mode = %#o, want directory", op.Attributes.Mode)
	}
	if op.Attributes.Size == 0 {
		t.Fatal("root size = 0, want non-zero placeholder size")
	}
}

func TestSyntheticAttributesRemainStable(t *testing.T) {
	ctx := context.Background()
	f := newLocalOverlayFuseFixture(t)
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: ".git"}
	if err := f.fs.LookUpInode(ctx, lookup); err != nil {
		t.Fatal(err)
	}
	gitfile := &fuseops.GetInodeAttributesOp{Inode: lookup.Entry.Child}
	if err := f.fs.GetInodeAttributes(ctx, gitfile); err != nil {
		t.Fatal(err)
	}
	if gitfile.Attributes != lookup.Entry.Attributes {
		t.Fatal("synthetic Git file attributes changed between lookup and stat")
	}

	fs := NewArtifactFuse(model.RepoConfig{Name: "repo", GitDir: "/tmp/repo.git"}, nil)
	statRoot := func() fuseops.InodeAttributes {
		t.Helper()
		op := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}
		if err := fs.GetInodeAttributes(ctx, op); err != nil {
			t.Fatal(err)
		}
		return op.Attributes
	}
	if statRoot() != statRoot() {
		t.Fatal("fallback root attributes changed between stats")
	}
}

func TestDetachedFileHandleRemainsReadableWritableAndSyncable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unlinked")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fh := &FileHandle{path: "file.txt", cacheFile: f, cacheGeneration: -1, detached: true}
	fs := &ArtifactFuse{fileHandles: map[fuseops.HandleID]*FileHandle{1: fh}}

	write := &fuseops.WriteFileOp{Handle: 1, Offset: 0, Data: []byte("after!")}
	if err := fs.WriteFile(context.Background(), write); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	read := &fuseops.ReadFileOp{Handle: 1, Offset: 0, Size: 6}
	if err := fs.ReadFile(context.Background(), read); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(read.Data[0]); got != "after!" {
		t.Fatalf("read = %q, want after!", got)
	}
	if err := fs.SyncFile(context.Background(), &fuseops.SyncFileOp{Handle: 1}); err != nil {
		t.Fatalf("SyncFile: %v", err)
	}
	if err := fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: 1}); err != nil {
		t.Fatalf("ReleaseFileHandle: %v", err)
	}
}

func TestWriteThroughOpenedBaseHandlePromotesAndWrites(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "known", SizeBytes: 4}
	snap := &fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(snap, ov)
	h := &fakeLookupHydrator{size: 4, path: cachePath}
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: ov, Hydrator: h}
	fs := NewArtifactFuse(repo, engine)
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	open := &fuseops.OpenFileOp{Inode: lookup.Entry.Child}
	if err := fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Handle: open.Handle, Data: []byte("local")}); err != nil {
		t.Fatal(err)
	}
	if ov.writes != 1 || string(ov.data) != "local" {
		t.Fatalf("overlay writes = %d, data = %q", ov.writes, ov.data)
	}
}

func TestSetInodeAttributesAppliesExecutableBit(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{RepoID: repo.ID, Path: "script.sh", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 4}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{base.Path: base}}, ov)
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: ov, Hydrator: &fakeLookupHydrator{size: 4, path: cachePath}}
	fs := NewArtifactFuse(repo, engine)
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: base.Path}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o755)
	op := &fuseops.SetInodeAttributesOp{Inode: lookup.Entry.Child, Mode: &mode}
	if err := fs.SetInodeAttributes(context.Background(), op); err != nil {
		t.Fatalf("SetInodeAttributes: %v", err)
	}
	if op.Attributes.Mode.Perm() != 0o755 {
		t.Fatalf("returned mode = %#o, want 0755", op.Attributes.Mode.Perm())
	}
	entry, ok := ov.entries[base.Path]
	if !ok || entry.Mode != 0o100755 {
		t.Fatalf("overlay entry = %+v, want promoted executable file", entry)
	}
}

func TestCreateAndReadOverlaySymlink(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: ov}
	fs := NewArtifactFuse(repo, engine)
	create := &fuseops.CreateSymlinkOp{
		Parent: fuseops.RootInodeID,
		Name:   "readme-link",
		Target: "README.md",
	}
	if err := fs.CreateSymlink(context.Background(), create); err != nil {
		t.Fatalf("CreateSymlink: %v", err)
	}
	if create.Entry.Attributes.Mode&os.ModeSymlink == 0 {
		t.Fatalf("created mode = %#o, want symlink", create.Entry.Attributes.Mode)
	}
	read := &fuseops.ReadSymlinkOp{Inode: create.Entry.Child}
	if err := fs.ReadSymlink(context.Background(), read); err != nil {
		t.Fatalf("ReadSymlink: %v", err)
	}
	if read.Target != create.Target {
		t.Fatalf("target = %q, want %q", read.Target, create.Target)
	}
}

func TestMoveInodePathPreservesSourceIdentityAndStalesReplacement(t *testing.T) {
	fs := NewArtifactFuse(model.RepoConfig{}, nil)
	fs.mu.Lock()
	source := fs.allocInode("source.txt", "file", 0o100644, 1)
	replaced := fs.allocInode("destination.txt", "file", 0o100644, 1)
	fs.mu.Unlock()

	fs.moveInodePath("source.txt", "destination.txt")

	moved, err := fs.requireInode(source.ID, syscall.ESTALE)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Path != "destination.txt" {
		t.Fatalf("source inode path = %q, want destination.txt", moved.Path)
	}
	if got := fs.pathToInode["destination.txt"]; got != source.ID {
		t.Fatalf("destination inode = %d, want source %d", got, source.ID)
	}
	if _, err := fs.requireInode(replaced.ID, syscall.ESTALE); err != syscall.ESTALE {
		t.Fatalf("replaced inode error = %v, want ESTALE", err)
	}

	if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: replaced.ID, N: 1}); err != nil {
		t.Fatal(err)
	}
	if got := fs.pathToInode["destination.txt"]; got != source.ID {
		t.Fatalf("forgetting replaced inode removed source mapping: got %d, want %d", got, source.ID)
	}
}

func TestMoveInodePathRewritesDirectoryDescendants(t *testing.T) {
	fs := NewArtifactFuse(model.RepoConfig{}, nil)
	fs.mu.Lock()
	source := fs.allocInode("source", "dir", 0o755, 1)
	child := fs.allocInode("source/nested/file.txt", "file", 0o100644, 1)
	sibling := fs.allocInode("source-other/file.txt", "file", 0o100644, 1)
	replaced := fs.allocInode("destination", "dir", 0o755, 1)
	replacedChild := fs.allocInode("destination/nested/file.txt", "file", 0o100644, 1)
	fs.dirHandles[1] = &DirHandle{inode: &InodeRef{Path: "source/nested"}}
	fs.fileHandles[2] = &FileHandle{path: "source/nested/file.txt"}
	fs.mu.Unlock()

	fs.moveInodePath("source", "destination")
	fs.moveOpenHandles("source", "destination")

	for id, want := range map[fuseops.InodeID]string{
		source.ID:  "destination",
		child.ID:   "destination/nested/file.txt",
		sibling.ID: "source-other/file.txt",
	} {
		got, err := fs.requireInode(id, syscall.ESTALE)
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != want {
			t.Fatalf("inode %d path = %q, want %q", id, got.Path, want)
		}
	}
	if _, err := fs.requireInode(replaced.ID, syscall.ESTALE); err != syscall.ESTALE {
		t.Fatalf("replaced inode error = %v, want ESTALE", err)
	}
	if _, err := fs.requireInode(replacedChild.ID, syscall.ESTALE); err != syscall.ESTALE {
		t.Fatalf("replaced child inode error = %v, want ESTALE", err)
	}
	if got := fs.pathToInode["destination/nested/file.txt"]; got != child.ID {
		t.Fatalf("destination child inode = %d, want source child %d", got, child.ID)
	}
	if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: replacedChild.ID, N: 1}); err != nil {
		t.Fatal(err)
	}
	if got := fs.pathToInode["destination/nested/file.txt"]; got != child.ID {
		t.Fatalf("forgetting replaced child removed source child mapping: got %d, want %d", got, child.ID)
	}
	if got := fs.dirHandles[1].inode.Path; got != "destination/nested" {
		t.Fatalf("directory handle path = %q", got)
	}
	if got := fs.fileHandles[2].path; got != "destination/nested/file.txt" {
		t.Fatalf("file handle path = %q", got)
	}
}

func TestRenameReturnsNativeErrorsBeforeMutatingOverlay(t *testing.T) {
	snapshot := &fakeSnapshot{nodes: map[string]model.BaseNode{
		"src-dir":       {Path: "src-dir", Type: "dir", Mode: 0o40000},
		"src-dir/child": {Path: "src-dir/child", Type: "dir", Mode: 0o40000},
		"dst-file":      {Path: "dst-file", Type: "file", Mode: 0o100644},
		"src-file":      {Path: "src-file", Type: "file", Mode: 0o100644},
		"dst-dir":       {Path: "dst-dir", Type: "dir", Mode: 0o40000},
	}}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(snapshot, overlay)
	engine := &Engine{Resolver: resolver, Overlay: overlay}
	fs := NewArtifactFuse(model.RepoConfig{ID: "repo"}, engine)

	for _, test := range []struct {
		name string
		op   fuseops.RenameOp
		want error
	}{
		{
			name: "directory_over_file",
			op:   fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src-dir", NewParent: fuseops.RootInodeID, NewName: "dst-file"},
			want: syscall.ENOTDIR,
		},
		{
			name: "file_over_directory",
			op:   fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src-file", NewParent: fuseops.RootInodeID, NewName: "dst-dir"},
			want: syscall.EISDIR,
		},
		{
			name: "missing_same_path",
			op:   fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "missing", NewParent: fuseops.RootInodeID, NewName: "missing"},
			want: syscall.ENOENT,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := fs.Rename(context.Background(), &test.op); kernelErrno(err) != test.want {
				t.Fatalf("Rename error = %v, want %v", err, test.want)
			}
		})
	}

	sourceLookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "src-dir"}
	if err := fs.LookUpInode(context.Background(), sourceLookup); err != nil {
		t.Fatal(err)
	}
	childLookup := &fuseops.LookUpInodeOp{Parent: sourceLookup.Entry.Child, Name: "child"}
	if err := fs.LookUpInode(context.Background(), childLookup); err != nil {
		t.Fatal(err)
	}
	selfDescendant := &fuseops.RenameOp{
		OldParent: fuseops.RootInodeID,
		OldName:   "src-dir",
		NewParent: childLookup.Entry.Child,
		NewName:   "moved",
	}
	if err := fs.Rename(context.Background(), selfDescendant); kernelErrno(err) != syscall.EINVAL {
		t.Fatalf("Rename into descendant error = %v, want EINVAL", err)
	}
	if len(overlay.entries) != 0 {
		t.Fatalf("rejected renames mutated overlay: %+v", overlay.entries)
	}
}

func TestInodeAndHandleCreationWaitForNamespaceMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*ArtifactFuse) error
	}{
		{name: "lookup", call: func(fs *ArtifactFuse) error {
			return fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{})
		}},
		{name: "opendir", call: func(fs *ArtifactFuse) error {
			return fs.OpenDir(context.Background(), &fuseops.OpenDirOp{})
		}},
		{name: "readdirplus", call: func(fs *ArtifactFuse) error {
			return fs.ReadDirPlus(context.Background(), &fuseops.ReadDirPlusOp{})
		}},
		{name: "open", call: func(fs *ArtifactFuse) error {
			return fs.OpenFile(context.Background(), &fuseops.OpenFileOp{})
		}},
		{name: "setattr", call: func(fs *ArtifactFuse) error {
			return fs.SetInodeAttributes(context.Background(), &fuseops.SetInodeAttributesOp{})
		}},
		{name: "mkdir", call: func(fs *ArtifactFuse) error {
			return fs.MkDir(context.Background(), &fuseops.MkDirOp{})
		}},
		{name: "rmdir", call: func(fs *ArtifactFuse) error {
			return fs.RmDir(context.Background(), &fuseops.RmDirOp{})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := &ArtifactFuse{}
			fs.handleOps.Lock()
			done := make(chan struct{})
			go func() {
				_ = test.call(fs)
				close(done)
			}()
			select {
			case <-done:
				t.Fatal("operation did not wait for namespace mutation")
			case <-time.After(25 * time.Millisecond):
			}
			fs.handleOps.Unlock()
			<-done
		})
	}
}

func TestRootInodeAttributesUseStableResolverAttrsWhenReady(t *testing.T) {
	// The synctest clock moves exactly as far as the sleep, so attributes read
	// from the clock would differ between the two calls.
	synctest.Test(t, func(t *testing.T) {
		resolver := &Resolver{
			Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{
				".": {Path: ".", Type: "dir", Mode: 0o755, SizeBytes: 4096},
			}},
			Overlay: &fakeOverlay{entries: map[string]model.OverlayEntry{}},
		}
		resolver.SetGeneration(7)
		resolver.SetCommitTime(1_700_000_000)
		fs := NewArtifactFuse(model.RepoConfig{Name: "repo", GitDir: "/tmp/repo.git"}, &Engine{Resolver: resolver})

		first := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}
		if err := fs.GetInodeAttributes(context.Background(), first); err != nil {
			t.Fatalf("first GetInodeAttributes(root): %v", err)
		}
		time.Sleep(2 * time.Millisecond)
		second := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}
		if err := fs.GetInodeAttributes(context.Background(), second); err != nil {
			t.Fatalf("second GetInodeAttributes(root): %v", err)
		}

		want := time.Unix(1_700_000_000, 0)
		if !first.Attributes.Mtime.Equal(want) || !second.Attributes.Mtime.Equal(want) {
			t.Fatalf("root mtime = %v then %v, want stable %v", first.Attributes.Mtime, second.Attributes.Mtime, want)
		}
		if !first.Attributes.Ctime.Equal(second.Attributes.Ctime) {
			t.Fatalf("root ctime changed: %v then %v", first.Attributes.Ctime, second.Attributes.Ctime)
		}
	})
}

func TestLookUpInodeHydratesUnknownSizeBaseFileAttributes(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{
		RepoID:    repo.ID,
		Path:      "file.txt",
		Type:      "file",
		Mode:      0o100644,
		ObjectOID: "blob",
		SizeState: "unknown",
	}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}

	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatalf("LookUpInode: %v", err)
	}
	if op.Entry.Attributes.Size != uint64(h.size) {
		t.Fatalf("lookup size = %d, want hydrated size %d", op.Entry.Attributes.Size, h.size)
	}
	if h.calls != 1 {
		t.Fatalf("EnsureHydrated calls = %d, want 1", h.calls)
	}
}

func TestGetInodeAttributesHydratesUnknownSizeBaseFileAttributes(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{
		RepoID:    repo.ID,
		Path:      "file.txt",
		Type:      "file",
		Mode:      0o100644,
		ObjectOID: "blob",
		SizeState: "unknown",
	}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatalf("LookUpInode: %v", err)
	}
	h.calls = 0

	op := &fuseops.GetInodeAttributesOp{Inode: lookup.Entry.Child}
	if err := fs.GetInodeAttributes(context.Background(), op); err != nil {
		t.Fatalf("GetInodeAttributes: %v", err)
	}
	if op.Attributes.Size != uint64(h.size) {
		t.Fatalf("getattr size = %d, want hydrated size %d", op.Attributes.Size, h.size)
	}
	if h.calls != 1 {
		t.Fatalf("EnsureHydrated calls = %d, want 1", h.calls)
	}
}

func TestGetInodeAttributesHydrationFailureReturnsEIO(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{
		RepoID:    repo.ID,
		Path:      "file.txt",
		Type:      "file",
		Mode:      0o100644,
		ObjectOID: "blob",
		SizeState: "unknown",
	}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatalf("LookUpInode: %v", err)
	}
	h.err = errors.New("hydrate failed")

	op := &fuseops.GetInodeAttributesOp{Inode: lookup.Entry.Child}
	if err := fs.GetInodeAttributes(context.Background(), op); kernelErrno(err) != syscall.EIO {
		t.Fatalf("GetInodeAttributes err = %v, want EIO", err)
	}
}

func TestLookUpInodeResolvesUnknownBaseAttributeSizes(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	tests := []struct {
		name              string
		base              model.BaseNode
		overlay           map[string]model.OverlayEntry
		want              uint64
		wantHydrateCalls  int
		wantReadBlobCalls int
	}{
		{
			name: "known base file",
			base: model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 0},
			want: 0,
		},
		{
			name: "overlay file",
			base: model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"},
			overlay: map[string]model.OverlayEntry{
				"file.txt": {Path: "file.txt", Kind: model.OverlayKindModify, Mode: 0o644, SizeBytes: 3},
			},
			want: 3,
		},
		{
			name: "base dir",
			base: model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "dir", Mode: 0o40000, ObjectOID: "tree", SizeState: "unknown"},
			want: 4096,
		},
		{
			name:              "base symlink",
			base:              model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "symlink", Mode: 0o120000, ObjectOID: "blob", SizeState: "unknown"},
			want:              uint64(len("package.json")),
			wantReadBlobCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newResolver(
				&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": tt.base}},
				&fakeOverlay{entries: tt.overlay},
			)
			h := &fakeLookupHydrator{size: 12, data: []byte("package.json")}
			fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
			op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}

			if err := fs.LookUpInode(context.Background(), op); err != nil {
				t.Fatalf("LookUpInode: %v", err)
			}
			if op.Entry.Attributes.Size != tt.want {
				t.Fatalf("lookup size = %d, want %d", op.Entry.Attributes.Size, tt.want)
			}
			if h.calls != tt.wantHydrateCalls {
				t.Fatalf("EnsureHydrated calls = %d, want %d", h.calls, tt.wantHydrateCalls)
			}
			if h.readBlobCalls != tt.wantReadBlobCalls {
				t.Fatalf("ReadBlob calls = %d, want %d", h.readBlobCalls, tt.wantReadBlobCalls)
			}
		})
	}
}

func TestFirstFileReadPrefetchesItsDirectoryAndListingPrefetchesNothing(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{
		"src/a.go":      "package a\n",
		"src/b.go":      "package b\n",
		"src/c.go":      "package c\n",
		"docs/guide.md": "# guide\n",
		"top.txt":       "top\n",
	}
	tmp := t.TempDir()
	repo := model.RepoConfig{
		ID:            "repo",
		Name:          "repo",
		GitDir:        filepath.Join(tmp, "repo.git"),
		RemoteURL:     gittest.PartialCloneRemote(t, files),
		Branch:        "main",
		BlobCacheDir:  filepath.Join(tmp, "cache"),
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	git := gitstore.New(nil)
	t.Cleanup(git.Close)
	if err := git.CloneBlobless(ctx, repo); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	head, ref, err := git.ResolveHEAD(ctx, repo)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	nodes, err := git.BuildTreeIndex(ctx, repo, head)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	oids := map[string]string{}
	for _, n := range nodes {
		oids[n.Path] = n.ObjectOID
	}
	snap, err := snapshot.New(ctx, filepath.Join(tmp, "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	gen, err := snap.PublishGeneration(ctx, head, ref, nodes)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := overlaystore.New(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	h := hydrator.New(git)
	t.Cleanup(h.Stop)
	resolver := &Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	fs := NewArtifactFuse(repo, &Engine{Resolver: resolver, Repo: repo, Overlay: ov, Hydrator: h})
	packs := packCount(t, repo.GitDir)

	// The hydrator has not started, so anything a listing queued would still
	// be queued.
	walkListing(t, fs, fuseops.RootInodeID, ".")
	if got := h.QueueDepth(repo.ID); got != 0 {
		t.Fatalf("a listing-only walk queued %d blobs for hydration", got)
	}

	h.Start(4, repo)
	src := lookUpInode(t, fs, fuseops.RootInodeID, "src")
	if got := readWholeFile(t, fs, lookUpInode(t, fs, src, "a.go")); got != files["src/a.go"] {
		t.Fatalf("src/a.go = %q, want %q", got, files["src/a.go"])
	}
	for _, path := range []string{"src/b.go", "src/c.go"} {
		waitForCachedBlob(t, repo, oids[path])
	}
	// One fetch for a.go itself and one for its siblings.
	if got := packCount(t, repo.GitDir); got != packs+2 {
		t.Fatalf("packs = %d after reading src/a.go, want %d", got, packs+2)
	}
	for _, name := range []string{"b.go", "c.go"} {
		if got := readWholeFile(t, fs, lookUpInode(t, fs, src, name)); got != files["src/"+name] {
			t.Fatalf("src/%s = %q, want %q", name, got, files["src/"+name])
		}
	}
	if got := packCount(t, repo.GitDir); got != packs+2 {
		t.Fatalf("packs = %d after reading the prefetched siblings, want %d", got, packs+2)
	}
	for _, path := range []string{"docs/guide.md", "top.txt"} {
		if _, err := os.Stat(filepath.Join(repo.BlobCacheDir, oids[path])); err == nil {
			t.Fatalf("%s was hydrated though nothing in its directory was read", path)
		}
	}
}

func TestReadsPrefetchEachDirectoryOncePerGeneration(t *testing.T) {
	ctx := context.Background()
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := model.BaseNode{Path: "src/a.go", Type: "file", Mode: 0o100644, ObjectOID: "a", SizeState: "unknown"}
	b := model.BaseNode{Path: "src/b.go", Type: "file", Mode: 0o100644, ObjectOID: "b", SizeState: "unknown"}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	r := newResolver(&fakeSnapshot{
		nodes: map[string]model.BaseNode{"src": {Path: "src", Type: "dir", Mode: 0o40755}, a.Path: a, b.Path: b},
		kids:  map[string][]model.BaseNode{"src": {a, b}},
	}, ov)
	h := &fakeBatchHydrator{path: cachePath}
	engine := &Engine{Resolver: r, Repo: model.RepoConfig{ID: "repo"}, Overlay: ov, Hydrator: h}
	dst := make([]byte, 16)

	for _, path := range []string{a.Path, b.Path} {
		if _, err := engineRead(ctx, engine, path, 0, dst); err != nil {
			t.Fatalf("Read(%s): %v", path, err)
		}
	}
	if len(h.tasks) != 2 {
		t.Fatalf("prefetch tasks after two reads in src = %d, want 2", len(h.tasks))
	}
	// A new generation can hold new blobs.
	r.SetGeneration(2)
	if _, err := engineRead(ctx, engine, a.Path, 0, dst); err != nil {
		t.Fatalf("Read(%s) at generation 2: %v", a.Path, err)
	}
	if len(h.tasks) != 4 {
		t.Fatalf("prefetch tasks after a read at a new generation = %d, want 4", len(h.tasks))
	}
}

// newPrefetchFixture serves src/a.go and src/b.go, whose sizes are unknown, from
// a hydrator that records what is queued.
func newPrefetchFixture(t *testing.T) (*ArtifactFuse, *Engine, *fakeBatchHydrator) {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := model.BaseNode{Path: "src", Type: "dir", Mode: 0o40755}
	a := model.BaseNode{Path: "src/a.go", Type: "file", Mode: 0o100644, ObjectOID: "a", SizeState: "unknown"}
	b := model.BaseNode{Path: "src/b.go", Type: "file", Mode: 0o100644, ObjectOID: "b", SizeState: "unknown"}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	r := newResolver(&fakeSnapshot{
		nodes: map[string]model.BaseNode{src.Path: src, a.Path: a, b.Path: b},
		kids:  map[string][]model.BaseNode{".": {src}, "src": {a, b}},
	}, ov)
	h := &fakeBatchHydrator{path: cachePath}
	repo := model.RepoConfig{ID: "repo"}
	engine := &Engine{Resolver: r, Repo: repo, Overlay: ov, Hydrator: h}
	return NewArtifactFuse(repo, engine), engine, h
}

func TestLookupAndGetattrDoNotPrefetch(t *testing.T) {
	fs, _, h := newPrefetchFixture(t)
	src := lookUpInode(t, fs, fuseops.RootInodeID, "src")
	a := lookUpInode(t, fs, src, "a.go")
	if err := fs.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: a}); err != nil {
		t.Fatalf("GetInodeAttributes: %v", err)
	}
	if h.calls == 0 {
		t.Fatal("lookup did not hydrate src/a.go, whose size is unknown")
	}
	if len(h.tasks) != 0 {
		t.Fatalf("lookup and getattr queued %d prefetch tasks for src", len(h.tasks))
	}
}

func TestWriteOnlyOpenDoesNotPrefetch(t *testing.T) {
	fs, _, h := newPrefetchFixture(t)
	ctx := context.Background()
	a := lookUpInode(t, fs, lookUpInode(t, fs, fuseops.RootInodeID, "src"), "a.go")
	open := &fuseops.OpenFileOp{Inode: a, OpenFlags: syscall.O_WRONLY | syscall.O_TRUNC}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatalf("OpenFile(O_WRONLY|O_TRUNC): %v", err)
	}
	_ = fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	if len(h.tasks) != 0 {
		t.Fatalf("a write-only open queued %d prefetch tasks for src", len(h.tasks))
	}
	readWholeFile(t, fs, a)
	if len(h.tasks) != 2 {
		t.Fatalf("a read queued %d prefetch tasks for src, want 2", len(h.tasks))
	}
}

func TestReadsAtTwoGenerationsPrefetchADirectoryOnce(t *testing.T) {
	_, engine, h := newPrefetchFixture(t)
	engine.Resolver.SetGeneration(2)
	// A handle opened before a HEAD change reads at the previous generation.
	for _, gen := range []int64{1, 2, 1, 2} {
		if err := engineBaseCacheFile(context.Background(), engine, "src/a.go", gen); err != nil {
			t.Fatalf("BaseCacheFile at generation %d: %v", gen, err)
		}
	}
	if len(h.tasks) != 2 {
		t.Fatalf("reads at generations 1 and 2 queued %d prefetch tasks, want 2", len(h.tasks))
	}
}

func TestFirstReadHydratesItsBlobBeforeItPrefetches(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		read func(t *testing.T, fs *ArtifactFuse, engine *Engine)
	}{
		{name: "open and read a file", read: func(t *testing.T, fs *ArtifactFuse, _ *Engine) {
			readWholeFile(t, fs, lookUpInode(t, fs, lookUpInode(t, fs, fuseops.RootInodeID, "src"), "a.go"))
		}},
		{name: "read a path", read: func(t *testing.T, _ *ArtifactFuse, engine *Engine) {
			if _, err := engineRead(ctx, engine, "src/a.go", 0, make([]byte, 16)); err != nil {
				t.Fatalf("Read: %v", err)
			}
		}},
		{name: "read through a handle", read: func(t *testing.T, _ *ArtifactFuse, engine *Engine) {
			if err := engineBaseCacheFile(ctx, engine, "src/a.go", 0); err != nil {
				t.Fatalf("BaseCacheFile: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs, engine, h := newPrefetchFixture(t)
			rec := &queuedAtOpenHydrator{fakeBatchHydrator: h}
			engine.Hydrator = rec
			tt.read(t, fs, engine)
			if len(h.tasks) != 2 {
				t.Fatalf("prefetch tasks = %d, want 2", len(h.tasks))
			}
			if len(rec.queuedAtOpen) == 0 || rec.queuedAtOpen[0] != 0 {
				t.Fatalf("prefetch tasks queued when src/a.go started to hydrate = %v, want 0: the read would wait in its directory's speculative batch", rec.queuedAtOpen)
			}
		})
	}
}

// queuedAtOpenHydrator records how many tasks were queued each time a blob
// started to hydrate for an open.
type queuedAtOpenHydrator struct {
	*fakeBatchHydrator
	queuedAtOpen []int
}

func (h *queuedAtOpenHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	h.queuedAtOpen = append(h.queuedAtOpen, len(h.tasks))
	return h.fakeBatchHydrator.OpenHydrated(ctx, repo, node)
}

// engineBaseCacheFile opens and closes path's base content at generation gen,
// as a file handle's read does.
func engineBaseCacheFile(ctx context.Context, e *Engine, path string, gen int64) error {
	return e.Do(ctx, nil, func(ctx context.Context, b *Blobs) error {
		f, _, _, err := e.BaseCacheFile(ctx, b, path, gen)
		if f != nil {
			_ = f.Close()
		}
		return err
	})
}

// walkListing opens and lists every directory under inode, and looks up only
// directories, as find and ls -R do.
func walkListing(t *testing.T, fs *ArtifactFuse, inode fuseops.InodeID, path string) {
	t.Helper()
	ctx := context.Background()
	open := &fuseops.OpenDirOp{Inode: inode}
	if err := fs.OpenDir(ctx, open); err != nil {
		t.Fatalf("OpenDir(%s): %v", path, err)
	}
	if err := fs.ReadDir(ctx, &fuseops.ReadDirOp{Handle: open.Handle, Dst: make([]byte, 64*1024)}); err != nil {
		t.Fatalf("ReadDir(%s): %v", path, err)
	}
	plus := &fuseops.ReadDirPlusOp{}
	plus.Handle = open.Handle
	plus.Dst = make([]byte, 64*1024)
	if err := fs.ReadDirPlus(ctx, plus); err != nil {
		t.Fatalf("ReadDirPlus(%s): %v", path, err)
	}
	if err := fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: open.Handle}); err != nil {
		t.Fatalf("ReleaseDirHandle(%s): %v", path, err)
	}
	entries, _, _, err := fs.engine.ReadDir(ctx, path)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", path, err)
	}
	for _, e := range entries {
		if e.Type == "dir" {
			walkListing(t, fs, lookUpInode(t, fs, inode, e.Name), model.CleanPath(filepath.Join(path, e.Name)))
		}
	}
}

func lookUpInode(t *testing.T, fs *ArtifactFuse, parent fuseops.InodeID, name string) fuseops.InodeID {
	t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatalf("LookUpInode(%s): %v", name, err)
	}
	return op.Entry.Child
}

func readWholeFile(t *testing.T, fs *ArtifactFuse, inode fuseops.InodeID) string {
	t.Helper()
	ctx := context.Background()
	open := &fuseops.OpenFileOp{Inode: inode}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}) }()
	read := &fuseops.ReadFileOp{Handle: open.Handle, Size: 4096, Dst: make([]byte, 4096)}
	if err := fs.ReadFile(ctx, read); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(read.Dst[:read.BytesRead])
}

// waitForCachedBlob polls until oid's blob is in repo's cache; go test
// -timeout is its only bound.
func waitForCachedBlob(t *testing.T, repo model.RepoConfig, oid string) {
	t.Helper()
	for {
		if _, err := os.Stat(filepath.Join(repo.BlobCacheDir, oid)); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func packCount(t *testing.T, gitDir string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

func TestReadDirPlusUsesReaddirMetadataWithoutHydration(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 42}},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	op := &fuseops.ReadDirPlusOp{}
	op.Handle = open.Handle
	op.Dst = make([]byte, 4096)
	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if op.BytesRead == 0 {
		t.Fatal("ReadDirPlus wrote no entries")
	}
	if h.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", h.calls)
	}
	if fs.pathToInode["file.txt"] == 0 {
		t.Fatal("ReadDirPlus did not allocate file inode")
	}
}

func TestReadDirPlusDoesNotHydrateUnknownSizeBaseFile(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"}},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	op := &fuseops.ReadDirPlusOp{}
	op.Handle = open.Handle
	op.Dst = make([]byte, 4096)

	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if op.BytesRead == 0 {
		t.Fatal("ReadDirPlus wrote no entries")
	}
	if h.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", h.calls)
	}
}

func TestReadDirPlusContinuesInItsListingGeneration(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	snap := &generationSnapshot{nodes: map[int64]map[string]model.BaseNode{}, kids: map[int64]map[string][]model.BaseNode{
		1: {".": {
			{Path: "a.txt", Type: "file", Mode: 0o100644, SizeState: "known", SizeBytes: 1},
			{Path: "b.txt", Type: "file", Mode: 0o100644, SizeState: "known", SizeBytes: 1},
		}},
		2: {".": {
			{Path: "c.txt", Type: "file", Mode: 0o100644, SizeState: "known", SizeBytes: 2},
			{Path: "d.txt", Type: "file", Mode: 0o100644, SizeState: "known", SizeBytes: 2},
		}},
	}}
	r := &Resolver{Snapshot: snap, Overlay: &fakeOverlay{entries: map[string]model.OverlayEntry{}}}
	r.SetGeneration(1)
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: &fakeLookupHydrator{}})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	read := func(offset fuseops.DirOffset) {
		t.Helper()
		op := &fuseops.ReadDirPlusOp{}
		op.Handle = open.Handle
		op.Offset = offset
		op.Dst = make([]byte, 4096)
		if err := fs.ReadDirPlus(context.Background(), op); err != nil {
			t.Fatalf("ReadDirPlus: %v", err)
		}
		if op.BytesRead == 0 {
			t.Fatalf("ReadDirPlus at offset %d wrote no entries", offset)
		}
	}
	read(0)
	r.SetGeneration(2)
	// Offset 2 is the root's .git entry plus a.txt.
	read(2)
	if fs.pathToInode["d.txt"] != 0 {
		t.Fatal("continued ReadDirPlus used the live resolver generation")
	}
}

func TestReadDirPlusDropsLookupWhenEntryDoesNotFit(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 42}},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: &fakeLookupHydrator{}})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	op := &fuseops.ReadDirPlusOp{}
	op.Handle = open.Handle
	op.Dst = make([]byte, 1)
	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if op.BytesRead != 0 {
		t.Fatalf("BytesRead = %d, want 0", op.BytesRead)
	}
	if fs.pathToInode[".git"] != 0 {
		t.Fatal("inode lookup leaked for entry that did not fit")
	}
}

type fakeLookupHydrator struct {
	size          int64
	calls         int
	readBlobCalls int
	err           error
	path          string
	data          []byte
}

func (f *fakeLookupHydrator) Enqueue(model.HydrationTask) {}

func (f *fakeLookupHydrator) EnqueueBatch([]model.HydrationTask) {}

func (f *fakeLookupHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, _ model.BaseNode) (string, int64, error) {
	f.calls++
	if f.err != nil {
		return "", 0, f.err
	}
	return f.path, f.size, nil
}

func (f *fakeLookupHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	path, size, err := f.EnsureHydrated(ctx, repo, node)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	return file, size, err
}

func (f *fakeLookupHydrator) ReadBlob(_ context.Context, _ model.RepoConfig, _ model.BaseNode, _ int64) ([]byte, error) {
	f.readBlobCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func (f *fakeLookupHydrator) QueueDepth(model.RepoID) int { return 0 }

// Every backend lists a directory at the first ReadDir and again at each
// ReadDir from offset 0: FSKit reuses a handle across enumerations, and
// rewinddir reads from offset 0 on the same handle.
func TestReadDirListsAtEnumerationStart(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	ctx := context.Background()
	open := &fuseops.OpenDirOp{Inode: f.nestInode}
	if err := f.fs.OpenDir(ctx, open); err != nil {
		t.Fatal(err)
	}
	read := func(offset fuseops.DirOffset) []byte {
		t.Helper()
		op := &fuseops.ReadDirOp{Handle: open.Handle, Offset: offset, Dst: make([]byte, 4096)}
		if err := f.fs.ReadDir(ctx, op); err != nil {
			t.Fatal(err)
		}
		return op.Dst[:op.BytesRead]
	}
	if _, err := f.store.CreateFile(ctx, "src/nested/opened.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	first := read(0)
	if !bytes.Contains(first, []byte("value.txt\x00")) || !bytes.Contains(first, []byte("opened.txt\x00")) {
		t.Fatalf("first enumeration %q omitted a file that existed at the first ReadDir", first)
	}
	if _, err := f.store.CreateFile(ctx, "src/nested/added.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(2); len(got) != 0 {
		t.Fatalf("a continued enumeration changed its listing: %q", got)
	}
	if !bytes.Contains(read(0), []byte("added.txt\x00")) {
		t.Fatal("rewound enumeration omitted the new file")
	}
	if err := f.store.Remove(ctx, "src/nested/value.txt"); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(read(0), []byte("value.txt\x00")) {
		t.Fatal("rewound enumeration includes the deleted file")
	}
	if err := f.fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: open.Handle}); err != nil {
		t.Fatal(err)
	}
}
