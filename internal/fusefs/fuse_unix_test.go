//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
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
	store := newOverlay(t, "src/", "src/nested/")
	writeOverlayFile(t, store, "src/nested/value.txt", "overlay content")
	repo := model.RepoPaths{ID: "repo"}
	resolver := newResolver(&fakeSnapshot{}, store)
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

type failingLookupOverlay struct {
	model.OverlayStore
	err error
}

func (o *failingLookupOverlay) Lookup(context.Context, string) (model.OverlayEntry, bool, error) {
	return model.OverlayEntry{}, false, o.err
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

// A base entry's times are the HEAD commit's, or the generation's while the
// commit time is unknown. An overlay entry keeps its mtime and ctime apart,
// and an overlay directory chmodded to 0 keeps permission bits 0.
func TestGetInodeAttributesReportsEachEntrysTimesAndMode(t *testing.T) {
	f := newCacheFixture(t, baseFile("base.txt", "base", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	getattr := func(inode fuseops.InodeID) fuseops.InodeAttributes {
		t.Helper()
		op := &fuseops.GetInodeAttributesOp{Inode: inode}
		if err := f.fs.GetInodeAttributes(f.ctx, op); err != nil {
			t.Fatal(err)
		}
		return op.Attributes
	}
	base := f.lookup(root, "base.txt")
	for commitTime, want := range map[int64]time.Time{0: time.Unix(f.resolver.Generation(), 0), 1_700_000_000: time.Unix(1_700_000_000, 0)} {
		f.resolver.SetCommitTime(commitTime)
		if got := getattr(base); !got.Mtime.Equal(want) || !got.Ctime.Equal(want) {
			t.Fatalf("base.txt at commit time %d: mtime %v, ctime %v; want both %v", commitTime, got.Mtime, got.Ctime, want)
		}
	}

	create := &fuseops.CreateFileOp{Parent: root, Name: "new.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: create.Entry.Child, Mtime: &mtime}); err != nil {
		t.Fatal(err)
	}
	entry, ok := lookupOverlay(t, f.resolver.Overlay, "new.txt")
	if !ok {
		t.Fatal("new.txt is not in the overlay")
	}
	ctime := time.Unix(0, entry.CtimeUnixNs)
	if got := getattr(create.Entry.Child); !got.Mtime.Equal(mtime) || !got.Atime.Equal(mtime) || !got.Ctime.Equal(ctime) || ctime.Equal(mtime) {
		t.Fatalf("new.txt: atime %v, mtime %v, ctime %v; want atime and mtime %v and the overlay's ctime %v", got.Atime, got.Mtime, got.Ctime, mtime, ctime)
	}

	mkdir := &fuseops.MkDirOp{Parent: root, Name: "dir", Mode: os.ModeDir | 0o755}
	if err := f.fs.MkDir(f.ctx, mkdir); err != nil {
		t.Fatal(err)
	}
	noAccess := os.ModeDir
	if err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: mkdir.Entry.Child, Mode: &noAccess}); err != nil {
		t.Fatal(err)
	}
	if got := getattr(mkdir.Entry.Child).Mode; got != os.ModeDir {
		t.Fatalf("dir mode after chmod 0 = %v, want a directory with permission bits 0", got)
	}
}

func TestGitFileAttrsUsesOneTimestamp(t *testing.T) {
	fs := NewArtifactFuse(model.RepoPaths{GitDir: "/tmp/repo/.git"}, nil)

	attr := fs.gitFileAttrs()
	if attr.Mtime.IsZero() || attr.Atime.IsZero() || attr.Ctime.IsZero() {
		t.Fatalf("expected non-zero times: atime=%v mtime=%v ctime=%v", attr.Atime, attr.Mtime, attr.Ctime)
	}
	if !attr.Atime.Equal(attr.Mtime) || !attr.Ctime.Equal(attr.Mtime) {
		t.Fatalf("expected .git attrs to use one timestamp: atime=%v mtime=%v ctime=%v", attr.Atime, attr.Mtime, attr.Ctime)
	}
}

func TestRootInodeAttributesDoNotRequireResolver(t *testing.T) {
	fs := NewArtifactFuse(model.RepoPaths{Name: "repo", GitDir: "/tmp/repo.git"}, nil)
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

	fs := NewArtifactFuse(model.RepoPaths{Name: "repo", GitDir: "/tmp/repo.git"}, nil)
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

func TestSetInodeAttributesAppliesExecutableBit(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := model.RepoPaths{ID: "repo"}
	base := model.BaseNode{RepoID: repo.ID, Path: "script.sh", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 4}
	ov := newOverlay(t)
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{base.Path: base}}, ov)
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: ov, Hydrator: &fakeHydrator{size: 4, path: cachePath}}
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
	entry, ok := lookupOverlay(t, ov, base.Path)
	if !ok || entry.Mode != 0o100755 {
		t.Fatalf("overlay entry = %+v, want promoted executable file", entry)
	}
}

