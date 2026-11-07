//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
)

// recordingNotifier records the invalidations a mount sends the kernel.
type recordingNotifier struct {
	mu      sync.Mutex
	inodes  []fuseops.InodeID
	entries []kernelEntry
	sent    chan struct{}
}

func newRecordingNotifier() *recordingNotifier {
	return &recordingNotifier{sent: make(chan struct{}, 64)}
}

func (n *recordingNotifier) InvalidateInode(inode fuseops.InodeID, offset, length int64) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if offset != 0 || length != 0 {
		return errors.New("invalidation of part of an inode")
	}
	n.inodes = append(n.inodes, inode)
	n.sent <- struct{}{}
	return nil
}

func (n *recordingNotifier) InvalidateEntry(parent fuseops.InodeID, name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.entries = append(n.entries, kernelEntry{parent, name})
	n.sent <- struct{}{}
	return nil
}

func (n *recordingNotifier) recorded() ([]fuseops.InodeID, []kernelEntry) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.inodes), slices.Clone(n.entries)
}

// cacheFixture serves a repo whose base snapshot the test publishes, through
// MountRepoWithGate on the kernel transport with fuse.Mount replaced.
type cacheFixture struct {
	t        *testing.T
	ctx      context.Context
	snap     *snapshot.Store
	resolver *Resolver
	fs       *ArtifactFuse
	mount    MountedFS
	notifier *recordingNotifier
	blobs    *fakeBatchHydrator
}

func baseFile(path, blob string, mode uint32) model.BaseNode {
	return model.BaseNode{Path: path, Type: "file", Mode: mode, ObjectOID: blob, SizeState: "known", SizeBytes: int64(len(blob))}
}

func baseDir(path, tree string) model.BaseNode {
	return model.BaseNode{Path: path, Type: "dir", Mode: 0o40000, ObjectOID: tree, SizeState: "known"}
}

func newCacheFixture(t *testing.T, nodes ...model.BaseNode) *cacheFixture {
	t.Helper()
	return newCacheFixtureOn(t, BackendKernel, nodes...)
}

