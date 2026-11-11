//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
	blobs    *fakeHydrator
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
	f := &cacheFixture{t: t, ctx: ctx, snap: snap, resolver: &Resolver{Snapshot: snap, Overlay: ov}, blobs: &fakeHydrator{pathsByOID: map[string]string{}}}
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
// file's or symlink's blob that holds the blob's name.
func (f *cacheFixture) publish(nodes ...model.BaseNode) int64 {
	f.t.Helper()
	for _, n := range nodes {
		if n.Type == "dir" {
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
	return f.openOp(&fuseops.OpenFileOp{Inode: inode})
}

// openRW opens inode for reading and writing. A kernel sends a write or an
// ftruncate only through such a handle.
func (f *cacheFixture) openRW(inode fuseops.InodeID) *fuseops.OpenFileOp {
	f.t.Helper()
	return f.openOp(&fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_RDWR})
}

func (f *cacheFixture) openOp(op *fuseops.OpenFileOp) *fuseops.OpenFileOp {
	f.t.Helper()
	if err := f.fs.OpenFile(f.ctx, op); err != nil {
		f.t.Fatalf("OpenFile(%d): %v", op.Inode, err)
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
// holds whose type, permission bits or blob changed, the entry of each name
// that vanished, and both the entry and the inode of each name that changed
// type. On Linux it also invalidates each
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
	gone := f.lookup(root, "gone.txt")
	swap := f.lookup(root, "swap")
	dir := f.lookup(root, "dir")
	f.lookup(dir, "kept.txt")
	olddir := f.lookup(root, "olddir")
	child := f.lookup(olddir, "child.txt")
	f.lookup(root, ".git")

	// Names the kernel caches as missing: one the switch adds, one that stays
	// missing, and four made through the mount.
	for _, name := range []string{"new.txt", "never.txt", "made-dir", "made-file", "made-link", "moved.txt"} {
		op := &fuseops.LookUpInodeOp{Parent: root, Name: name}
		if err := f.fs.LookUpInode(f.ctx, op); !lookupMissed(err, op) {
			t.Fatalf("lookup of missing %q = inode %d, %v", name, op.Entry.Child, err)
		}
		if negativeLookupEntries && !op.Entry.EntryExpiration.After(time.Now()) {
			t.Fatalf("negative entry for %q expires %v, want later than now so the kernel caches it", name, op.Entry.EntryExpiration)
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
	if want := sortedInodes([]fuseops.InodeID{edited, chmod, swap}); !slices.Equal(sortedInodes(inodes), want) {
		t.Errorf("invalidated inodes = %v, want %v (edited.txt, chmod.sh, swap); same.txt is inode %d, gone.txt %d, olddir %d, olddir/child.txt %d", sortedInodes(inodes), want, same, gone, olddir, child)
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

// failingLookupOf fails the snapshot lookup of each of paths.
type failingLookupOf struct {
	model.SnapshotStore
	paths []string
}

func (s failingLookupOf) LookupNode(ctx context.Context, gen int64, path string) (model.BaseNode, bool, error) {
	if slices.Contains(s.paths, path) {
		return model.BaseNode{}, false, errors.New("injected lookup failure")
	}
	return s.SnapshotStore.LookupNode(ctx, gen, path)
}

// A path whose identity cannot be resolved skips only its own invalidation,
// and so does a negative entry whose name cannot be resolved. The pass still
// invalidates every other changed inode, and returns the first error, which
// here is the held inode's. broken.txt changes in the switch, so the pass
// resolves it: a path the switch leaves unchanged needs no resolve.
func TestInvalidateChangedContinuesPastAResolveError(t *testing.T) {
	f := newCacheFixture(t, baseFile("edited.txt", "edited-1", 0o100644), baseFile("broken.txt", "broken-1", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	edited := f.lookup(root, "edited.txt")
	f.lookup(root, "broken.txt")
	if child := f.lookup(root, "missing.txt"); child != 0 {
		t.Fatalf("lookup of missing.txt = inode %d", child)
	}
	f.switchTo(baseFile("edited.txt", "edited-2", 0o100644), baseFile("broken.txt", "broken-2", 0o100644))
	f.resolver.Snapshot = failingLookupOf{SnapshotStore: f.snap, paths: []string{"broken.txt", "missing.txt"}}

	err := f.mount.InvalidateChanged()
	if err == nil || !strings.Contains(err.Error(), "broken.txt") {
		t.Fatalf("InvalidateChanged = %v, want the first error, on broken.txt", err)
	}
	if inodes, _ := f.notifier.recorded(); !slices.Equal(inodes, []fuseops.InodeID{edited}) {
		t.Fatalf("invalidated inodes = %v, want edited.txt (%d) despite the errors", inodes, edited)
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

// A lookup or getattr between a switch and its pass reports the new blob, so
// the identity the kernel received already matches the merged view when the
// pass compares them. The kernel's pages can still hold the old blob: the
// macFUSE kernel extension ignores a lookup's attributes for an inode it
// holds, and drops pages on a getattr only when the size changed. The pass
// must still invalidate the inode, and a later switch that keeps the file
// must not invalidate it again.
func TestSwitchPassInvalidatesInodeAReplyUpdatedFirst(t *testing.T) {
	for _, reply := range []string{"none", "lookup", "getattr"} {
		t.Run(reply, func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			f.open(held)
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			switch reply {
			case "lookup":
				f.lookup(fuseops.RootInodeID, "file.txt")
			case "getattr":
				f.getattr(held)
			}
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if inodes, _ := f.notifier.recorded(); !slices.Equal(inodes, []fuseops.InodeID{held}) {
				t.Fatalf("pass invalidated inodes %v, want held inode %d, whose pages hold blob-A", inodes, held)
			}
			f.getattr(held)
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if inodes, _ := f.notifier.recorded(); len(inodes) != 1 {
				t.Fatalf("a switch that kept file.txt invalidated it again: inodes %v", inodes)
			}
		})
	}
}

// A switch that turns a held file into a directory invalidates its entry and
// its inode once, also when a getattr before the pass retired the inode. A
// lookup before the pass already gave the kernel the directory under a new
// inode, so the pass sends nothing. A later switch that keeps the directory
// sends nothing either.
func TestSwitchPassInvalidatesRetypedEntryOnce(t *testing.T) {
	for _, first := range []string{"pass", "lookup", "getattr"} {
		t.Run(first, func(t *testing.T) {
			f := newCacheFixture(t, baseFile("swap", "blob-S", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "swap")
			dir := []model.BaseNode{baseDir("swap", "t1"), baseFile("swap/x.txt", "x", 0o100644)}
			f.switchTo(dir...)
			wantInodes, wantEntries := []fuseops.InodeID{held}, []kernelEntry{{fuseops.RootInodeID, "swap"}}
			switch first {
			case "lookup":
				f.lookup(fuseops.RootInodeID, "swap")
				wantInodes, wantEntries = nil, nil
			case "getattr":
				f.getattr(held)
			}
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if inodes, entries := f.notifier.recorded(); !slices.Equal(inodes, wantInodes) || !slices.Equal(entries, wantEntries) {
				t.Fatalf("the switch that made swap a directory invalidated inodes %v and entries %v, want inodes %v and entries %v", inodes, entries, wantInodes, wantEntries)
			}
			f.lookup(fuseops.RootInodeID, "swap")
			f.switchTo(dir...)
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if inodes, entries := f.notifier.recorded(); len(inodes) != len(wantInodes) || len(entries) != len(wantEntries) {
				t.Fatalf("a switch that kept the directory swap sent invalidations: inodes %v, entries %v", inodes, entries)
			}
		})
	}
}

// An entry invalidation leaves the kernel's attributes and pages of the
// inode. A switch that retypes a held file therefore invalidates its inode
// too, and retires it. A switch that removes the held file invalidates only
// its entry, and a lookup, getattr or pass that finds the path back changed
// retires the file's inode. Either way the inode never takes the blob of a
// later switch that brings the path back as a file.
func TestSwitchPassInvalidatesInodeWithItsEntry(t *testing.T) {
	for _, away := range []struct {
		name  string
		nodes []model.BaseNode
	}{
		{"retype", []model.BaseNode{baseDir("swap", "t1"), baseFile("swap/x.txt", "x", 0o100644)}},
		{"vanish", nil},
	} {
		for _, reply := range []string{"none", "lookup", "getattr"} {
			t.Run(away.name+"/"+reply, func(t *testing.T) {
				f := newCacheFixture(t, baseFile("swap", "blob-S", 0o100644))
				held := f.lookup(fuseops.RootInodeID, "swap")
				f.open(held)
				f.switchTo(away.nodes...)
				if err := f.mount.InvalidateChanged(); err != nil {
					t.Fatal(err)
				}
				f.switchTo(baseFile("swap", "blob-T", 0o100644))
				switch reply {
				case "lookup":
					f.lookup(fuseops.RootInodeID, "swap")
				case "getattr":
					f.getattr(held)
				}
				if err := f.mount.InvalidateChanged(); err != nil {
					t.Fatal(err)
				}
				if away.nodes == nil {
					if got := openErrno(f, held); got != syscall.ESTALE {
						t.Fatalf("open of held inode %d, whose path came back as blob-T, = errno %v, want ESTALE", held, got)
					}
					return
				}
				if inodes, _ := f.notifier.recorded(); !slices.Contains(inodes, held) {
					t.Fatalf("passes invalidated inodes %v, want held inode %d, whose pages hold blob-S", inodes, held)
				}
			})
		}
	}
}

// The switch pass sends a vanished regular file only its entry invalidation,
// because the file's content did not change: its open file keeps reading what
// it read. When the path comes back unchanged, the pass maps it back to the
// held inode and sends nothing.
func TestSwitchPassSendsOnlyTheEntryOfAVanishedFile(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.open(held)
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if inodes, entries := f.notifier.recorded(); len(inodes) != 0 || !slices.Equal(entries, []kernelEntry{{fuseops.RootInodeID, "file.txt"}}) {
		t.Fatalf("the switch that removed file.txt invalidated inodes %v and entries %v, want only the entry of file.txt", inodes, entries)
	}
	f.switchTo(baseFile("file.txt", "blob-A", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if inodes, _ := f.notifier.recorded(); len(inodes) != 0 {
		t.Fatalf("the switch that restored file.txt invalidated inodes %v", inodes)
	}
	read := &fuseops.ReadFileOp{Inode: held, Handle: open.Handle, Size: 64, Dst: make([]byte, 64)}
	if err := f.fs.ReadFile(f.ctx, read); err != nil {
		t.Fatalf("read through the open file after file.txt came back: %v", err)
	}
	if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != held {
		t.Fatalf("file.txt came back unchanged as inode %d, want %d", got, held)
	}
}

// A directory that a HEAD change leaves holding only untracked files is
// implied by them. Once they are removed through the mount, it no longer
// exists, though no later switch changes its path in the base. The next
// switch pass still invalidates its name.
func TestSwitchPassDropsImpliedDirectoryWhoseFilesWereRemoved(t *testing.T) {
	keep := baseFile("keep.txt", "keep", 0o100644)
	f := newCacheFixture(t, keep, baseDir("d", "d1"), baseFile("d/file.txt", "blob-A", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	d := f.lookup(root, "d")
	create := &fuseops.CreateFileOp{Parent: d, Name: "new.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	f.switchTo(keep)
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if got := f.lookup(root, "d"); got != d {
		t.Fatalf("d, implied by new.txt after the switch, looks up as inode %d, want %d", got, d)
	}
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: d, Name: "new.txt"}); err != nil {
		t.Fatal(err)
	}
	f.switchTo(keep, baseFile("other.txt", "other", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if _, entries := f.notifier.recorded(); !slices.Equal(entries, []kernelEntry{{root, "d"}}) {
		t.Fatalf("invalidated entries = %v, want d, which no file implies any more", entries)
	}
}

// An inode whose path changed keeps the generation it was last reported at.
// A prune can drop that generation, which leaves the pass no generation to
// diff the current one with. It then resolves the inode's path.
func TestSwitchPassResolvesInodeWhoseGenerationWasPruned(t *testing.T) {
	keep := baseFile("keep.txt", "keep", 0o100644)
	f := newCacheFixture(t, keep, baseFile("file.txt", "blob-A", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	file := f.lookup(root, "file.txt")
	f.switchTo(keep, baseFile("file.txt", "blob-B", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if inodes, _ := f.notifier.recorded(); !slices.Equal(inodes, []fuseops.InodeID{file}) {
		t.Fatalf("invalidated inodes = %v, want file.txt (%d)", inodes, file)
	}
	if err := f.snap.PruneGenerations(f.ctx, f.resolver.Generation()); err != nil {
		t.Fatal(err)
	}
	f.switchTo(keep)
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if _, entries := f.notifier.recorded(); !slices.Equal(entries, []kernelEntry{{root, "file.txt"}}) {
		t.Fatalf("invalidated entries = %v, want file.txt, which the last switch removed", entries)
	}
}

// The pass diffs the generations from the root down to each path it checks,
// through every directory above the path, whichever generation it last found
// those directories unchanged at. It walks into a directory whose tree ID
// changed, and into one without a tree ID. Here a/b/file.txt changes, which
// leaves its inode at the first generation while the pass moves the other
// paths to the second, and then it vanishes. The kernel holds enough of a/b
// for the first diff to list a and a/b.
func TestSwitchPassFindsChangesTwoDirectoriesDown(t *testing.T) {
	for _, trees := range []bool{true, false} {
		t.Run(fmt.Sprint("tree IDs=", trees), func(t *testing.T) {
			dirs := func(gen int) []model.BaseNode {
				var a, b string
				if trees {
					a, b = fmt.Sprint("a-", gen), fmt.Sprint("b-", gen)
				}
				return []model.BaseNode{baseFile("keep.txt", "keep", 0o100644), baseDir("a", a), baseDir("a/b", b), baseFile("a/b/x.txt", "x", 0o100644), baseFile("a/b/y.txt", "y", 0o100644)}
			}
			f := newCacheFixture(t, append(dirs(1), baseFile("a/b/file.txt", "blob-A", 0o100644))...)
			a := f.lookup(fuseops.RootInodeID, "a")
			b := f.lookup(a, "b")
			f.lookup(b, "x.txt")
			f.lookup(b, "y.txt")
			file := f.lookup(b, "file.txt")
			f.switchTo(append(dirs(2), baseFile("a/b/file.txt", "blob-B", 0o100644))...)
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if inodes, _ := f.notifier.recorded(); !slices.Equal(inodes, []fuseops.InodeID{file}) {
				t.Fatalf("invalidated inodes = %v, want a/b/file.txt (%d)", inodes, file)
			}
			f.switchTo(append(dirs(3), baseFile("a/b/other.txt", "other", 0o100644))...)
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if _, entries := f.notifier.recorded(); !slices.Equal(entries, []kernelEntry{{b, "file.txt"}}) {
				t.Fatalf("invalidated entries = %v, want a/b/file.txt, which the last switch removed", entries)
			}
		})
	}
}

// A prune can drop an inode's generation while the pass diffs it with the
// current one. Listings of the dropped generation then come back empty, and
// a file that vanished would look unchanged. The pass finds the generation
// gone after its diff, and resolves the inode's path instead.
func TestSwitchPassResolvesWhenAPruneOverlapsItsDiff(t *testing.T) {
	kept := []model.BaseNode{baseFile("dir/a.txt", "a", 0o100644), baseFile("dir/b.txt", "b", 0o100644)}
	f := newCacheFixture(t, append(kept, baseDir("dir", "dir-1"), baseFile("dir/gone.txt", "gone", 0o100644))...)
	dir := f.lookup(fuseops.RootInodeID, "dir")
	for _, name := range []string{"a.txt", "b.txt", "gone.txt"} {
		f.lookup(dir, name)
	}
	f.switchTo(append(kept, baseDir("dir", "dir-2"))...)
	snap := &listingHook{SnapshotStore: f.snap}
	f.resolver.Snapshot = snap
	// The diff lists the root at both generations, then dir at the first.
	prune := func() {
		if err := f.snap.PruneGenerations(f.ctx, f.resolver.Generation()); err != nil {
			t.Error(err)
		}
	}
	snap.onList = func() { snap.onList = func() { snap.onList = prune } }
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if snap.onList != nil {
		t.Fatal("the pass did not list dir at the first generation")
	}
	if _, entries := f.notifier.recorded(); !slices.Equal(entries, []kernelEntry{{dir, "gone.txt"}}) {
		t.Fatalf("invalidated entries = %v, want dir/gone.txt, which the switch removed", entries)
	}
}