func TestCreateAndReadOverlaySymlink(t *testing.T) {
	repo := model.RepoPaths{ID: "repo"}
	ov := newOverlay(t)
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
	long := &fuseops.CreateSymlinkOp{Parent: fuseops.RootInodeID, Name: "long-link", Target: strings.Repeat("x", model.MaxSymlinkTargetBytes+1)}
	if err := fs.CreateSymlink(context.Background(), long); kernelErrno(err) != syscall.ENAMETOOLONG {
		t.Fatalf("CreateSymlink of a %d-byte target = %v, want ENAMETOOLONG", len(long.Target), err)
	}
	if _, ok := lookupOverlay(t, ov, "long-link"); ok {
		t.Fatal("the refused symlink reached the overlay")
	}
}

func TestMoveInodePathPreservesSourceIdentityAndStalesReplacement(t *testing.T) {
	fs := NewArtifactFuse(model.RepoPaths{}, nil)
	fs.mu.Lock()
	source := fs.allocInode("source.txt", Attrs{Type: "file", Mode: 0o100644, Gen: 1})
	replaced := fs.allocInode("destination.txt", Attrs{Type: "file", Mode: 0o100644, Gen: 1})
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
	fs := NewArtifactFuse(model.RepoPaths{}, nil)
	fs.mu.Lock()
	source := fs.allocInode("source", Attrs{Type: "dir", Mode: 0o755, Gen: 1})
	child := fs.allocInode("source/nested/file.txt", Attrs{Type: "file", Mode: 0o100644, Gen: 1})
	sibling := fs.allocInode("source-other/file.txt", Attrs{Type: "file", Mode: 0o100644, Gen: 1})
	replaced := fs.allocInode("destination", Attrs{Type: "dir", Mode: 0o755, Gen: 1})
	replacedChild := fs.allocInode("destination/nested/file.txt", Attrs{Type: "file", Mode: 0o100644, Gen: 1})
	fs.dirHandles[1] = &DirHandle{inode: &InodeRef{Path: "source/nested"}}
	fs.fileHandles[2] = &FileHandle{inode: child, path: "source/nested/file.txt"}
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
	overlay := newOverlay(t)
	resolver := newResolver(snapshot, overlay)
	engine := &Engine{Resolver: resolver, Overlay: overlay}
	fs := NewArtifactFuse(model.RepoPaths{ID: "repo"}, engine)

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
	if entries, err := overlay.ListAll(context.Background()); err != nil || len(entries) != 0 {
		t.Fatalf("rejected renames mutated overlay: %+v, %v", entries, err)
	}
}