// newCacheFixtureOn mounts through backend, which only darwin acts on.
func newCacheFixtureOn(t *testing.T, backend string, nodes ...model.BaseNode) *cacheFixture {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	repo := model.RepoPaths{ID: "repo", Name: "repo", MountPath: t.TempDir(), OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	snap, err := snapshot.New(ctx, filepath.Join(tmp, "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	ov, err := overlaystore.New(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	f := &cacheFixture{t: t, ctx: ctx, snap: snap, resolver: &Resolver{Snapshot: snap, Overlay: ov}, blobs: &fakeBatchHydrator{pathsByOID: map[string]string{}}}
	f.resolver.SetGeneration(f.publish(nodes...))
	engine := &Engine{Resolver: f.resolver, Repo: repo, Overlay: ov, Hydrator: f.blobs}

	t.Setenv(backendEnv, backend)
	saved := fuseMount
	fuseMount = func(context.Context, string, fuse.Server, *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		return &fuse.MountedFileSystem{}, nil
	}
	t.Cleanup(func() { fuseMount = saved })
	f.mount, err = MountRepoWithGate(context.Background(), context.Background(), repo, engine, nil, slog.New(slog.DiscardHandler), unredacted)
	if err != nil {
		t.Fatal(err)
	}
	f.fs = f.mount.(*mountedFSWrapper).fs
	f.notifier = newRecordingNotifier()
	f.fs.setNotifier(f.notifier)
	return f
}

// publish publishes nodes as a new generation, with a blob file for each
// file's blob that holds the blob's name.
func (f *cacheFixture) publish(nodes ...model.BaseNode) int64 {
	f.t.Helper()
	for _, n := range nodes {
		if n.Type != "file" {
			continue
		}
		path := filepath.Join(f.t.TempDir(), n.ObjectOID)
		if err := os.WriteFile(path, []byte(n.ObjectOID), 0o644); err != nil {
			f.t.Fatal(err)
		}
		f.blobs.pathsByOID[n.ObjectOID] = path
	}
	gen, err := f.snap.PublishGeneration(f.ctx, "head", "main", append([]model.BaseNode{baseDir(".", "root")}, nodes...))
	if err != nil {
		f.t.Fatal(err)
	}
	return gen
}

// switchTo publishes nodes and switches the mount's resolver to them.
func (f *cacheFixture) switchTo(nodes ...model.BaseNode) {
	f.t.Helper()
	if err := f.resolver.SwitchGeneration(f.ctx, f.publish(nodes...), 0); err != nil {
		f.t.Fatal(err)
	}
}

// lookupMissed reports whether a lookup answered that the name is missing:
// with a negative entry where the platform caches them, and ENOENT elsewhere.
func lookupMissed(err error, op *fuseops.LookUpInodeOp) bool {
	if negativeLookupEntries {
		return err == nil && op.Entry.Child == 0
	}
	return kernelErrno(err) == syscall.ENOENT
}

// lookup looks up name in parent and returns its inode, or 0 for a missing name.
func (f *cacheFixture) lookup(parent fuseops.InodeID, name string) fuseops.InodeID {
	f.t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	if err := f.fs.LookUpInode(f.ctx, op); err != nil && !lookupMissed(err, op) {
		f.t.Fatalf("LookUpInode(%q): %v", name, err)
	}
	return op.Entry.Child
}

func (f *cacheFixture) open(inode fuseops.InodeID) *fuseops.OpenFileOp {
	f.t.Helper()
	op := &fuseops.OpenFileOp{Inode: inode}
	if err := f.fs.OpenFile(f.ctx, op); err != nil {
		f.t.Fatalf("OpenFile(%d): %v", inode, err)
	}
	f.t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
	})
	return op
}

func (f *cacheFixture) getattr(inode fuseops.InodeID) {
	f.t.Helper()
	if err := f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: inode}); err != nil {
		f.t.Fatalf("GetInodeAttributes(%d): %v", inode, err)
	}
}

func sortedInodes(ids []fuseops.InodeID) []fuseops.InodeID {
	return slices.Sorted(slices.Values(ids))
}

func entrySet(entries []kernelEntry) map[kernelEntry]int {
	set := map[kernelEntry]int{}
	for _, e := range entries {
		set[e]++
	}
	return set
}