func TestInodeAndHandleCreationWaitForNamespaceMutation(t *testing.T) {
	for _, test := range []struct {
		op   string
		call func(*ArtifactFuse) error
	}{
		{"LookUpInode", func(fs *ArtifactFuse) error {
			return fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{})
		}},
		{"OpenDir", func(fs *ArtifactFuse) error {
			return fs.OpenDir(context.Background(), &fuseops.OpenDirOp{})
		}},
		{"ReadDirPlus", func(fs *ArtifactFuse) error {
			return fs.ReadDirPlus(context.Background(), &fuseops.ReadDirPlusOp{})
		}},
		{"OpenFile", func(fs *ArtifactFuse) error {
			return fs.OpenFile(context.Background(), &fuseops.OpenFileOp{})
		}},
		{"SetInodeAttributes", func(fs *ArtifactFuse) error {
			return fs.SetInodeAttributes(context.Background(), &fuseops.SetInodeAttributesOp{})
		}},
		{"MkDir", func(fs *ArtifactFuse) error {
			return fs.MkDir(context.Background(), &fuseops.MkDirOp{})
		}},
		{"RmDir", func(fs *ArtifactFuse) error {
			return fs.RmDir(context.Background(), &fuseops.RmDirOp{})
		}},
	} {
		t.Run(test.op, func(t *testing.T) {
			fs := &ArtifactFuse{}
			fs.handleOps.Lock()
			done := make(chan struct{})
			go func() {
				_ = test.call(fs)
				close(done)
			}()
			if !waitParked(done, "sync.(*RWMutex).", "(*Engine).Do", "(*ArtifactFuse)."+test.op) {
				t.Fatal("operation did not wait for namespace mutation")
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
			Overlay: newOverlay(t),
		}
		resolver.SetGeneration(7)
		resolver.SetCommitTime(1_700_000_000)
		fs := NewArtifactFuse(model.RepoPaths{Name: "repo", GitDir: "/tmp/repo.git"}, &Engine{Resolver: resolver})

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

func TestGetInodeAttributesHydratesUnknownSizeBaseFileAttributes(t *testing.T) {
	repo := model.RepoPaths{ID: "repo"}
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
		newOverlay(t),
	)
	h := &fakeHydrator{size: 12}
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
	repo := model.RepoPaths{ID: "repo"}
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
		newOverlay(t),
	)
	h := &fakeHydrator{size: 12}
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
	repo := model.RepoPaths{ID: "repo"}
	tests := []struct {
		name              string
		base              model.BaseNode
		overlay           string
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
			name:             "unknown base file",
			base:             model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"},
			want:             12,
			wantHydrateCalls: 1,
		},
		{
			name:    "overlay file",
			base:    model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"},
			overlay: "abc",
			want:    3,
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
			ov := newOverlay(t)
			if tt.overlay != "" {
				writeOverlayFile(t, ov, "file.txt", tt.overlay)
			}
			r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": tt.base}}, ov)
			h := &fakeHydrator{size: 12, data: []byte("package.json")}
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
		RepoPaths: model.RepoPaths{
			ID:            "repo",
			Name:          "repo",
			GitDir:        filepath.Join(tmp, "repo.git"),
			BlobCacheDir:  filepath.Join(tmp, "cache"),
			OverlayDir:    filepath.Join(tmp, "overlay"),
			OverlayDBPath: filepath.Join(tmp, "overlay.db"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: gittest.PartialCloneRemote(t, files),
			Branch:    "main",
		},
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
	ov, err := overlaystore.New(ctx, repo.RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	h := hydrator.New(git)
	t.Cleanup(h.Stop)
	queue := &enqueueCounter{Hydrator: h}
	resolver := &Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	fs := NewArtifactFuse(repo.RepoPaths, &Engine{Resolver: resolver, Repo: repo.RepoPaths, Overlay: ov, Hydrator: queue})
	packs := packCount(t, repo.GitDir)

	walkListing(t, fs, fuseops.RootInodeID, ".")
	if got := queue.queued.Load(); got != 0 {
		t.Fatalf("a listing-only walk queued %d blobs for hydration", got)
	}

	h.Start(4, repo.RepoPaths)
	src := lookUpInode(t, fs, fuseops.RootInodeID, "src")
	if got := readWholeFile(t, fs, lookUpInode(t, fs, src, "a.go")); got != files["src/a.go"] {
		t.Fatalf("src/a.go = %q, want %q", got, files["src/a.go"])
	}
	for _, path := range []string{"src/b.go", "src/c.go"} {
		waitForCachedBlob(t, repo.RepoPaths, oids[path])
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
		if _, err := os.Stat(blobcache.Path(repo.BlobCacheDir, oids[path])); err == nil {
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
	ov := newOverlay(t)
	r := newResolver(&fakeSnapshot{
		nodes: map[string]model.BaseNode{"src": {Path: "src", Type: "dir", Mode: 0o40755}, a.Path: a, b.Path: b},
		kids:  map[string][]model.BaseNode{"src": {a, b}},
	}, ov)
	h := &fakeHydrator{path: cachePath}
	engine := &Engine{Resolver: r, Repo: model.RepoPaths{ID: "repo"}, Overlay: ov, Hydrator: h}
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
func newPrefetchFixture(t *testing.T) (*ArtifactFuse, *Engine, *fakeHydrator) {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := model.BaseNode{Path: "src", Type: "dir", Mode: 0o40755}
	a := model.BaseNode{Path: "src/a.go", Type: "file", Mode: 0o100644, ObjectOID: "a", SizeState: "unknown"}
	b := model.BaseNode{Path: "src/b.go", Type: "file", Mode: 0o100644, ObjectOID: "b", SizeState: "unknown"}
	ov := newOverlay(t)
	r := newResolver(&fakeSnapshot{
		nodes: map[string]model.BaseNode{src.Path: src, a.Path: a, b.Path: b},
		kids:  map[string][]model.BaseNode{".": {src}, "src": {a, b}},
	}, ov)
	h := &fakeHydrator{path: cachePath}
	repo := model.RepoPaths{ID: "repo"}
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
			rec := &queuedAtOpenHydrator{fakeHydrator: h}
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
	*fakeHydrator
	queuedAtOpen []int
}

func (h *queuedAtOpenHydrator) OpenHydrated(ctx context.Context, repo model.RepoPaths, node model.BaseNode) (*os.File, int64, error) {
	h.queuedAtOpen = append(h.queuedAtOpen, len(h.tasks))
	return h.fakeHydrator.OpenHydrated(ctx, repo, node)
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
func waitForCachedBlob(t *testing.T, repo model.RepoPaths, oid string) {
	t.Helper()
	for {
		if _, err := os.Stat(blobcache.Path(repo.BlobCacheDir, oid)); err == nil {
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

// kernelDirent is one entry of a ReadDir or ReadDirPlus reply as the kernel
// decodes it. Only a ReadDirPlus entry carries the child's size, mode and
// times, the times in Unix nanoseconds.
type kernelDirent struct {
	name         string
	offset       fuseops.DirOffset
	size         uint64
	mode         uint32
	mtime, ctime int64
}

// decodeDirents decodes a ReadDir reply, or a ReadDirPlus reply when plus is
// set. Each entry is a fuse_dirent and its name padded to 8 bytes, after a
// fuse_entry_out for ReadDirPlus. macFUSE's fuse_attr carries crtime and
// flags, so its mode lies further in and the struct is longer.
func decodeDirents(buf []byte, plus bool) ([]kernelDirent, error) {
	const entryOutHeader = 40 // nodeid, generation, and the entry and attribute timeouts
	const direntHeader = 24   // ino, off, namelen and type
	// fuse_attr holds atime, mtime and ctime seconds from byte 24, and their
	// nanoseconds after them, after crtime's seconds on macFUSE.
	attrSize, modeOffset, nsecOffset := 88, 60, 48
	if runtime.GOOS == "darwin" {
		attrSize, modeOffset, nsecOffset = 104, 72, 56
	}
	var out []kernelDirent
	for len(buf) > 0 {
		var d kernelDirent
		if plus {
			if len(buf) < entryOutHeader+attrSize {
				return out, fmt.Errorf("entry %d: %d bytes left, short of a fuse_entry_out", len(out), len(buf))
			}
			attr := buf[entryOutHeader:]
			d.size = binary.NativeEndian.Uint64(attr[8:])
			d.mode = binary.NativeEndian.Uint32(attr[modeOffset:])
			unixNano := func(sec, nsec int) int64 {
				return time.Unix(int64(binary.NativeEndian.Uint64(attr[sec:])), int64(binary.NativeEndian.Uint32(attr[nsec:]))).UnixNano()
			}
			d.mtime, d.ctime = unixNano(32, nsecOffset+4), unixNano(40, nsecOffset+8)
			buf = buf[entryOutHeader+attrSize:]
		}
		if len(buf) < direntHeader {
			return out, fmt.Errorf("entry %d: %d bytes left, short of a fuse_dirent", len(out), len(buf))
		}
		d.offset = fuseops.DirOffset(binary.NativeEndian.Uint64(buf[8:]))
		namelen := int(binary.NativeEndian.Uint32(buf[16:]))
		size := (direntHeader + namelen + 7) &^ 7
		if len(buf) < size {
			return out, fmt.Errorf("entry %d: %d bytes left, short of its %d-byte name", len(out), len(buf), namelen)
		}
		d.name = string(buf[direntHeader : direntHeader+namelen])
		buf = buf[size:]
		out = append(out, d)
	}
	return out, nil
}

// readDirPlus lists dir through ReadDirPlus from offset 0 and decodes the reply.
func readDirPlus(t *testing.T, fs *ArtifactFuse, dir fuseops.InodeID) map[string]kernelDirent {
	t.Helper()
	open := &fuseops.OpenDirOp{Inode: dir}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	op := &fuseops.ReadDirPlusOp{ReadDirOp: fuseops.ReadDirOp{Handle: open.Handle, Dst: make([]byte, 4096)}}
	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	entries, err := decodeDirents(op.Dst[:op.BytesRead], true)
	if err != nil {
		t.Fatalf("ReadDirPlus reply: %v", err)
	}
	out := map[string]kernelDirent{}
	for _, d := range entries {
		out[d.name] = d
	}
	return out
}

// A listing longer than one reply arrives in pieces, each read starting at
// the offset of the last entry before it. Joined, the pieces hold each name
// once.
func TestReadDirPagesThroughAListingLongerThanItsBuffer(t *testing.T) {
	var files []model.BaseNode
	want := []string{".git"}
	for i := range 20 {
		files = append(files, baseFile(fmt.Sprintf("file-%02d.txt", i), fmt.Sprintf("blob-%02d", i), 0o100644))
		want = append(want, files[i].Path)
	}
	f := newCacheFixture(t, files...)
	for _, plus := range []bool{false, true} {
		open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
		if err := f.fs.OpenDir(f.ctx, open); err != nil {
			t.Fatal(err)
		}
		var got []string
		var offset fuseops.DirOffset
		// The bound stops reads that keep restarting at an entry already read.
		for range len(want) {
			op := &fuseops.ReadDirPlusOp{ReadDirOp: fuseops.ReadDirOp{Handle: open.Handle, Offset: offset, Dst: make([]byte, 400)}}
			read := func() error { return f.fs.ReadDir(f.ctx, &op.ReadDirOp) }
			if plus {
				read = func() error { return f.fs.ReadDirPlus(f.ctx, op) }
			}
			if err := read(); err != nil {
				t.Fatal(err)
			}
			page, err := decodeDirents(op.Dst[:op.BytesRead], plus)
			if err != nil {
				t.Fatalf("reply at offset %d: %v", offset, err)
			}
			if len(page) == 0 {
				break
			}
			for _, d := range page {
				got = append(got, d.name)
			}
			offset = page[len(page)-1].offset
		}
		if !slices.Equal(got, want) {
			t.Fatalf("listing read in pieces (ReadDirPlus %t) = %v, want %v", plus, got, want)
		}
	}
}

func TestReadDirPlusUsesReaddirMetadataWithoutHydration(t *testing.T) {
	repo := model.RepoPaths{ID: "repo", GitDir: "/tmp/repo.git"}
	file := model.BaseNode{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 42}
	script := model.BaseNode{Path: "run.sh", Type: "file", Mode: 0o100755, ObjectOID: "script", SizeState: "known", SizeBytes: 7}
	r := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{script.Path: script}, kids: map[string][]model.BaseNode{".": {file, script}}}, newOverlay(t))
	h := &fakeHydrator{size: 12}
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	entries := readDirPlus(t, fs, fuseops.RootInodeID)
	for name, want := range map[string]kernelDirent{
		"file.txt": {name: "file.txt", offset: 2, size: 42, mode: syscall.S_IFREG | 0o644},
		"run.sh":   {name: "run.sh", offset: 3, size: 7, mode: syscall.S_IFREG | 0o755},
	} {
		got := entries[name]
		got.mtime, got.ctime = 0, 0
		if got != want {
			t.Fatalf("ReadDirPlus entry %s = %+v, want %+v", name, got, want)
		}
	}
	if h.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", h.calls)
	}
	if fs.pathToInode["file.txt"] == 0 {
		t.Fatal("ReadDirPlus did not allocate file inode")
	}
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "run.sh"}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	if got := lookup.Entry.Attributes.Mode; got != 0o755 {
		t.Fatalf("lookup mode of run.sh = %v, want -rwxr-xr-x", got)
	}
}

// ReadDirPlus reports the size the snapshot knows, and 0 for one it does not
// know, rather than hydrate the blob. MountRepoWithGate leaves READDIRPLUS
// off, because the kernel would cache that 0.
func TestReadDirPlusDoesNotHydrateUnknownSizeBaseFile(t *testing.T) {
	repo := model.RepoPaths{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"}},
		}},
		newOverlay(t),
	)
	h := &fakeHydrator{size: 12}
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	if got, ok := readDirPlus(t, fs, fuseops.RootInodeID)["file.txt"]; !ok || got.size != 0 {
		t.Fatalf("ReadDirPlus entry file.txt = %+v, %t; want size 0", got, ok)
	}
	if h.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", h.calls)
	}
}

func TestReadDirPlusContinuesInItsListingGeneration(t *testing.T) {
	repo := model.RepoPaths{ID: "repo", GitDir: "/tmp/repo.git"}
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
	r := &Resolver{Snapshot: snap, Overlay: newOverlay(t)}
	r.SetGeneration(1)
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: &fakeHydrator{}})
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
	repo := model.RepoPaths{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 42}},
		}},
		newOverlay(t),
	)
	fs := NewArtifactFuse(repo, &Engine{Resolver: r, Repo: repo, Hydrator: &fakeHydrator{}})
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
	if fs.inodes[fs.pathToInode[".git"]] != nil {
		t.Fatal("inode lookup leaked for entry that did not fit")
	}
}

func TestReleasingAReaderLeavesAnotherHandlesWritesCoalescing(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := model.RepoPaths{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	ov, err := overlaystore.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{}}, Overlay: ov}
	resolver.SetGeneration(1)
	fs := NewArtifactFuse(cfg, &Engine{Repo: cfg, Resolver: resolver, Overlay: ov})
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "f.txt", Mode: 0o644}
	if err := fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	reader := &fuseops.OpenFileOp{Inode: create.Entry.Child}
	if err := fs.OpenFile(ctx, reader); err != nil {
		t.Fatal(err)
	}
	if err := fs.FlushFile(ctx, &fuseops.FlushFileOp{Inode: create.Entry.Child, Handle: reader.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: reader.Handle}); err != nil {
		t.Fatal(err)
	}
	if size, pending := committedOverlay(t, cfg.OverlayDBPath, "f.txt"); size != 0 || pending != 1 {
		t.Fatalf("after the reader's release: committed size %d, pending rows %d; want 0 and 1", size, pending)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if size, pending := committedOverlay(t, cfg.OverlayDBPath, "f.txt"); size != 3 || pending != 0 {
		t.Fatalf("after the writer's release: committed size %d, pending rows %d; want 3 and 0", size, pending)
	}
}

// A create and close with no write, as touch does, leaves no pending row.
func TestCreateThenCloseClearsPending(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := model.RepoPaths{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	ov, err := overlaystore.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{}}, Overlay: ov}
	resolver.SetGeneration(1)
	fs := NewArtifactFuse(cfg, &Engine{Repo: cfg, Resolver: resolver, Overlay: ov})
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "f.txt", Mode: 0o644}
	if err := fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := fs.FlushFile(ctx, &fuseops.FlushFileOp{Inode: create.Entry.Child, Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if size, pending := committedOverlay(t, cfg.OverlayDBPath, "f.txt"); size != 0 || pending != 0 {
		t.Fatalf("after create and close: committed size %d, pending rows %d; want 0 and 0", size, pending)
	}
}

// newChmoddedFile creates f.txt holding abc, closes it, and chmods it to
// mode, through the file system an FSKit mount serves when fskit is set and
// a kernel mount serves otherwise.
func newChmoddedFile(t *testing.T, fskit bool, mode os.FileMode) (fuseutil.FileSystem, *overlaystore.Store, fuseops.InodeID) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := model.RepoPaths{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	ov, err := overlaystore.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{}}, Overlay: ov}
	resolver.SetGeneration(1)
	af := NewArtifactFuse(cfg, &Engine{Repo: cfg, Resolver: resolver, Overlay: ov})
	var fs fuseutil.FileSystem = af
	if fskit {
		fs = permissionCheckingFS{af}
	}
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "f.txt", Mode: 0o644}
	if err := fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: create.Entry.Child, Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	return fs, ov, create.Entry.Child
}