// After a generation switch, the mount invalidates each inode the kernel
// holds whose type, permission bits or blob changed, and the entry of each
// name that vanished or changed type. On Linux it also invalidates each
// negative entry whose name now exists. A name made through the mount is
// already positive in the kernel, so it gets no invalidation, and neither do
// directories whose tree changed or overlay entries.
func TestInvalidateChangedDropsWhatTheSwitchChanged(t *testing.T) {
	f := newCacheFixture(t,
		baseFile("same.txt", "same", 0o100644),
		baseFile("edited.txt", "edited-1", 0o100644),
		baseFile("chmod.sh", "chmod", 0o100644),
		baseFile("gone.txt", "gone", 0o100644),
		baseFile("swap", "swap", 0o100644),
		baseDir("dir", "dir-1"),
		baseFile("dir/kept.txt", "kept", 0o100644),
		baseDir("olddir", "olddir"),
		baseFile("olddir/child.txt", "child", 0o100644),
		baseFile("source.txt", "source", 0o100644),
	)
	var root fuseops.InodeID = fuseops.RootInodeID
	same := f.lookup(root, "same.txt")
	edited := f.lookup(root, "edited.txt")
	chmod := f.lookup(root, "chmod.sh")
	f.lookup(root, "gone.txt")
	f.lookup(root, "swap")
	dir := f.lookup(root, "dir")
	f.lookup(dir, "kept.txt")
	olddir := f.lookup(root, "olddir")
	f.lookup(olddir, "child.txt")
	f.lookup(root, ".git")

	// Names the kernel caches as missing: one the switch adds, one that stays
	// missing, and four made through the mount.
	for _, name := range []string{"new.txt", "never.txt", "made-dir", "made-file", "made-link", "moved.txt"} {
		if child := f.lookup(root, name); child != 0 {
			t.Fatalf("lookup of missing %q = inode %d", name, child)
		}
	}
	if err := f.fs.MkDir(f.ctx, &fuseops.MkDirOp{Parent: root, Name: "made-dir", Mode: os.ModeDir | 0o755}); err != nil {
		t.Fatal(err)
	}
	create := &fuseops.CreateFileOp{Parent: root, Name: "made-file", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.CreateSymlink(f.ctx, &fuseops.CreateSymlinkOp{Parent: root, Name: "made-link", Target: "same.txt"}); err != nil {
		t.Fatal(err)
	}
	f.lookup(root, "source.txt")
	if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: root, OldName: "source.txt", NewParent: root, NewName: "moved.txt"}); err != nil {
		t.Fatal(err)
	}

	f.switchTo(
		baseFile("same.txt", "same", 0o100644),
		baseFile("edited.txt", "edited-2", 0o100644),
		baseFile("chmod.sh", "chmod", 0o100755),
		baseDir("swap", "swap"),
		baseFile("swap/inner.txt", "inner", 0o100644),
		baseDir("dir", "dir-2"),
		baseFile("dir/kept.txt", "kept", 0o100644),
		baseFile("dir/fresh.txt", "fresh", 0o100644),
		baseFile("new.txt", "new", 0o100644),
		baseFile("source.txt", "source", 0o100644),
	)
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}

	inodes, entries := f.notifier.recorded()
	if want := sortedInodes([]fuseops.InodeID{edited, chmod}); !slices.Equal(sortedInodes(inodes), want) {
		t.Errorf("invalidated inodes = %v, want %v (edited.txt, chmod.sh); same.txt is inode %d", sortedInodes(inodes), want, same)
	}
	want := map[kernelEntry]int{
		{root, "gone.txt"}:    1,
		{root, "swap"}:        1,
		{root, "olddir"}:      1,
		{olddir, "child.txt"}: 1,
	}
	if negativeLookupEntries {
		want[kernelEntry{root, "new.txt"}] = 1
	}
	if got := entrySet(entries); !maps.Equal(got, want) {
		t.Errorf("invalidated entries = %v, want %v", got, want)
	}
}

// A write through the mount makes the kernel's view of a file come from the
// overlay. A commit of that content makes reconcile drop the overlay entry, so
// the path is base again. A later switch that removes the path still
// invalidates the kernel's entry for it.
func TestInvalidateChangedDropsVanishedNameLastReportedFromOverlay(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-a", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	inode := f.lookup(root, "file.txt")
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: inode, Handle: f.open(inode).Handle, Data: []byte("xxxxxx")}); err != nil {
		t.Fatal(err)
	}
	f.switchTo(baseFile("file.txt", blobOID("xxxxxx"), 0o100644))
	if _, ok, err := f.resolver.Overlay.Lookup(f.ctx, "file.txt"); err != nil || ok {
		t.Fatalf("overlay holds file.txt after its content was committed: %t, %v", ok, err)
	}
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if _, entries := f.notifier.recorded(); len(entries) != 0 {
		t.Fatalf("invalidated entries after the commit = %v, want none", entries)
	}

	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if _, entries := f.notifier.recorded(); !slices.Equal(entries, []kernelEntry{{root, "file.txt"}}) {
		t.Fatalf("invalidated entries after file.txt vanished = %v, want file.txt", entries)
	}
}

// failingLookupOf fails the overlay lookup of each of paths.
type failingLookupOf struct {
	model.OverlayStore
	paths []string
}