// On an FSKit mount, a new open for writing or with O_TRUNC of a file
// chmodded to 0444, and a truncate without a handle, fail with EACCES.
func TestReadOnlyFileRefusesNewWriter(t *testing.T) {
	ctx := context.Background()
	fs, ov, inode := newChmoddedFile(t, true, 0o444)
	for _, open := range []*fuseops.OpenFileOp{
		{Inode: inode, OpenFlags: syscall.O_WRONLY},
		{Inode: inode, OpenFlags: syscall.O_RDWR},
		{Inode: inode, OpenFlags: syscall.O_RDONLY | syscall.O_TRUNC},
	} {
		if got := kernelErrno(fs.OpenFile(ctx, open)); got != syscall.EACCES {
			t.Errorf("open with flags %v = %v, want EACCES", open.OpenFlags, got)
		}
	}
	zero := uint64(0)
	if got := kernelErrno(fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: inode, Size: &zero})); got != syscall.EACCES {
		t.Errorf("truncate without a handle = %v, want EACCES", got)
	}
	if err := fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_RDONLY}); err != nil {
		t.Errorf("read-only open of a 0444 file = %v", err)
	}
	e := overlayEntry(t, ov, "f.txt")
	if data, err := os.ReadFile(e.BackingPath); err != nil || string(data) != "abc" {
		t.Fatalf("content = %q, %v; want abc kept", data, err)
	}
}

// On an FSKit mount, a handle opened for writing before a chmod to 0444
// still writes and truncates through ftruncate(2).
func TestHandleOpenedForWriteBeforeChmodKeepsWriting(t *testing.T) {
	ctx := context.Background()
	fs, ov, inode := newChmoddedFile(t, true, 0o644)
	open := &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_WRONLY}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatal(err)
	}
	ro := os.FileMode(0o444)
	if err := fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: inode, Mode: &ro}); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: inode, Handle: open.Handle, Offset: 3, Data: []byte("def")}); err != nil {
		t.Fatalf("write through the earlier handle = %v", err)
	}
	two := uint64(2)
	if err := fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: inode, Handle: &open.Handle, Size: &two}); err != nil {
		t.Fatalf("ftruncate through the earlier handle = %v", err)
	}
	e := overlayEntry(t, ov, "f.txt")
	if data, err := os.ReadFile(e.BackingPath); err != nil || string(data) != "ab" {
		t.Fatalf("content = %q, %v; want ab", data, err)
	}
}

// On an FSKit mount, the handle that created a 0444 file for writing still
// writes and truncates it through ftruncate(2).
func TestCreateHandleTruncatesReadOnlyFile(t *testing.T) {
	ctx := context.Background()
	fs, ov, _ := newChmoddedFile(t, true, 0o644)
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "ro.txt", Mode: 0o444, OpenFlags: syscall.O_WRONLY}
	if err := fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	if err := fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: create.Entry.Child, Handle: &create.Handle, Size: &one}); err != nil {
		t.Fatalf("ftruncate through the create handle = %v", err)
	}
	e := overlayEntry(t, ov, "ro.txt")
	if data, err := os.ReadFile(e.BackingPath); err != nil || string(data) != "a" {
		t.Fatalf("content = %q, %v; want a", data, err)
	}
}

// createReleasedReadOnlyFile creates ro.txt with mode 0444 through fs, then
// releases the create handle, as an FSKit mount's create sequence does.
func createReleasedReadOnlyFile(t *testing.T, fs fuseutil.FileSystem) fuseops.InodeID {
	t.Helper()
	ctx := context.Background()
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "ro.txt", Mode: 0o444, OpenFlags: syscall.O_RDWR | syscall.O_CREAT | syscall.O_EXCL}
	if err := fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: create.Entry.Child, Handle: &create.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	return create.Entry.Child
}

// On an FSKit mount, the creator's descriptor can arrive as a write open after
// the create releases its handle. That open succeeds whatever the create's
// mode, and the next write open of the file is checked.
func TestCreatorsOpenAfterCreateWritesReadOnlyFile(t *testing.T) {
	ctx := context.Background()
	fs, ov, _ := newChmoddedFile(t, true, 0o644)
	inode := createReleasedReadOnlyFile(t, fs)
	open := &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_WRONLY}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatalf("the creator's write open = %v", err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: inode, Handle: open.Handle, Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
		t.Fatal(err)
	}
	e := overlayEntry(t, ov, "ro.txt")
	if data, err := os.ReadFile(e.BackingPath); err != nil || string(data) != "abc" {
		t.Fatalf("content = %q, %v; want abc", data, err)
	}
	if got := kernelErrno(fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_WRONLY})); got != syscall.EACCES {
		t.Errorf("a later write open = %v, want EACCES", got)
	}
}

// On an FSKit mount, only the first open after a create skips the check,
// whatever that open's flags.
func TestFirstOpenAfterCreateUsesTheCreatorsAllowance(t *testing.T) {
	ctx := context.Background()
	fs, _, _ := newChmoddedFile(t, true, 0o644)
	inode := createReleasedReadOnlyFile(t, fs)
	if err := fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_RDONLY}); err != nil {
		t.Fatalf("read open = %v", err)
	}
	if got := kernelErrno(fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_WRONLY})); got != syscall.EACCES {
		t.Errorf("write open after a read open = %v, want EACCES", got)
	}
}