func (o failingLookupOf) Lookup(ctx context.Context, path string) (model.OverlayEntry, bool, error) {
	if slices.Contains(o.paths, path) {
		return model.OverlayEntry{}, false, errors.New("injected lookup failure")
	}
	return o.OverlayStore.Lookup(ctx, path)
}

// A path whose identity cannot be resolved skips only its own invalidation.
// The pass still invalidates every other changed inode, and returns the error.
func TestInvalidateChangedContinuesPastAResolveError(t *testing.T) {
	f := newCacheFixture(t, baseFile("edited.txt", "edited-1", 0o100644), baseFile("broken.txt", "broken", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	edited := f.lookup(root, "edited.txt")
	f.lookup(root, "broken.txt")
	f.switchTo(baseFile("edited.txt", "edited-2", 0o100644), baseFile("broken.txt", "broken", 0o100644))
	f.resolver.Overlay = failingLookupOf{OverlayStore: f.resolver.Overlay, paths: []string{"broken.txt"}}

	err := f.mount.InvalidateChanged()
	if err == nil {
		t.Fatal("InvalidateChanged hid the resolve error")
	}
	if inodes, _ := f.notifier.recorded(); !slices.Equal(inodes, []fuseops.InodeID{edited}) {
		t.Fatalf("invalidated inodes = %v, want edited.txt (%d) despite the error on broken.txt", inodes, edited)
	}
}

// Two attribute replies race a switch: one computed before it is recorded
// after one computed after it. The switch's InvalidateInode makes the kernel
// discard the older reply, so reads keep serving the newer generation.
//
// The race falls between the moment an op computes its reply and the moment
// it records it, and an op holds no lock or hook there to pause it. So the
// test computes the older reply with Engine.Attrs and records it with
// setReported, as GetInodeAttributes does.
func TestLateReplyKeepsTheNewerGeneration(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "old-content", 0o100644))
	inode := f.lookup(fuseops.RootInodeID, "file.txt")
	older, err := f.fs.engine.Attrs(&Blobs{}, "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.switchTo(baseFile("file.txt", "new", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	getattr := &fuseops.GetInodeAttributesOp{Inode: inode}
	if err := f.fs.GetInodeAttributes(f.ctx, getattr); err != nil {
		t.Fatal(err)
	}
	f.fs.setReported(inode, older)

	read := &fuseops.ReadFileOp{Inode: inode, Handle: f.open(inode).Handle, Size: 64}
	if err := f.fs.ReadFile(f.ctx, read); err != nil {
		t.Fatal(err)
	}
	if got := string(bytes.Join(read.Data, nil)); got != "new" || uint64(len(got)) != getattr.Attributes.Size {
		t.Fatalf("read after the kernel took the %d-byte size of the newer reply = %q, want %q", getattr.Attributes.Size, got, "new")
	}
}

// A write fills the kernel's pages with the written bytes. A commit of them
// makes reconcile drop the overlay entry, and a later switch brings back the
// blob the file held before the write, at the same size. The kernel last saw
// the file from the overlay, so the pass must still drop those pages.
func TestInvalidateChangedDropsWrittenPagesWhenABaseBlobReturns(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-a", 0o100644))
	inode := f.lookup(fuseops.RootInodeID, "file.txt")
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: inode, Handle: f.open(inode).Handle, Data: []byte("xxxxxx")}); err != nil {
		t.Fatal(err)
	}
	f.switchTo(baseFile("file.txt", blobOID("xxxxxx"), 0o100644))
	if _, ok, err := f.resolver.Overlay.Lookup(f.ctx, "file.txt"); err != nil || ok {
		t.Fatalf("overlay holds file.txt after its content was committed: %t, %v", ok, err)
	}
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	before, _ := f.notifier.recorded()

	f.switchTo(baseFile("file.txt", "blob-a", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if after, _ := f.notifier.recorded(); !slices.Equal(after[len(before):], []fuseops.InodeID{inode}) {
		t.Fatalf("inodes the switch back to blob-a invalidated = %v, want file.txt (%d)", after[len(before):], inode)
	}
}