// On an FSKit mount, a write through the create handle ends the creator's
// allowance, so the next write open is checked.
func TestWriteAfterCreateEndsTheCreatorsAllowance(t *testing.T) {
	ctx := context.Background()
	fs, _, _ := newChmoddedFile(t, true, 0o644)
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "ro.txt", Mode: 0o444, OpenFlags: syscall.O_RDWR | syscall.O_CREAT | syscall.O_EXCL}
	if err := fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if got := kernelErrno(fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: create.Entry.Child, OpenFlags: syscall.O_WRONLY})); got != syscall.EACCES {
		t.Errorf("write open after a write = %v, want EACCES", got)
	}
}

// On an FSKit mount, ftruncate(2) through a read-only handle of a 0444 file
// fails with EACCES.
func TestReadOnlyHandleCannotTruncateReadOnlyFile(t *testing.T) {
	ctx := context.Background()
	fs, ov, inode := newChmoddedFile(t, true, 0o444)
	open := &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_RDONLY}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	if got := kernelErrno(fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: inode, Handle: &open.Handle, Size: &zero})); got != syscall.EACCES {
		t.Errorf("ftruncate through a read-only handle = %v, want EACCES", got)
	}
	e := overlayEntry(t, ov, "f.txt")
	if data, err := os.ReadFile(e.BackingPath); err != nil || string(data) != "abc" {
		t.Fatalf("content = %q, %v; want abc kept", data, err)
	}
}

// Only a size change an FSKit mount serves looks the entry's mode up for the
// permission check.
func TestKernelMountTruncateSkipsThePermissionLookup(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := model.RepoPaths{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	store, err := overlaystore.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ov := &lookupCountingOverlay{OverlayStore: store}
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{}}, Overlay: ov}
	resolver.SetGeneration(1)
	kernel := NewArtifactFuse(cfg, &Engine{Repo: cfg, Resolver: resolver, Overlay: ov})
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "f.txt", Mode: 0o644}
	if err := kernel.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	lookupsFor := func(fs fuseutil.FileSystem) int {
		t.Helper()
		ov.lookups = 0
		one := uint64(1)
		if err := fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: create.Entry.Child, Size: &one}); err != nil {
			t.Fatal(err)
		}
		return ov.lookups
	}
	if kernelLookups, fskitLookups := lookupsFor(kernel), lookupsFor(permissionCheckingFS{kernel}); kernelLookups >= fskitLookups {
		t.Fatalf("truncate looked the entry up %d times on a kernel mount and %d on an FSKit mount, want fewer on the kernel mount", kernelLookups, fskitLookups)
	}
}

// On an FSKit mount, a new open of a file chmodded to 0000 fails with EACCES.
func TestNoAccessFileRefusesReader(t *testing.T) {
	ctx := context.Background()
	fs, _, inode := newChmoddedFile(t, true, 0)
	for _, open := range []*fuseops.OpenFileOp{
		{Inode: inode, OpenFlags: syscall.O_RDONLY},
		{Inode: inode, OpenFlags: syscall.O_WRONLY},
		{Inode: inode, OpenFlags: syscall.O_RDWR},
	} {
		if got := kernelErrno(fs.OpenFile(ctx, open)); got != syscall.EACCES {
			t.Errorf("open with flags %v = %v, want EACCES", open.OpenFlags, got)
		}
	}
}

// On a kernel mount, the adapter refuses no open and no truncate for the
// entry's permission bits.
func TestKernelMountAdapterSkipsPermissionCheck(t *testing.T) {
	ctx := context.Background()
	fs, ov, inode := newChmoddedFile(t, false, 0)
	for _, open := range []*fuseops.OpenFileOp{
		{Inode: inode, OpenFlags: syscall.O_RDONLY},
		{Inode: inode, OpenFlags: syscall.O_WRONLY},
		{Inode: inode, OpenFlags: syscall.O_RDWR},
	} {
		if err := fs.OpenFile(ctx, open); err != nil {
			t.Errorf("open with flags %v = %v, want success", open.OpenFlags, err)
		}
	}
	zero := uint64(0)
	if err := fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: inode, Size: &zero}); err != nil {
		t.Errorf("truncate without a handle = %v, want success", err)
	}
	if e := overlayEntry(t, ov, "f.txt"); e.SizeBytes != 0 {
		t.Fatalf("size after the truncate = %d, want 0", e.SizeBytes)
	}
}

// overlayEntry returns path's overlay entry, and fails the test without one.
func overlayEntry(t *testing.T, ov *overlaystore.Store, path string) model.OverlayEntry {
	t.Helper()
	e, ok, err := ov.Lookup(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("overlay entry %s = %v, %t", path, err, ok)
	}
	return e
}

// committedOverlay reads path's committed size and the pending row count
// straight from the database, past the store's in-memory values.
func committedOverlay(t *testing.T, dbPath, path string) (size int64, pending int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT size_bytes, (SELECT COUNT(*) FROM overlay_pending) FROM overlay_entries WHERE path=?`, path).Scan(&size, &pending); err != nil {
		t.Fatal(err)
	}
	return size, pending
}

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

// The kernel can send CREATE for a name that appeared after its lookup
// missed, so CreateFile must not replace an entry that exists.
func TestCreateFileKeepsExistingEntry(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "src/nested/base.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 12}
	f.resolver.Snapshot = &fakeSnapshot{nodes: map[string]model.BaseNode{base.Path: base}, kids: map[string][]model.BaseNode{}}
	for _, name := range []string{"value.txt", "base.txt"} {
		op := &fuseops.CreateFileOp{Parent: f.nestInode, Name: name, Mode: 0o644}
		if got := kernelErrno(f.fs.CreateFile(ctx, op)); got != syscall.EEXIST {
			t.Fatalf("CreateFile(%q) over an existing entry = %v, want EEXIST", name, got)
		}
	}
	if entry, ok, err := f.store.Lookup(ctx, "src/nested/value.txt"); err != nil || !ok || entry.SizeBytes != int64(len("overlay content")) {
		t.Fatalf("overlay file after CreateFile = %+v, %v, %v; want its content kept", entry, ok, err)
	}
	if entry, ok, err := f.store.Lookup(ctx, base.Path); err != nil || ok {
		t.Fatalf("CreateFile over a base file left overlay entry %+v, %v; want the base file kept", entry, err)
	}
	attrs := &fuseops.LookUpInodeOp{Parent: f.nestInode, Name: "base.txt"}
	if err := f.fs.LookUpInode(ctx, attrs); err != nil || attrs.Entry.Attributes.Size != uint64(base.SizeBytes) {
		t.Fatalf("base file after CreateFile = size %d, %v; want size %d", attrs.Entry.Attributes.Size, err, base.SizeBytes)
	}
}

// The kernel can send MKDIR or SYMLINK for a name its lookup missed before a
// HEAD change added it. The op must fail with EEXIST and keep the base file.
func TestMkDirAndCreateSymlinkKeepEntryASwitchAdded(t *testing.T) {
	for _, op := range []string{"MkDir", "CreateSymlink"} {
		t.Run(op, func(t *testing.T) {
			f := newCacheFixture(t)
			if got := f.lookup(fuseops.RootInodeID, "x"); got != 0 {
				t.Fatalf("lookup of x before the switch = inode %d, want a miss", got)
			}
			f.switchTo(baseFile("x", "blob-X", 0o100644))
			var err error
			if op == "MkDir" {
				err = f.fs.MkDir(f.ctx, &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "x", Mode: os.ModeDir | 0o755})
			} else {
				err = f.fs.CreateSymlink(f.ctx, &fuseops.CreateSymlinkOp{Parent: fuseops.RootInodeID, Name: "x", Target: "elsewhere"})
			}
			id, exists, idErr := f.fs.engine.Identity("x")
			if idErr != nil {
				t.Fatal(idErr)
			}
			if kernelErrno(err) != syscall.EEXIST || !exists || id.Type != "file" || id.Blob != "blob-X" {
				t.Fatalf("%s over base file x = %v, and x is now %+v (exists %t); want EEXIST and the base file kept", op, err, id, exists)
			}
		})
	}
}

// A listing shows a directory that only overlay entries below it imply: one a
// HEAD change removed while it held an untracked file, or one whose file a
// write through a held inode recreated. A lookup of it finds a directory.
func TestLookUpFindsImplicitDirectory(t *testing.T) {
	for _, how := range []string{"untracked file", "write through held inode"} {
		t.Run(how, func(t *testing.T) {
			f := newCacheFixture(t, baseDir("d", "d1"), baseFile("d/file.txt", "blob-A", 0o100644))
			d := f.lookup(fuseops.RootInodeID, "d")
			held := f.lookup(d, "file.txt")
			open := f.open(held)
			if how == "untracked file" {
				create := &fuseops.CreateFileOp{Parent: d, Name: "new.txt", Mode: 0o644}
				if err := f.fs.CreateFile(f.ctx, create); err != nil {
					t.Fatal(err)
				}
				if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
					t.Fatal(err)
				}
			}
			f.switchTo()
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if how == "write through held inode" {
				if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("new")}); err != nil {
					t.Fatal(err)
				}
			}
			listed, _, _, err := f.fs.engine.ReadDir(f.ctx, ".")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(listed, func(e ReaddirEntry) bool { return e.Name == "d" && e.Type == "dir" }) {
				t.Fatalf("root listing %+v lacks directory d", listed)
			}
			op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "d"}
			if err := f.fs.LookUpInode(f.ctx, op); err != nil || op.Entry.Child == 0 || !op.Entry.Attributes.Mode.IsDir() {
				t.Fatalf("lookup of d, which the root listing shows, = inode %d mode %v, %v; want a directory", op.Entry.Child, op.Entry.Attributes.Mode, err)
			}
		})
	}
}

// enqueueCounter passes every call to its hydrator and counts the blobs queued
// through it.
type enqueueCounter struct {
	model.Hydrator
	queued atomic.Int64
}

func (c *enqueueCounter) EnqueueBatch(tasks []model.HydrationTask) {
	c.queued.Add(int64(len(tasks)))
	c.Hydrator.EnqueueBatch(tasks)
}
