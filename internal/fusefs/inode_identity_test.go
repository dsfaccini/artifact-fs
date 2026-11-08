//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse/fuseops"
)

func forget(t *testing.T, fs *ArtifactFuse, ids ...fuseops.InodeID) {
	t.Helper()
	for _, id := range ids {
		if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
			t.Fatal(err)
		}
	}
}

func lookUp(t *testing.T, fs *ArtifactFuse, parent fuseops.InodeID, name string) fuseops.InodeID {
	t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatalf("LookUpInode(%q): %v", name, err)
	}
	return op.Entry.Child
}

// The kernel may evict a directory inode it holds no reference to and send
// FORGET for it, then look the same path up again. It does so when an expired
// dentry is released (fuse_dentry_delete) or after d_invalidate, which a
// revalidating LOOKUP of the pre-rename name triggers when RENAME wins the race.
// This drives that op sequence through the adapter.
func TestDirectoryInodeNumberSurvivesForgetAfterRename(t *testing.T) {
	ctx := context.Background()
	for _, forget := range []bool{false, true} {
		name := "no forget"
		if forget {
			name = "forget then lookup"
		}
		t.Run(name, func(t *testing.T) {
			f := newLocalOverlayFuseFixture(t)
			rename := &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src", NewParent: fuseops.RootInodeID, NewName: "dst"}
			if err := f.fs.Rename(ctx, rename); err != nil {
				t.Fatalf("Rename: %v", err)
			}
			// A revalidation that still carries the old name misses, so the
			// kernel invalidates the dentry that now names dst.
			stale := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "src"}
			if err := f.fs.LookUpInode(ctx, stale); !lookupMissed(err, stale) {
				t.Fatalf("LookUpInode(src) after rename = inode %d, %v; want a miss", stale.Entry.Child, err)
			}
			if forget {
				for _, id := range []fuseops.InodeID{f.fileInode, f.nestInode, f.srcInode} {
					if err := f.fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
						t.Fatal(err)
					}
				}
			}
			lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "dst"}
			if err := f.fs.LookUpInode(ctx, lookup); err != nil {
				t.Fatalf("LookUpInode(dst): %v", err)
			}
			if lookup.Entry.Child != f.srcInode {
				t.Fatalf("dst inode = %d, want %d: the directory got a new st_ino while it still exists", lookup.Entry.Child, f.srcInode)
			}
		})
	}
}

// A rename moves the numbers of the whole subtree, including those of paths
// the kernel has forgotten.
func TestRenameMovesForgottenInodeNumbers(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	forget(t, f.fs, f.fileInode, f.nestInode, f.srcInode)
	if err := f.fs.Rename(context.Background(), &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src", NewParent: fuseops.RootInodeID, NewName: "dst"}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	dst := lookUp(t, f.fs, fuseops.RootInodeID, "dst")
	nested := lookUp(t, f.fs, dst, "nested")
	file := lookUp(t, f.fs, nested, "value.txt")
	if got, want := []fuseops.InodeID{dst, nested, file}, []fuseops.InodeID{f.srcInode, f.nestInode, f.fileInode}; !slices.Equal(got, want) {
		t.Fatalf("inodes of dst, dst/nested, dst/nested/value.txt = %v, want the numbers they had under src: %v", got, want)
	}
}

// A file created where an unlinked file still open was gets a new number, so
// the kernel cannot take the open inode for the new file.
func TestCreateAfterUnlinkGetsNewInodeNumber(t *testing.T) {
	ctx := context.Background()
	f := newLocalOverlayFuseFixture(t)
	open := &fuseops.OpenFileOp{Inode: f.fileInode}
	if err := f.fs.OpenFile(ctx, open); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	})
	if err := f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: f.nestInode, Name: "value.txt"}); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	create := &fuseops.CreateFileOp{Parent: f.nestInode, Name: "value.txt", Mode: 0o644}
	if err := f.fs.CreateFile(ctx, create); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
	})
	if create.Entry.Child == f.fileInode {
		t.Fatalf("created file took inode %d of the unlinked file that is still open", f.fileInode)
	}
	forget(t, f.fs, f.fileInode)
	if got := lookUp(t, f.fs, f.nestInode, "value.txt"); got != create.Entry.Child {
		t.Fatalf("lookup after the unlinked inode's FORGET = inode %d, want the created file's %d", got, create.Entry.Child)
	}
}

// A directory made where a removed directory was gets a new number.
func TestMkDirAfterRmDirGetsNewInodeNumber(t *testing.T) {
	ctx := context.Background()
	f := newLocalOverlayFuseFixture(t)
	if err := f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: f.nestInode, Name: "value.txt"}); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if err := f.fs.RmDir(ctx, &fuseops.RmDirOp{Parent: f.srcInode, Name: "nested"}); err != nil {
		t.Fatalf("RmDir: %v", err)
	}
	mkdir := &fuseops.MkDirOp{Parent: f.srcInode, Name: "nested", Mode: os.ModeDir | 0o755}
	if err := f.fs.MkDir(ctx, mkdir); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if mkdir.Entry.Child == f.nestInode {
		t.Fatalf("made directory took inode %d of the removed one", f.nestInode)
	}
}

// A path deleted through the mount leaves no mapping behind once the kernel
// forgets its inode, so files that a build creates and deletes do not grow
// the table between HEAD changes.
func TestDeletedPathsLeaveNoMapping(t *testing.T) {
	ctx := context.Background()
	f := newLocalOverlayFuseFixture(t)
	before := len(f.fs.pathToInode)
	create := &fuseops.CreateFileOp{Parent: f.srcInode, Name: "tmp.txt", Mode: 0o644}
	if err := f.fs.CreateFile(ctx, create); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := f.fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: f.srcInode, Name: "tmp.txt"}); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	mkdir := &fuseops.MkDirOp{Parent: f.srcInode, Name: "tmp", Mode: os.ModeDir | 0o755}
	if err := f.fs.MkDir(ctx, mkdir); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := f.fs.RmDir(ctx, &fuseops.RmDirOp{Parent: f.srcInode, Name: "tmp"}); err != nil {
		t.Fatalf("RmDir: %v", err)
	}
	forget(t, f.fs, create.Entry.Child, mkdir.Entry.Child)
	if got := len(f.fs.pathToInode); got != before {
		t.Fatalf("table holds %d mappings after the created paths were deleted and forgotten, want %d: %v", got, before, f.fs.pathToInode)
	}
}

// A HEAD change retires the number of each path it removes, whether or not
// the kernel still holds its inode, and keeps the number of each path that
// still exists. The kernel holds nothing for a forgotten path, so the switch
// sends no invalidation for it.
func TestSwitchRetiresInodeNumbersOfVanishedPaths(t *testing.T) {
	f := newCacheFixture(t,
		baseFile("kept.txt", "kept-1", 0o100644),
		baseFile("forgotten.txt", "forgotten", 0o100644),
		baseFile("held.txt", "held", 0o100644),
	)
	var root fuseops.InodeID = fuseops.RootInodeID
	kept := f.lookup(root, "kept.txt")
	forgotten := f.lookup(root, "forgotten.txt")
	held := f.lookup(root, "held.txt")
	forget(t, f.fs, kept, forgotten)

	f.switchTo(baseFile("kept.txt", "kept-2", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if inodes, entries := f.notifier.recorded(); len(inodes) != 0 || !slices.Equal(entries, []kernelEntry{{root, "held.txt"}}) {
		t.Fatalf("invalidated inodes %v and entries %v, want only the entry of held.txt", inodes, entries)
	}
	forget(t, f.fs, held)
	if len(f.fs.unmapped) != 0 {
		t.Fatalf("unmapped holds %v after the kernel forgot held.txt, want nothing", f.fs.unmapped)
	}

	f.switchTo(baseFile("kept.txt", "kept-2", 0o100644), baseFile("forgotten.txt", "forgotten", 0o100644), baseFile("held.txt", "held", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if got := f.lookup(root, "kept.txt"); got != kept {
		t.Errorf("kept.txt = inode %d after the switches, want %d", got, kept)
	}
	if got := f.lookup(root, "forgotten.txt"); got == forgotten {
		t.Errorf("forgotten.txt came back with inode %d, which it had before a switch removed it", got)
	}
	if got := f.lookup(root, "held.txt"); got == held {
		t.Errorf("held.txt came back with inode %d, which it had before a switch removed it", got)
	}
}

// A create can land between a switch that removed its path and the pass that
// retires the path's number. It still gets a new number.
func TestCreateBeforeTheSwitchPassGetsNewInodeNumber(t *testing.T) {
	for _, tt := range []struct {
		name   string
		create func(*cacheFixture) (fuseops.InodeID, error)
	}{
		{"CreateFile", func(f *cacheFixture) (fuseops.InodeID, error) {
			op := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
			err := f.fs.CreateFile(f.ctx, op)
			if err == nil {
				f.t.Cleanup(func() {
					_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
				})
			}
			return op.Entry.Child, err
		}},
		{"MkDir", func(f *cacheFixture) (fuseops.InodeID, error) {
			op := &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: os.ModeDir | 0o755}
			err := f.fs.MkDir(f.ctx, op)
			return op.Entry.Child, err
		}},
		{"CreateSymlink", func(f *cacheFixture) (fuseops.InodeID, error) {
			op := &fuseops.CreateSymlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt", Target: "elsewhere"}
			err := f.fs.CreateSymlink(f.ctx, op)
			return op.Entry.Child, err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "old", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			f.switchTo()
			created, err := tt.create(f)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if created == held {
				t.Fatalf("%s took inode %d of the file the switch removed", tt.name, held)
			}
		})
	}
}

// An inode the kernel still holds after a switch removed its path is retired
// and invalidated when a later switch brings the path back with another blob,
// so the kernel drops what it cached and gets ESTALE when it asks again. Its
// name already left the kernel, so no switch sends an entry invalidation for
// it once the path is unmapped.
func TestSwitchInvalidatesHeldInodeWhosePathReturns(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	f.open(held)
	pass := func(nodes ...model.BaseNode) ([]fuseops.InodeID, []kernelEntry) {
		t.Helper()
		inodesBefore, entriesBefore := f.notifier.recorded()
		f.switchTo(nodes...)
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
		inodes, entries := f.notifier.recorded()
		return inodes[len(inodesBefore):], entries[len(entriesBefore):]
	}
	if _, entries := pass(); !slices.Equal(entries, []kernelEntry{{fuseops.RootInodeID, "file.txt"}}) {
		t.Fatalf("entries invalidated when file.txt vanished = %v, want file.txt", entries)
	}
	if inodes, entries := pass(); len(inodes) != 0 || len(entries) != 0 {
		t.Fatalf("a switch that keeps file.txt absent invalidated inodes %v and entries %v, want none", inodes, entries)
	}
	if inodes, entries := pass(baseFile("file.txt", "blob-B", 0o100644)); !slices.Equal(inodes, []fuseops.InodeID{held}) || len(entries) != 0 {
		t.Fatalf("file.txt came back with blob-B and the switch invalidated inodes %v and entries %v, want only the held inode %d", inodes, entries, held)
	}
}

// A write through an open file whose path a switch removed recreates the
// path. The path maps back to the writer's inode, so one number names the
// file. Once another inode took the path, the write returns ESTALE and the
// path keeps that inode's number.
func TestWriteThroughHeldInodeRecreatesPathUnderItsNumber(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		t.Run(fmt.Sprint("created first=", recreated), func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			f.switchTo()
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			want := held
			if recreated {
				create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
				if err := f.fs.CreateFile(f.ctx, create); err != nil {
					t.Fatal(err)
				}
				if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
					t.Fatal(err)
				}
				want = create.Entry.Child
			}
			var wantErr syscall.Errno
			if recreated {
				wantErr = syscall.ESTALE
			}
			if got := kernelErrno(f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("new")})); got != wantErr {
				t.Fatalf("write through inode %d = errno %v, want %v", held, got, wantErr)
			}
			if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != want {
				t.Fatalf("file.txt after a write through inode %d looks up as inode %d, want %d", held, got, want)
			}
			if recreated {
				return
			}
			// The recreated file is the writer's own, so a later switch that
			// keeps it leaves the writer working.
			f.switchTo()
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Offset: 3, Data: []byte("er")}); err != nil {
				t.Fatalf("write through inode %d after a later switch: %v", held, err)
			}
		})
	}
}

// A rename of the parent of an open file whose path a switch removed moves
// the file's path too. A write through it recreates the moved path under the
// file's own number, and getattr finds it there.
func TestWriteAfterParentRenameRecreatesPathUnderHeldNumber(t *testing.T) {
	f := newCacheFixture(t, baseDir("d", "d1"), baseFile("d/file.txt", "blob-A", 0o100644), baseFile("d/keep.txt", "keep", 0o100644))
	d := f.lookup(fuseops.RootInodeID, "d")
	held := f.lookup(d, "file.txt")
	open := f.open(held)
	f.switchTo(baseDir("d", "d2"), baseFile("d/keep.txt", "keep", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "d", NewParent: fuseops.RootInodeID, NewName: "e"}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("new")}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	attr := &fuseops.GetInodeAttributesOp{Inode: held}
	attrErr := f.fs.GetInodeAttributes(f.ctx, attr)
	e := f.lookup(fuseops.RootInodeID, "e")
	if got := f.lookup(e, "file.txt"); got != held || attrErr != nil || attr.Attributes.Size != 3 {
		t.Fatalf("e/file.txt, recreated by a write through inode %d, looks up as inode %d; getattr of inode %d = size %d, %v; want inode %d of size 3", held, got, held, attr.Attributes.Size, attrErr, held)
	}
}

// An open file whose path a switch removed becomes stale when the path comes
// back changed or another inode takes it: through a create, through a later
// switch that restores the path with another blob, or through a lookup or a
// write before that switch's pass. Its handle keeps reading what it read
// before, and its writes and getattr return ESTALE, so neither reaches the
// file now at the path. An unlink of that file does not repoint the stale
// handle either.
func TestHeldInodeLosesItsPathToAnotherInode(t *testing.T) {
	for _, tt := range []struct {
		name string
		// lose makes file.txt another file's and returns its content.
		lose func(*cacheFixture) string
		// writeFirst says the write, not the getattr, is the first op to
		// find the path changed.
		writeFirst bool
		// beforePass says lose runs before the pass of the switch that
		// removed file.txt.
		beforePass bool
	}{
		{"create", func(f *cacheFixture) string {
			create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
			if err := f.fs.CreateFile(f.ctx, create); err != nil {
				f.t.Fatal(err)
			}
			f.t.Cleanup(func() {
				_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
			})
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("mine")}); err != nil {
				f.t.Fatal(err)
			}
			return "mine"
		}, false, false},
		{"create before the pass", func(f *cacheFixture) string {
			create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
			if err := f.fs.CreateFile(f.ctx, create); err != nil {
				f.t.Fatal(err)
			}
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("mine")}); err != nil {
				f.t.Fatal(err)
			}
			if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
				f.t.Fatal(err)
			}
			return "mine"
		}, false, true},
		{"rename over the path", func(f *cacheFixture) string {
			create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "other.txt", Mode: 0o644}
			if err := f.fs.CreateFile(f.ctx, create); err != nil {
				f.t.Fatal(err)
			}
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("mine")}); err != nil {
				f.t.Fatal(err)
			}
			if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
				f.t.Fatal(err)
			}
			if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "other.txt", NewParent: fuseops.RootInodeID, NewName: "file.txt"}); err != nil {
				f.t.Fatal(err)
			}
			return "mine"
		}, false, false},
		{"path returns changed", func(f *cacheFixture) string {
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			if err := f.mount.InvalidateChanged(); err != nil {
				f.t.Fatal(err)
			}
			return "blob-B"
		}, false, false},
		{"lookup before the pass", func(f *cacheFixture) string {
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			f.lookup(fuseops.RootInodeID, "file.txt")
			return "blob-B"
		}, false, false},
		{"write before the pass", func(f *cacheFixture) string {
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			return "blob-B"
		}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			f.switchTo()
			if !tt.beforePass {
				if err := f.mount.InvalidateChanged(); err != nil {
					t.Fatal(err)
				}
			}
			want := tt.lose(f)
			write := func() {
				if got := kernelErrno(f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("old")})); got != syscall.ESTALE {
					t.Fatalf("write through the stale inode = errno %v, want ESTALE", got)
				}
			}
			if tt.writeFirst {
				write()
			}
			if got := openErrno(f, held); got != syscall.ESTALE {
				t.Fatalf("open of the stale inode = errno %v, want ESTALE", got)
			}
			write()
			other := f.lookup(fuseops.RootInodeID, "file.txt")
			if other == held {
				t.Fatalf("file.txt went back to inode %d", held)
			}
			if got := readWholeFile(t, f.fs, other); got != want {
				t.Fatalf("file.txt holds %q after a write through the stale inode, want %q", got, want)
			}
			if tt.name == "create" {
				if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
					t.Fatal(err)
				}
			}
			read := &fuseops.ReadFileOp{Inode: held, Handle: open.Handle, Size: 64, Dst: make([]byte, 64)}
			if err := f.fs.ReadFile(f.ctx, read); err != nil || string(read.Dst[:read.BytesRead]) != "blob-A" {
				t.Fatalf("read through the stale inode = %q, %v; want blob-A, what it read before", read.Dst[:read.BytesRead], err)
			}
		})
	}
}

// A file or directory the kernel holds, such as an open file or a shell's
// working directory, gets its path back when a switch restores the path
// unchanged: the switch pass or a lookup before it maps the path back. Its
// writes, getattr, lookups below it and opendir keep working.
func TestHeldPathComesBackUnchanged(t *testing.T) {
	for _, dir := range []bool{false, true} {
		for _, decidedBy := range []string{"pass", "lookup"} {
			t.Run(fmt.Sprintf("dir=%t decided by %s", dir, decidedBy), func(t *testing.T) {
				nodes := []model.BaseNode{baseFile("file.txt", "blob-A", 0o100644)}
				name := "file.txt"
				if dir {
					nodes = []model.BaseNode{baseDir("d", "d1"), baseFile("d/keep.txt", "keep", 0o100644)}
					name = "d"
				}
				f := newCacheFixture(t, nodes...)
				held := f.lookup(fuseops.RootInodeID, name)
				var open *fuseops.OpenFileOp
				if !dir {
					open = f.open(held)
				}
				f.switchTo()
				if err := f.mount.InvalidateChanged(); err != nil {
					t.Fatal(err)
				}
				f.switchTo(nodes...)
				if decidedBy == "pass" {
					if err := f.mount.InvalidateChanged(); err != nil {
						t.Fatal(err)
					}
				}
				if got := f.lookup(fuseops.RootInodeID, name); got != held {
					t.Fatalf("%s, restored unchanged, looks up as inode %d, want the held %d", name, got, held)
				}
				if dir {
					if got := f.lookup(held, "keep.txt"); got == 0 {
						t.Fatal("lookup of keep.txt below the held directory missed")
					}
					if err := f.fs.OpenDir(f.ctx, &fuseops.OpenDirOp{Inode: held}); err != nil {
						t.Fatalf("OpenDir of the held directory: %v", err)
					}
					return
				}
				if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("new")}); err != nil {
					t.Fatalf("write through the held inode: %v", err)
				}
				f.getattr(held)
				if got := readWholeFile(t, f.fs, held); got != "newb-A" {
					t.Fatalf("file.txt holds %q, want newb-A", got)
				}
			})
		}
	}
}

// An open file unlinked through the mount is retired at once. Its handles
// hold their own copy, so the inode must not get its path back when a later
// switch restores the path unchanged.
func TestUnlinkedOpenFileNeverGetsItsPathBack(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	f.open(held)
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	for _, nodes := range [][]model.BaseNode{nil, {baseFile("file.txt", "blob-A", 0o100644)}} {
		f.switchTo(nodes...)
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	got := f.lookup(fuseops.RootInodeID, "file.txt")
	if got == 0 {
		t.Fatal("file.txt did not come back")
	}
	if got == held {
		t.Fatalf("file.txt came back as inode %d, which an unlink through the mount removed", held)
	}
	if errno := openErrno(f, held); errno != syscall.ESTALE {
		t.Fatalf("open of the unlinked inode = errno %v, want ESTALE", errno)
	}
}

// A directory made where a held directory's path vanished is a new
// directory, even though the path is a directory again. It gets a new number,
// and the held directory is retired.
func TestMkDirOverHeldDirectoryPathGetsNewNumber(t *testing.T) {
	f := newCacheFixture(t, baseDir("d", "d1"), baseFile("d/keep.txt", "keep", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "d")
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	mkdir := &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "d", Mode: os.ModeDir | 0o755}
	if err := f.fs.MkDir(f.ctx, mkdir); err != nil {
		t.Fatal(err)
	}
	if mkdir.Entry.Child == held {
		t.Fatalf("MkDir of d took inode %d of the held directory", held)
	}
	if got := kernelErrno(f.fs.OpenDir(f.ctx, &fuseops.OpenDirOp{Inode: held})); got != syscall.ESTALE {
		t.Fatalf("opendir of the held directory = errno %v, want ESTALE", got)
	}
}

// An open of an unmapped inode can race an op that retires it, such as a
// getattr that finds its path back changed. The open must not hand out a
// handle on the retired inode, whose earlier handles the retirement already
// detached. The open runs twice, because Engine.Do hydrates the blob and
// runs it again, so the getattr runs inside the second run.
func TestOpenRacingARetirementGetsNoHandle(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	f.switchTo(baseFile("file.txt", "blob-BBBB", 0o100644))
	ov := hookOverlay(f)
	var getattrErr error
	ov.beforeLookup = func() {
		ov.beforeLookup = func() {
			getattrErr = f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: held})
		}
	}
	open := &fuseops.OpenFileOp{Inode: held}
	err := f.fs.OpenFile(f.ctx, open)
	if err == nil {
		_ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	}
	if getattrErr != nil {
		t.Fatalf("getattr inside the open = %v", getattrErr)
	}
	if got := kernelErrno(err); got != syscall.ESTALE {
		t.Fatalf("open of inode %d, which a getattr retired during the open, = errno %v, want ESTALE", held, got)
	}
}

// An ftruncate through an open file whose path a switch removed reaches the
// path. A create of the same path must not land inside it, or the ftruncate
// truncates the created file. The test runs the create there whenever the
// lock the ftruncate holds lets it.
func TestCreateCannotLandInsideSetattrThroughUnmappedInode(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.open(held)
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	ov := hookOverlay(f)
	created := createAlongside(f, &ov.beforeLookup)
	size := uint64(2)
	setErr := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Handle: &open.Handle, Size: &size})
	child, err := created()
	if err != nil {
		t.Fatalf("create of file.txt alongside the ftruncate: %v", err)
	}
	if got := readWholeFile(t, f.fs, child); got != "mine" {
		t.Fatalf("a create landed inside the ftruncate through inode %d (%v), and the file it made holds %q, want mine", held, setErr, got)
	}
}

// createAlongside sets hook to start a create of file.txt holding "mine" on
// another goroutine, and to return once the create finished or an op waits
// for handleOps exclusively. The returned func waits for the create and
// returns the inode it made.
func createAlongside(f *cacheFixture, hook *func()) func() (fuseops.InodeID, error) {
	done := make(chan struct{})
	var started bool
	var created fuseops.InodeID
	var err error
	*hook = func() {
		started = true
		go func() {
			defer close(done)
			create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
			if err = f.fs.CreateFile(f.ctx, create); err != nil {
				return
			}
			created = create.Entry.Child
			err = f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("mine")})
			_ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
		}()
		waitDoneOrExclusive(f.fs, done)
	}
	return func() (fuseops.InodeID, error) {
		if !started {
			return 0, errors.New("the hook never ran")
		}
		<-done
		return created, err
	}
}

// waitDoneOrExclusive returns once done is closed, or once an op waits for
// handleOps exclusively.
func waitDoneOrExclusive(fs *ArtifactFuse, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		if !fs.handleOps.TryRLock() {
			return
		}
		fs.handleOps.RUnlock()
		runtime.Gosched()
	}
}

// A write through an open file whose path a switch removed recreates the
// path. A create of the same path must not land inside that write, or the
// write replaces the created file. The test runs the create there whenever
// the lock the write holds lets it.
func TestCreateCannotLandInsideWriteThatRecreatesPath(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.open(held)
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	ov := hookOverlay(f)
	var created fuseops.InodeID
	ov.beforeCreate = func() {
		if !f.fs.handleOps.TryRLock() {
			return
		}
		f.fs.handleOps.RUnlock()
		create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
		if err := f.fs.CreateFile(f.ctx, create); err != nil {
			t.Errorf("CreateFile: %v", err)
			return
		}
		if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("mine")}); err != nil {
			t.Error(err)
		}
		_ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
		created = create.Entry.Child
	}
	writeErr := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("old")})
	if created != 0 {
		if got := readWholeFile(t, f.fs, created); got != "mine" {
			t.Fatalf("a create landed inside the write through inode %d (%v), and the file it made holds %q, want mine", held, writeErr, got)
		}
		return
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != held {
		t.Fatalf("file.txt, recreated by a write through inode %d, looks up as inode %d", held, got)
	}
}

// hookedOverlay runs each hook once: after a rename commits, before or after
// a listing, after a removal, before or after a file create, or before a
// lookup.
type hookedOverlay struct {
	model.OverlayStore
	afterRename, beforeList, afterList, afterRemove, beforeCreate, afterCreate, beforeLookup func()
}

func (o *hookedOverlay) Lookup(ctx context.Context, path string) (model.OverlayEntry, bool, error) {
	fire(&o.beforeLookup)
	return o.OverlayStore.Lookup(ctx, path)
}

func fire(hook *func()) {
	if h := *hook; h != nil {
		*hook = nil
		h()
	}
}

func (o *hookedOverlay) Rename(ctx context.Context, oldPath, newPath string, sourceTracked bool) error {
	err := o.OverlayStore.Rename(ctx, oldPath, newPath, sourceTracked)
	if err == nil {
		fire(&o.afterRename)
	}
	return err
}

func (o *hookedOverlay) RenameTree(ctx context.Context, oldPath, newPath string, src, dst []string) error {
	err := o.OverlayStore.RenameTree(ctx, oldPath, newPath, src, dst)
	if err == nil {
		fire(&o.afterRename)
	}
	return err
}

func (o *hookedOverlay) ListByPrefix(ctx context.Context, prefix string) ([]model.OverlayEntry, error) {
	fire(&o.beforeList)
	out, err := o.OverlayStore.ListByPrefix(ctx, prefix)
	fire(&o.afterList)
	return out, err
}

func (o *hookedOverlay) CreateFile(ctx context.Context, path string, mode uint32) (model.OverlayEntry, error) {
	fire(&o.beforeCreate)
	e, err := o.OverlayStore.CreateFile(ctx, path, mode)
	if err == nil {
		fire(&o.afterCreate)
	}
	return e, err
}

func (o *hookedOverlay) Remove(ctx context.Context, path string) error {
	err := o.OverlayStore.Remove(ctx, path)
	if err == nil {
		fire(&o.afterRemove)
	}
	return err
}

// hookOverlay routes f's overlay through a hookedOverlay.
func hookOverlay(f *cacheFixture) *hookedOverlay {
	ov := &hookedOverlay{OverlayStore: f.resolver.Overlay}
	f.resolver.Overlay, f.fs.engine.Overlay = ov, ov
	return ov
}

// waitParked polls the goroutine stacks until one waits for a lock with every
// one of frames on its stack, and reports whether one did before done closed.
// A rename of a frame's function leaves waitParked polling until go test
// -timeout.
func waitParked(done <-chan struct{}, frames ...string) bool {
	buf := make([]byte, 1<<20)
	for {
		select {
		case <-done:
			return false
		default:
		}
		n := runtime.Stack(buf, true)
		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			// A wait for an RWMutex's write lock first waits in the
			// sync.Mutex inside it, so the reason can name either type.
			header, stack, _ := strings.Cut(g, "\n")
			if strings.Contains(header, "Mutex.") && !slices.ContainsFunc(frames, func(f string) bool { return !strings.Contains(stack, f) }) {
				return true
			}
		}
		runtime.Gosched()
	}
}

// waitRecheck returns once done is closed, or once unmapStillMissing waits
// for a read lock. The only read lock it takes is handleOps, in the switch
// pass's re-check, which a Rename op holds off until it has moved its
// mappings.
func waitRecheck(done <-chan struct{}) {
	waitParked(done, "sync.(*RWMutex).RLock", "(*ArtifactFuse).unmapStillMissing")
}

// newRenameFixture makes src/file.txt through the mount. The kernel holds
// src and has forgotten file.txt.
func newRenameFixture(t *testing.T) (f *cacheFixture, ov *hookedOverlay, src, file fuseops.InodeID) {
	t.Helper()
	f = newCacheFixture(t)
	ov = hookOverlay(f)
	mkdir := &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "src", Mode: os.ModeDir | 0o755}
	if err := f.fs.MkDir(f.ctx, mkdir); err != nil {
		t.Fatal(err)
	}
	create := &fuseops.CreateFileOp{Parent: mkdir.Entry.Child, Name: "file.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	forget(t, f.fs, create.Entry.Child)
	return f, ov, mkdir.Entry.Child, create.Entry.Child
}

func renameSrcToDst(f *cacheFixture) error {
	return f.fs.Rename(context.Background(), &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src", NewParent: fuseops.RootInodeID, NewName: "dst"})
}

func assertMoved(t *testing.T, f *cacheFixture, src, file fuseops.InodeID) {
	t.Helper()
	dst := f.lookup(fuseops.RootInodeID, "dst")
	moved := f.lookup(dst, "file.txt")
	if got, want := []fuseops.InodeID{dst, moved}, []fuseops.InodeID{src, file}; !slices.Equal(got, want) {
		t.Fatalf("inodes of dst and dst/file.txt = %v, want the numbers they had under src: %v", got, want)
	}
}

// A rename commits the overlay before it moves the mappings. A switch pass
// that starts between the two sees the old names missing, and must not unmap
// the numbers the rename is moving.
func TestSwitchPassDuringRenameKeepsMovedNumbers(t *testing.T) {
	f, ov, src, file := newRenameFixture(t)
	passed := make(chan struct{})
	var passErr error
	ov.afterRename = func() {
		go func() {
			defer close(passed)
			passErr = f.mount.InvalidateChanged()
		}()
		waitRecheck(passed)
	}
	if err := renameSrcToDst(f); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	<-passed
	if passErr != nil {
		t.Fatal(passErr)
	}
	assertMoved(t, f, src, file)
}

// A rename can start after the switch pass read the table and commit before
// the pass reads the overlay. It is still moving the mappings when the pass
// would unmap.
func TestSwitchPassWaitsForRenameStartedAfterItsSnapshot(t *testing.T) {
	f, ov, src, file := newRenameFixture(t)
	committed, release, renamed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ov.afterRename = func() {
		close(committed)
		<-release
	}
	ov.beforeList = func() {
		go func() { renamed <- renameSrcToDst(f) }()
		<-committed
	}
	passed := make(chan struct{})
	go func() {
		waitRecheck(passed)
		close(release)
	}()
	passErr := f.mount.InvalidateChanged()
	close(passed)
	if err := <-renamed; err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if passErr != nil {
		t.Fatal(passErr)
	}
	assertMoved(t, f, src, file)
}

// Under steady renames every switch pass overlaps one. A path the switches
// removed still loses its number, so it comes back under a new one.
func TestSwitchPassesUnmapUnderSteadyRenames(t *testing.T) {
	f := newCacheFixture(t, baseFile("gone.txt", "blob-A", 0o100644))
	ov := hookOverlay(f)
	gone := f.lookup(fuseops.RootInodeID, "gone.txt")
	forget(t, f.fs, gone)
	if err := f.fs.MkDir(f.ctx, &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "a", Mode: os.ModeDir | 0o755}); err != nil {
		t.Fatal(err)
	}
	names := []string{"a", "b"}
	for i := range 5 {
		f.switchTo()
		from, to := names[i%2], names[(i+1)%2]
		ov.beforeList = func() {
			if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: from, NewParent: fuseops.RootInodeID, NewName: to}); err != nil {
				t.Error(err)
			}
		}
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	f.switchTo(baseFile("gone.txt", "blob-C", 0o100644))
	if got := f.lookup(fuseops.RootInodeID, "gone.txt"); got == gone {
		t.Fatalf("gone.txt came back with inode %d, which it had before 5 switches removed it", got)
	}
}

// A rename away and back can straddle the switch pass's overlay read: the
// pass sees src.txt missing, but by the time it would unmap, src.txt exists
// again under its number. The pass's re-check keeps the mapping.
func TestRenameAwayAndBackDuringSwitchPassKeepsNumber(t *testing.T) {
	f := newCacheFixture(t)
	ov := hookOverlay(f)
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "src.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	forget(t, f.fs, create.Entry.Child)
	f.switchTo()
	rename := func(from, to string) {
		if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: from, NewParent: fuseops.RootInodeID, NewName: to}); err != nil {
			t.Error(err)
		}
	}
	ov.beforeList = func() {
		rename("src.txt", "tmp.txt")
		ov.afterList = func() { rename("tmp.txt", "src.txt") }
	}
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if got := f.lookup(fuseops.RootInodeID, "src.txt"); got != create.Entry.Child {
		t.Fatalf("src.txt, renamed away and back during the pass, looks up as inode %d, want %d", got, create.Entry.Child)
	}
}

// A write through a held inode can recreate its path after the switch pass
// found the path missing and before the pass unmaps it. The path then still
// maps to the writer's inode, and must keep that number.
func TestWriteDuringSwitchPassKeepsHeldNumber(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644), baseFile("other.txt", "o", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.open(held)
	forget(t, f.fs, f.lookup(fuseops.RootInodeID, "other.txt"))
	f.switchTo(baseFile("other.txt", "o", 0o100644))
	ov := hookOverlay(f)
	wrote := false
	ov.afterList = func() {
		if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("new")}); err != nil {
			t.Error(err)
			return
		}
		wrote = true
	}
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("the write did not run inside the pass")
	}
	if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != held {
		t.Fatalf("file.txt, recreated by a write through inode %d during the pass, looks up as inode %d", held, got)
	}
}

// A rename overlaps the switch pass, so the pass checks file.txt again while
// it holds handleOps. A write through the held inode recreates file.txt after
// the re-check's overlay read and before its unmap, and must keep its number.
func TestWriteDuringSwitchPassRecheckKeepsHeldNumber(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644), baseFile("other.txt", "o", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.open(held)
	forget(t, f.fs, f.lookup(fuseops.RootInodeID, "other.txt"))
	if err := f.fs.MkDir(f.ctx, &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "a", Mode: os.ModeDir | 0o755}); err != nil {
		t.Fatal(err)
	}
	f.switchTo(baseFile("other.txt", "o", 0o100644))
	ov := hookOverlay(f)
	wrote := false
	write := func() {
		if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("new")}); err != nil {
			t.Error(err)
			return
		}
		wrote = true
	}
	lists := 0
	ov.beforeList = func() {
		if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "a", NewParent: fuseops.RootInodeID, NewName: "b"}); err != nil {
			t.Error(err)
		}
		// The pass's own overlay read, then the re-check's.
		ov.afterList = func() {
			lists++
			ov.afterList = func() {
				lists++
				write()
			}
		}
	}
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if !wrote || lists != 2 {
		t.Fatalf("the write did not run inside the re-check: wrote %v after %d overlay reads, want 2", wrote, lists)
	}
	if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != held {
		t.Fatalf("file.txt, recreated by a write through inode %d during the re-check, looks up as inode %d", held, got)
	}
}

// A create can land after the switch pass read the mappings and the overlay.
// The pass then finds the path missing, and must leave the created path's
// mapping alone.
func TestCreateDuringSwitchPassKeepsItsNumber(t *testing.T) {
	f := newCacheFixture(t, baseFile("gone.txt", "gone", 0o100644))
	forget(t, f.fs, f.lookup(fuseops.RootInodeID, "gone.txt"))
	f.switchTo()
	ov := hookOverlay(f)
	var created fuseops.InodeID
	ov.afterList = func() {
		op := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "gone.txt", Mode: 0o644}
		if err := f.fs.CreateFile(f.ctx, op); err != nil {
			t.Error(err)
			return
		}
		_ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
		created = op.Entry.Child
	}
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if created == 0 {
		t.Fatal("the create did not run inside the switch pass")
	}
	if got := f.lookup(fuseops.RootInodeID, "gone.txt"); got != created {
		t.Fatalf("lookup of gone.txt after the pass = inode %d, want %d, the number its create got", got, created)
	}
}

// RmDir unmaps its path after the overlay removes it. A MkDir of the same name
// must not land between the two steps, or the unmap drops the new directory's
// number. The test runs the MkDir there whenever the lock RmDir holds lets it.
func TestMkDirCannotLandInsideRmDir(t *testing.T) {
	f := newCacheFixture(t)
	ov := hookOverlay(f)
	if err := f.fs.MkDir(f.ctx, &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "dir", Mode: os.ModeDir | 0o755}); err != nil {
		t.Fatal(err)
	}
	mkdir := func() fuseops.InodeID {
		op := &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "dir", Mode: os.ModeDir | 0o755}
		if err := f.fs.MkDir(f.ctx, op); err != nil {
			t.Fatalf("MkDir: %v", err)
		}
		return op.Entry.Child
	}
	var made fuseops.InodeID
	ov.afterRemove = func() {
		if f.fs.handleOps.TryRLock() {
			f.fs.handleOps.RUnlock()
			made = mkdir()
		}
	}
	if err := f.fs.RmDir(f.ctx, &fuseops.RmDirOp{Parent: fuseops.RootInodeID, Name: "dir"}); err != nil {
		t.Fatalf("RmDir: %v", err)
	}
	if made == 0 {
		made = mkdir()
	}
	if got := f.lookup(fuseops.RootInodeID, "dir"); got != made {
		t.Fatalf("lookup of dir = inode %d, want %d, the number its MkDir got", got, made)
	}
}

// MissingPaths reads the overlay once for every directory. It must report the
// same paths as a merged listing of each directory: over overlay deletes, a
// renamed base directory and file, implicit directories at the root and below
// it, a directory replaced by a file and a file replaced by a directory.
// Engine.Identity, which the switch pass uses for held inodes, must agree.
func TestMissingPathsMatchesPerDirectoryListings(t *testing.T) {
	gen1 := []model.BaseNode{
		baseDir("a", "a1"), baseFile("a/x.txt", "x", 0o100644), baseDir("a/b", "ab1"), baseFile("a/b/y.txt", "y", 0o100644),
		baseDir("c", "c1"), baseFile("c/z.txt", "z", 0o100644), baseDir("c/deep", "cd1"), baseFile("c/deep/q.txt", "q", 0o100644),
		baseDir("d", "d1"), baseFile("d/w.txt", "w", 0o100644),
		baseDir("e", "e1"), baseFile("e/v.txt", "v", 0o100644),
		baseDir("f", "f1"), baseFile("f/u.txt", "u", 0o100644),
		baseDir("g", "g1"), baseFile("g/s.txt", "s", 0o100644),
		baseFile("k", "k", 0o100644), baseFile("top.txt", "top", 0o100644),
		baseDir("m", "m1"), baseDir("m/n", "mn1"), baseFile("m/n/p.txt", "p", 0o100644),
	}
	f := newCacheFixture(t, gen1...)
	ctx := f.ctx
	var root fuseops.InodeID = fuseops.RootInodeID
	ino := map[string]fuseops.InodeID{".": root}
	for _, n := range gen1 {
		ino[n.Path] = f.lookup(ino[filepath.Dir(n.Path)], filepath.Base(n.Path))
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	created := func(parent fuseops.InodeID, name string) {
		t.Helper()
		op := &fuseops.CreateFileOp{Parent: parent, Name: name, Mode: 0o644}
		must(f.fs.CreateFile(ctx, op))
		must(f.fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: op.Handle}))
	}
	must(f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: ino["a"], Name: "x.txt"}))
	must(f.fs.Rename(ctx, &fuseops.RenameOp{OldParent: root, OldName: "c", NewParent: root, NewName: "c2"}))
	must(f.fs.Rename(ctx, &fuseops.RenameOp{OldParent: root, OldName: "top.txt", NewParent: root, NewName: "top2.txt"}))
	created(ino["d"], "new.txt")
	must(f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: ino["f"], Name: "u.txt"}))
	must(f.fs.RmDir(ctx, &fuseops.RmDirOp{Parent: root, Name: "f"}))
	h := &fuseops.MkDirOp{Parent: root, Name: "h", Mode: os.ModeDir | 0o755}
	must(f.fs.MkDir(ctx, h))
	i := &fuseops.MkDirOp{Parent: h.Entry.Child, Name: "i", Mode: os.ModeDir | 0o755}
	must(f.fs.MkDir(ctx, i))
	created(i.Entry.Child, "j.txt")
	must(f.fs.CreateSymlink(ctx, &fuseops.CreateSymlinkOp{Parent: root, Name: "link", Target: "a"}))
	// g/t.txt and m/n/o.txt are untracked, and the switch removes g and m/n,
	// so both become implicit directories.
	created(ino["g"], "t.txt")
	created(ino["m/n"], "o.txt")

	f.switchTo(
		baseDir("a", "a2"), baseFile("a/x.txt", "x", 0o100644), baseDir("a/b", "ab1"), baseFile("a/b/y.txt", "y2", 0o100644),
		baseDir("c", "c1"), baseFile("c/z.txt", "z", 0o100644), baseDir("c/deep", "cd1"), baseFile("c/deep/q.txt", "q", 0o100644),
		baseFile("e", "e-file", 0o100644),
		baseDir("f", "f1"), baseFile("f/u.txt", "u", 0o100644),
		baseDir("k", "k2"), baseFile("k/m.txt", "m", 0o100644),
		baseFile("top.txt", "top", 0o100644), baseFile("n.txt", "n", 0o100644),
		baseDir("m", "m2"),
	)

	set := map[string]bool{}
	for _, n := range gen1 {
		set[n.Path] = true
	}
	all, err := f.resolver.Overlay.ListByPrefix(ctx, ".")
	must(err)
	for _, e := range all {
		set[e.Path] = true
	}
	for _, p := range []string{"d", "d/new.txt", "d/w.txt", "e", "e/v.txt", "k/m.txt", "n.txt", "c2/z.txt", "c2/deep/q.txt", "c2/deep", "h/i/j.txt", "zz/none", "a/b/nope", "g", "g/t.txt", "g/s.txt", "link", "top2.txt"} {
		set[p] = true
	}
	delete(set, ".")
	var paths []string
	for p := range set {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	got, err := f.fs.engine.MissingPaths(ctx, paths)
	must(err)
	slices.Sort(got)

	var want []string
	for _, p := range paths {
		listed, _, _, err := f.fs.engine.ReadDir(ctx, filepath.Dir(p))
		must(err)
		if !slices.ContainsFunc(listed, func(e ReaddirEntry) bool { return e.Name == filepath.Base(p) }) {
			want = append(want, p)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("MissingPaths = %v, merged listings of each parent = %v", got, want)
	}
	for _, p := range paths {
		_, exists, err := f.fs.engine.Identity(p)
		must(err)
		if missing := slices.Contains(want, p); exists == missing {
			t.Errorf("Identity(%q) exists = %t, but the merged listing of its parent says missing = %t", p, exists, missing)
		}
	}
}

// A directory that a HEAD change leaves holding only untracked files becomes
// implicit, and still exists. It keeps its inode number across the switch,
// whether the kernel still holds it or has forgotten it.
func TestImplicitDirectoryKeepsInodeNumberAcrossSwitch(t *testing.T) {
	for _, held := range []bool{true, false} {
		t.Run(fmt.Sprint("held=", held), func(t *testing.T) {
			f := newCacheFixture(t, baseDir("d", "d1"), baseFile("d/file.txt", "blob-A", 0o100644))
			d := f.lookup(fuseops.RootInodeID, "d")
			create := &fuseops.CreateFileOp{Parent: d, Name: "new.txt", Mode: 0o644}
			if err := f.fs.CreateFile(f.ctx, create); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
				t.Fatal(err)
			}
			if !held {
				forget(t, f.fs, d)
			}
			f.switchTo()
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if got := f.lookup(fuseops.RootInodeID, "d"); got != d {
				t.Fatalf("d, implicit after the switch, looks up as inode %d, want %d", got, d)
			}
		})
	}
}

// A getattr of an unmapped inode whose path came back changed, before the
// switch pass, decides the inode as the pass does: it retires the inode, and
// answers with the attributes of the content the inode's handle reads.
// Recording the new file's identity on the inode would let the pass, or the
// inode's next write, map the path back to it, and its old handle would write
// into the new HEAD's file.
func TestGetattrBeforeThePassRetiresInodeWhosePathCameBackChanged(t *testing.T) {
	for _, pass := range []bool{true, false} {
		t.Run(fmt.Sprint("pass before the write=", pass), func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			f.switchTo()
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			f.switchTo(baseFile("file.txt", "blob-BBBB", 0o100644))
			getattr := &fuseops.GetInodeAttributesOp{Inode: held}
			getattrErr := f.fs.GetInodeAttributes(f.ctx, getattr)
			if pass {
				if err := f.mount.InvalidateChanged(); err != nil {
					t.Fatal(err)
				}
			}
			writeErr := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("OLD")})
			if got := readWholeFile(t, f.fs, f.lookup(fuseops.RootInodeID, "file.txt")); got != "blob-BBBB" {
				t.Fatalf("a write through inode %d reached the new HEAD's file.txt, which holds %q", held, got)
			}
			if getattrErr != nil || getattr.Attributes.Size != uint64(len("blob-A")) {
				t.Errorf("getattr = size %d, %v; want the size of blob-A, which the inode's handle reads", getattr.Attributes.Size, getattrErr)
			}
			if got := openErrno(f, held); got != syscall.ESTALE {
				t.Errorf("open of inode %d = errno %v, want ESTALE", held, got)
			}
			if got := kernelErrno(writeErr); got != syscall.ESTALE {
				t.Errorf("write = errno %v, want ESTALE", got)
			}
		})
	}
}

// A create handle holds no file of its own. Its file is committed, so the
// switch drops the overlay entry, and later HEAD changes remove the path and
// bring it back as another file, which retires the inode. A read or fsync
// through the handle then returns ESTALE: a read by path would serve the new
// HEAD's file. A getattr of the inode returns ESTALE too, because its handle
// holds no content to answer from.
func TestRetiredCreateHandleReadsNothing(t *testing.T) {
	other := baseFile("other.txt", "blob-O", 0o100644)
	f := newCacheFixture(t, other)
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	held := create.Entry.Child
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: create.Handle, Data: []byte("data\n")}); err != nil {
		t.Fatal(err)
	}
	f.switchTo(other, baseFile("file.txt", blobOID("data\n"), 0o100644))
	if _, ok, err := f.resolver.Overlay.Lookup(f.ctx, "file.txt"); err != nil || ok {
		t.Fatalf("overlay holds file.txt after its content was committed: %t, %v", ok, err)
	}
	for _, nodes := range [][]model.BaseNode{{other}, {other, baseFile("file.txt", "blob-BBBB", 0o100644)}} {
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
		f.switchTo(nodes...)
	}
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	read := &fuseops.ReadFileOp{Inode: held, Handle: create.Handle, Size: 64, Dst: make([]byte, 64)}
	readErr := f.fs.ReadFile(f.ctx, read)
	if got := string(read.Dst[:read.BytesRead]); got != "" || kernelErrno(readErr) != syscall.ESTALE {
		t.Errorf("read through the retired create handle = %q, errno %v, want ESTALE", got, kernelErrno(readErr))
	}
	if got := kernelErrno(f.fs.SyncFile(f.ctx, &fuseops.SyncFileOp{Inode: held, Handle: create.Handle})); got != syscall.ESTALE {
		t.Errorf("fsync through the retired create handle = errno %v, want ESTALE", got)
	}
	if got := kernelErrno(f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: held})); got != syscall.ESTALE {
		t.Errorf("getattr of the retired inode, whose handle holds no file = errno %v, want ESTALE", got)
	}
}

// A handle open on an overlay file holds that file, but only an unlink or
// rename over the handle's path gives it a copy of its own. A handle that
// moves with its file in a rename keeps none. Once a commit drops the overlay
// entry and later HEAD changes retire the inode, a write through such a handle
// returns ESTALE. It would otherwise succeed into a file no path names.
func TestWriteThroughRetiredOverlayHandleReturnsESTALE(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(fmt.Sprint("renamed=", renamed), func(t *testing.T) {
			other := baseFile("other.txt", "blob-O", 0o100644)
			f := newCacheFixture(t, other)
			create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "a.txt", Mode: 0o644}
			if err := f.fs.CreateFile(f.ctx, create); err != nil {
				t.Fatal(err)
			}
			held := create.Entry.Child
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: create.Handle, Data: []byte("data\n")}); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
				t.Fatal(err)
			}
			open := f.open(held)
			name := "a.txt"
			if renamed {
				name = "file.txt"
				if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "a.txt", NewParent: fuseops.RootInodeID, NewName: name}); err != nil {
					t.Fatal(err)
				}
			}
			committed := baseFile(name, blobOID("data\n"), 0o100644)
			for _, nodes := range [][]model.BaseNode{{other, committed}, {other}, {other, committed}} {
				f.switchTo(nodes...)
				if err := f.mount.InvalidateChanged(); err != nil {
					t.Fatal(err)
				}
			}
			if got := openErrno(f, held); got != syscall.ESTALE {
				t.Fatalf("open of inode %d after the switches = errno %v, want ESTALE", held, got)
			}
			writeErr := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("LOST")})
			if got := kernelErrno(writeErr); got != syscall.ESTALE {
				t.Fatalf("write through the retired handle = errno %v, want ESTALE", got)
			}
			if got := readWholeFile(t, f.fs, f.lookup(fuseops.RootInodeID, name)); got != committed.ObjectOID {
				t.Fatalf("%s holds %q, want the committed blob's content %q", name, got, committed.ObjectOID)
			}
		})
	}
}

// A shared write through a held inode can reach its path after a switch
// removed the path, before or after the pass unmaps it. A create of the path
// holds handleOps exclusively, so it runs after the write, and the write does
// not land in the created file.
func TestCreateWaitsForSharedWriteThroughHeldInode(t *testing.T) {
	for _, pass := range []bool{true, false} {
		t.Run(fmt.Sprint("pass before the create=", pass), func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			f.switchTo()
			ov := hookOverlay(f)
			var start func()
			created := createAlongside(f, &start)
			ov.beforeLookup = func() {
				if pass {
					if err := f.mount.InvalidateChanged(); err != nil {
						t.Error(err)
					}
				}
				start()
			}
			writeErr := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("OLD")})
			child, createErr := created()
			if createErr == nil {
				if got := readWholeFile(t, f.fs, child); got != "mine" {
					t.Fatalf("the write through inode %d (%v) landed in the created file, which holds %q", held, writeErr, got)
				}
				return
			}
			if got := kernelErrno(createErr); got != syscall.EEXIST {
				t.Fatalf("create of file.txt = %v, want success or EEXIST", createErr)
			}
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != held {
				t.Fatalf("file.txt, which the write through inode %d recreated, looks up as inode %d", held, got)
			}
		})
	}
}

// A write through a held inode recreates its path, which a switch removed. A
// lookup or listing of the path between the write's overlay create and its
// mapping of the path would retire the writer. It holds handleOps
// exclusively instead, so it runs after the write and finds the path mapped
// to the writer.
func TestLookupWaitsForWriteThatRecreatesPath(t *testing.T) {
	for _, via := range []string{"lookup", "listing"} {
		t.Run(via, func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			f.switchTo()
			ov := hookOverlay(f)
			ov.beforeLookup = func() {
				if err := f.mount.InvalidateChanged(); err != nil {
					t.Error(err)
				}
			}
			done := make(chan struct{})
			var lookupErr error
			ov.afterCreate = func() {
				go func() {
					defer close(done)
					lookupErr = lookUpAlongside(f, via)
				}()
				waitDoneOrExclusive(f.fs, done)
			}
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("OLD")}); err != nil {
				t.Fatal(err)
			}
			if ov.afterCreate != nil {
				t.Fatal("the write did not create file.txt in the overlay")
			}
			<-done
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Offset: 3, Data: []byte("ER")}); err != nil {
				t.Fatalf("second write through inode %d = %v", held, err)
			}
			if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != held {
				t.Fatalf("file.txt, which writes through inode %d recreated, looks up as inode %d", held, got)
			}
		})
	}
}

// lookUpAlongside looks file.txt up in the root, through a lookup or through
// a listing with entries.
func lookUpAlongside(f *cacheFixture, via string) error {
	if via == "lookup" {
		return f.fs.LookUpInode(f.ctx, &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
	}
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := f.fs.OpenDir(f.ctx, open); err != nil {
		return err
	}
	defer func() { _ = f.fs.ReleaseDirHandle(f.ctx, &fuseops.ReleaseDirHandleOp{Handle: open.Handle}) }()
	plus := &fuseops.ReadDirPlusOp{}
	plus.Handle = open.Handle
	plus.Dst = make([]byte, 64*1024)
	return f.fs.ReadDirPlus(f.ctx, plus)
}

// An ftruncate through a held inode whose path came back as another file,
// before the switch pass, must not reach that file. It returns ESTALE, and
// the new HEAD's file keeps its content.
func TestSetattrThroughHeldInodeLeavesReturnedPathAlone(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.open(held)
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	f.switchTo(baseFile("file.txt", "blob-BBBB", 0o100644))
	size := uint64(2)
	err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Handle: &open.Handle, Size: &size})
	if got := readWholeFile(t, f.fs, f.lookup(fuseops.RootInodeID, "file.txt")); got != "blob-BBBB" {
		t.Fatalf("an ftruncate through inode %d reached the new HEAD's file.txt, which holds %q", held, got)
	}
	if got := kernelErrno(err); got != syscall.ESTALE {
		t.Fatalf("ftruncate through inode %d = errno %v, want ESTALE", held, got)
	}
}

// A directory renamed through the mount onto the vanished path of a held
// directory takes the path. The held directory is retired: an opendir of it
// returns ESTALE instead of listing the moved directory, and a getattr does
// not map the path back to it.
func TestRenameOntoHeldDirectoryPathRetiresIt(t *testing.T) {
	moved := []model.BaseNode{baseDir("e", "e1"), baseFile("e/other.txt", "other", 0o100644)}
	f := newCacheFixture(t, append([]model.BaseNode{baseDir("d", "d1"), baseFile("d/keep.txt", "keep", 0o100644)}, moved...)...)
	held := f.lookup(fuseops.RootInodeID, "d")
	e := f.lookup(fuseops.RootInodeID, "e")
	f.switchTo(moved...)
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "e", NewParent: fuseops.RootInodeID, NewName: "d"}); err != nil {
		t.Fatal(err)
	}
	if got := kernelErrno(f.fs.OpenDir(f.ctx, &fuseops.OpenDirOp{Inode: held})); got != syscall.ESTALE {
		t.Fatalf("opendir of the held directory %d = errno %v, want ESTALE", held, got)
	}
	if err := f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: held}); err != nil {
		t.Fatalf("getattr of the held directory %d = %v", held, err)
	}
	if got := f.lookup(fuseops.RootInodeID, "d"); got != e {
		t.Fatalf("d looks up as inode %d, want %d, the directory renamed there", got, e)
	}
}

// A kernel that renames a name it cached, as FSKit can, sends no lookup
// first. A rename of a path that came back unchanged for a held inode must
// move that inode as a mapped one, with or without a getattr first, so the
// next switch pass does not retire the inode the rename moved.
func TestRenameOfReturnedPathWithoutLookupKeepsHeldInode(t *testing.T) {
	for _, getattr := range []bool{true, false} {
		t.Run(fmt.Sprint("getattr=", getattr), func(t *testing.T) {
			f := newCacheFixtureOn(t, BackendFSKit, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			f.switchTo()
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			f.switchTo(baseFile("file.txt", "blob-A", 0o100644))
			if getattr {
				f.getattr(held)
			}
			if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "file.txt", NewParent: fuseops.RootInodeID, NewName: "moved.txt"}); err != nil {
				t.Fatal(err)
			}
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: held}); err != nil {
				t.Fatalf("getattr of the inode the rename moved = %v", err)
			}
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("new")}); err != nil {
				t.Fatalf("write through the inode the rename moved = %v", err)
			}
			if got := f.lookup(fuseops.RootInodeID, "moved.txt"); got != held {
				t.Fatalf("moved.txt looks up as inode %d, want the held %d", got, held)
			}
		})
	}
}

// An unlink or rmdir of a path that came back unchanged for a held inode,
// with no lookup first, retires that inode as an unlink of a mapped path does.
func TestUnlinkOfReturnedPathWithoutLookupRetiresHeldInode(t *testing.T) {
	t.Run("unlink", func(t *testing.T) {
		f := newCacheFixtureOn(t, BackendFSKit, baseFile("file.txt", "blob-A", 0o100644))
		held := f.lookup(fuseops.RootInodeID, "file.txt")
		f.open(held)
		f.switchTo()
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
		f.switchTo(baseFile("file.txt", "blob-A", 0o100644))
		if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
			t.Fatal(err)
		}
		if got := openErrno(f, held); got != syscall.ESTALE {
			t.Fatalf("open of the inode whose path an unlink removed = errno %v, want ESTALE", got)
		}
	})
	t.Run("rmdir", func(t *testing.T) {
		f := newCacheFixtureOn(t, BackendFSKit, baseDir("d", "d1"))
		held := f.lookup(fuseops.RootInodeID, "d")
		f.switchTo()
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
		f.switchTo(baseDir("d", "d1"))
		if err := f.fs.RmDir(f.ctx, &fuseops.RmDirOp{Parent: fuseops.RootInodeID, Name: "d"}); err != nil {
			t.Fatal(err)
		}
		if got := kernelErrno(f.fs.OpenDir(f.ctx, &fuseops.OpenDirOp{Inode: held})); got != syscall.ESTALE {
			t.Fatalf("opendir of the directory an rmdir removed = errno %v, want ESTALE", got)
		}
	})
}

// failingRemoveOverlay fails every Remove.
type failingRemoveOverlay struct{ model.OverlayStore }

func (o *failingRemoveOverlay) Remove(context.Context, string) error {
	return errors.New("remove failed")
}

// An unlink that fails after it pointed the open handles at the path's
// overlay file leaves them unpinned: they hold no copy of their own. A later
// retirement then makes a write through them return ESTALE, instead of
// succeeding into a file that a commit removed.
func TestFailedUnlinkLeavesHandleUnpinned(t *testing.T) {
	other := baseFile("other.txt", "blob-O", 0o100644)
	f := newCacheFixture(t, other)
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	held := create.Entry.Child
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: create.Handle, Data: []byte("data\n")}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	open := f.open(held)
	ov := f.resolver.Overlay
	failing := &failingRemoveOverlay{ov}
	f.resolver.Overlay, f.fs.engine.Overlay = failing, failing
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err == nil {
		t.Fatal("unlink succeeded through an overlay whose Remove fails")
	}
	f.resolver.Overlay, f.fs.engine.Overlay = ov, ov
	committed := baseFile("file.txt", blobOID("data\n"), 0o100644)
	for _, nodes := range [][]model.BaseNode{{other, committed}, {other}, {other, committed}} {
		f.switchTo(nodes...)
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	if got := kernelErrno(f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("LOST")})); got != syscall.ESTALE {
		t.Fatalf("write through the retired handle = errno %v, want ESTALE", got)
	}
}

// A lookup of a held path while the path is missing runs holding handleOps
// shared. A tool that polls the path on a backend that caches no negative
// entries would otherwise make every lookup wait for all other ops.
func TestLookupOfMissingHeldPathRunsShared(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	f.open(held)
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	f.fs.handleOps.RLock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.fs.LookUpInode(f.ctx, &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
	}()
	waitDoneOrExclusive(f.fs, done)
	select {
	case <-done:
		f.fs.handleOps.RUnlock()
	default:
		f.fs.handleOps.RUnlock()
		<-done
		t.Fatal("a lookup of the missing path file.txt waited for handleOps exclusively")
	}
}

// Linux fetches an open file's attributes before a read once they expire,
// and fails the read with any error the getattr returns. A getattr of a held
// inode whose path is away, or of an unlinked one, therefore answers with the
// attributes of the content the inode's handles read.
func TestGetattrOfHeldInodeAnswersFromItsContent(t *testing.T) {
	t.Run("path away", func(t *testing.T) {
		f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
		held := f.lookup(fuseops.RootInodeID, "file.txt")
		f.open(held)
		f.switchTo()
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
		op := &fuseops.GetInodeAttributesOp{Inode: held}
		if err := f.fs.GetInodeAttributes(f.ctx, op); err != nil || op.Attributes.Size != uint64(len("blob-A")) {
			t.Fatalf("getattr while file.txt is away = size %d, %v; want the size of blob-A", op.Attributes.Size, err)
		}
	})
	t.Run("unlinked", func(t *testing.T) {
		f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
		held := f.lookup(fuseops.RootInodeID, "file.txt")
		open := f.open(held)
		if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
			t.Fatal(err)
		}
		data := []byte("written after the unlink")
		if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: data}); err != nil {
			t.Fatal(err)
		}
		op := &fuseops.GetInodeAttributesOp{Inode: held}
		if err := f.fs.GetInodeAttributes(f.ctx, op); err != nil || op.Attributes.Size != uint64(len(data)) {
			t.Fatalf("getattr of the unlinked file = size %d, %v; want %d, the size of its own copy", op.Attributes.Size, err, len(data))
		}
	})
}

// POSIX applies ftruncate, fchmod and futimens through a descriptor to its
// open file. After an unlink of an open file's path, or a rename over it,
// through the mount, the file's handles hold their own copy, so a setattr of
// the retired inode changes that copy, and a getattr and a read through the
// handle show the change. Linux sends the handle only with an ftruncate.
func TestSetattrOfUnlinkedOpenFileChangesItsCopy(t *testing.T) {
	size := uint64(4)
	mode := os.FileMode(0o600)
	mtime := time.Unix(1_700_000_000, 123_456_000)
	for _, removal := range []string{"unlink", "rename over"} {
		for _, c := range []struct {
			name    string
			op      fuseops.SetInodeAttributesOp
			handle  bool
			attr    func(fuseops.InodeAttributes) any
			want    any
			content string
		}{
			{"ftruncate", fuseops.SetInodeAttributesOp{Size: &size}, true, func(a fuseops.InodeAttributes) any { return a.Size }, size, "blob"},
			{"fchmod", fuseops.SetInodeAttributesOp{Mode: &mode}, false, func(a fuseops.InodeAttributes) any { return a.Mode.Perm() }, mode, "blob-AAAA"},
			{"futimens", fuseops.SetInodeAttributesOp{Atime: &mtime, Mtime: &mtime}, false, func(a fuseops.InodeAttributes) any { return a.Mtime.UnixNano() }, mtime.UnixNano(), "blob-AAAA"},
		} {
			t.Run(removal+"/"+c.name, func(t *testing.T) {
				f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
				held := f.lookup(fuseops.RootInodeID, "file.txt")
				open := f.open(held)
				if c.handle {
					open = f.openRW(held)
				}
				var err error
				if removal == "unlink" {
					err = f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
				} else {
					create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "other.txt", Mode: 0o644}
					if err := f.fs.CreateFile(f.ctx, create); err != nil {
						t.Fatal(err)
					}
					if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
						t.Fatal(err)
					}
					err = f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "other.txt", NewParent: fuseops.RootInodeID, NewName: "file.txt"})
				}
				if err != nil {
					t.Fatal(err)
				}
				op := c.op
				op.Inode = held
				if c.handle {
					op.Handle = &open.Handle
				}
				if err := f.fs.SetInodeAttributes(f.ctx, &op); err != nil {
					t.Fatalf("%s of the open file after the %s = %v", c.name, removal, err)
				}
				get := &fuseops.GetInodeAttributesOp{Inode: held}
				if err := f.fs.GetInodeAttributes(f.ctx, get); err != nil {
					t.Fatalf("getattr of the open file after the %s = %v", c.name, err)
				}
				if got := c.attr(op.Attributes); got != c.want {
					t.Fatalf("%s replied %v, want %v", c.name, got, c.want)
				}
				if got := c.attr(get.Attributes); got != c.want {
					t.Fatalf("getattr after the %s = %v, want %v", c.name, got, c.want)
				}
				if got := mustReadHandle(t, f, held, open.Handle); got != c.content || get.Attributes.Size != uint64(len(got)) {
					t.Fatalf("after the %s, the handle reads %q and getattr reports size %d; want %q", c.name, got, get.Attributes.Size, c.content)
				}
			})
		}
	}
}

// On an FSKit mount, artifact-fs checks permissions itself. A truncate of an
// unlinked open file whose copy a chmod made 0444 fails with EACCES when it
// comes through no handle, as it does for a file a path names.
func TestTruncateOfReadOnlyUnlinkedCopyChecksPermissions(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.open(held)
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	checking := permissionCheckingFS{f.fs}
	ro := os.FileMode(0o444)
	if err := checking.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Mode: &ro}); err != nil {
		t.Fatalf("fchmod of the unlinked open file = %v", err)
	}
	zero := uint64(0)
	if got := kernelErrno(checking.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Size: &zero})); got != syscall.EACCES {
		t.Fatalf("truncate without a handle = errno %v, want EACCES", got)
	}
	if got := mustReadHandle(t, f, held, open.Handle); got != "blob-AAAA" {
		t.Fatalf("the handle reads %q, want blob-AAAA kept", got)
	}
}

// On an FSKit mount, a handle opened for writing before an fchmod to 0444 of
// an unlinked open file still truncates and writes its copy.
func TestWritableHandleOfUnlinkedFileKeepsWritingAfterFchmod(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.openRW(held)
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	checking := permissionCheckingFS{f.fs}
	ro := os.FileMode(0o444)
	if err := checking.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Mode: &ro}); err != nil {
		t.Fatalf("fchmod of the unlinked open file = %v", err)
	}
	two := uint64(2)
	if err := checking.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Handle: &open.Handle, Size: &two}); err != nil {
		t.Fatalf("ftruncate through the writable handle = %v", err)
	}
	if err := checking.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Offset: 2, Data: []byte("zz")}); err != nil {
		t.Fatalf("write through the writable handle = %v", err)
	}
	if got := mustReadHandle(t, f, held, open.Handle); got != "blzz" {
		t.Fatalf("the handle reads %q, want blzz", got)
	}
}

// A handle that opened a file's overlay file keeps it after a commit, whose
// reconcile drops the overlay entry and removes that file, until the switch
// pass closes it. The test runs no pass, as when the pass fails to look up the
// path. An unlink then copies the base for the handles open on the path.
// Every one of them, the older one too, must hold that one copy, so a write or
// an ftruncate through one shows through the others, as through two
// descriptors of one file. A write through the older handle between the
// commit and the unlink goes by path, so the copy holds it.
func TestPinnedHandlesOfOneFileShareOneCopy(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprint("write after commit=", write), func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			writeThroughHandle(t, f, held, "X")
			older := f.openRW(held)
			want := mustReadHandle(t, f, held, older.Handle)
			commitFile(t, f, want)
			if write {
				if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: older.Handle, Data: []byte("Y")}); err != nil {
					t.Fatal(err)
				}
				want = "Y" + want[1:]
			}
			newer := f.openRW(f.lookup(fuseops.RootInodeID, "file.txt"))
			if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: older.Handle, Data: []byte("Z")}); err != nil {
				t.Fatal(err)
			}
			want = "Z" + want[1:]
			for name, h := range map[string]fuseops.HandleID{"older": older.Handle, "newer": newer.Handle} {
				if got := mustReadHandle(t, f, held, h); got != want {
					t.Fatalf("after a write through the older handle, the %s one reads %q, want %q", name, got, want)
				}
			}
			two := uint64(2)
			if err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Handle: &newer.Handle, Size: &two}); err != nil {
				t.Fatalf("ftruncate through the newer handle = %v", err)
			}
			for name, h := range map[string]fuseops.HandleID{"older": older.Handle, "newer": newer.Handle} {
				if got := mustReadHandle(t, f, held, h); got != want[:2] {
					t.Fatalf("after an ftruncate through the newer handle, the %s one reads %q, want %q", name, got, want[:2])
				}
			}
		})
	}
}

// A handle that opened a file's overlay file must not keep it after a commit,
// whose reconcile drops the overlay entry and removes that file. Otherwise,
// after a later HEAD change, it reads the removed file while a handle of the
// same inode opened after the change reads the new blob.
func TestHandlesOfOneInodeReadOneContentAfterTheirOverlayFileIsReconciled(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	writeThroughHandle(t, f, held, "X")
	older := f.open(held)
	if got := mustReadHandle(t, f, held, older.Handle); got != "Xlob-AAAA" {
		t.Fatalf("the older handle reads %q, want Xlob-AAAA", got)
	}
	commitFile(t, f, "Xlob-AAAA")
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	f.switchTo(baseFile("file.txt", "blob-BBBB", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if id := f.lookup(fuseops.RootInodeID, "file.txt"); id != held {
		t.Fatalf("file.txt looks up as inode %d after the HEAD change, want %d", id, held)
	}
	newer := f.open(held)
	for name, h := range map[string]fuseops.HandleID{"older": older.Handle, "newer": newer.Handle} {
		if got := mustReadHandle(t, f, held, h); got != "blob-BBBB" {
			t.Fatalf("after the HEAD change, the %s handle reads %q, want blob-BBBB", name, got)
		}
	}
}

// commitFile switches to a generation whose file.txt holds content, so the
// switch's reconcile drops the overlay entry of file.txt, which holds content,
// and removes the entry's file. It runs no switch pass.
func commitFile(t *testing.T, f *cacheFixture, content string) {
	t.Helper()
	if _, ok, err := f.fs.engine.Overlay.Lookup(f.ctx, "file.txt"); err != nil || !ok {
		t.Fatalf("overlay entry of file.txt before the commit = %t, %v; want it recorded", ok, err)
	}
	committed := baseFile("file.txt", blobOID(content), 0o100644)
	committed.SizeBytes = int64(len(content))
	f.switchTo(committed)
	if err := os.WriteFile(f.blobs.pathsByOID[committed.ObjectOID], []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.fs.engine.Overlay.Lookup(f.ctx, "file.txt"); err != nil || ok {
		t.Fatalf("overlay entry of file.txt after the commit = %t, %v; want it dropped", ok, err)
	}
}

// An open can read a file's overlay file before a commit's reconcile removes
// it, and register its handle only after the switch pass closed the overlay
// files that handles held. The handle must not keep the removed file. After a
// later HEAD change, a read that comes before that change's pass would read
// the removed file, while a handle opened since reads the new blob.
func TestOpenOverlappingAReconcileDropsTheRemovedFile(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	writeThroughHandle(t, f, held, "X")
	f.fs.testHookBeforeAddFileHandle = func() {
		f.fs.testHookBeforeAddFileHandle = nil
		commitFile(t, f, "Xlob-AAAA")
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	racing := f.open(held)
	f.switchTo(baseFile("file.txt", "blob-BBBB", 0o100644))
	if id := f.lookup(fuseops.RootInodeID, "file.txt"); id != held {
		t.Fatalf("file.txt looks up as inode %d after the HEAD change, want %d", id, held)
	}
	newer := f.open(held)
	for name, h := range map[string]fuseops.HandleID{"racing": racing.Handle, "newer": newer.Handle} {
		if got := mustReadHandle(t, f, held, h); got != "blob-BBBB" {
			t.Fatalf("after the HEAD change, the %s handle reads %q, want blob-BBBB", name, got)
		}
	}
}

// An open can read a file's base blob before a write through another handle
// gives the path an overlay file, and register its handle only after the
// write closed the files that handles held on the path. The handle must then
// read the write, not the base blob.
func TestOpenOverlappingAWriteReadsTheWrite(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	writer := f.openRW(held)
	f.fs.testHookBeforeAddFileHandle = func() {
		f.fs.testHookBeforeAddFileHandle = nil
		if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: writer.Handle, Data: []byte("Y")}); err != nil {
			t.Fatal(err)
		}
	}
	racing := f.open(held)
	if got := mustReadHandle(t, f, held, racing.Handle); got != "Ylob-AAAA" {
		t.Fatalf("the handle opened during the write reads %q, want Ylob-AAAA", got)
	}
}

// An unlink pins an open file's handle to its own copy and detaches it. No
// overlay entry names that copy, so the switch pass must leave a detached
// handle's file open: the handle still reads its writes after a HEAD change.
func TestSwitchPassKeepsTheCopyOfAnUnlinkedOpenFile(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.openRW(held)
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("X")}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Offset: 1, Data: []byte("Z")}); err != nil {
		t.Fatalf("write through the unlinked open file = %v", err)
	}
	f.switchTo(baseFile("file.txt", "blob-BBBB", 0o100644))
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if got := mustReadHandle(t, f, held, open.Handle); got != "XZob-AAAA" {
		t.Fatalf("after the HEAD change, the unlinked file's handle reads %q, want XZob-AAAA", got)
	}
}

// A backing file that an earlier version left read-only opens only for
// reading, so a handle opened for writing then holds it through a read-only
// descriptor. When an unlink pins that handle and one opened later, after the
// file became writable, the unlink opens the first one's copy for writing
// again, so an ftruncate through either reaches both.
func TestSetattrOfSharedCopyGoesThroughWritableDescriptor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a read-only file for writing")
	}
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	writeThroughHandle(t, f, held, "X")
	ov, ok, err := f.fs.engine.Overlay.Lookup(f.ctx, "file.txt")
	if err != nil || !ok {
		t.Fatalf("overlay entry of file.txt = %t, %v", ok, err)
	}
	if err := os.Chmod(ov.BackingPath, 0o444); err != nil {
		t.Fatal(err)
	}
	first := f.openRW(held)
	if err := os.Chmod(ov.BackingPath, 0o644); err != nil {
		t.Fatal(err)
	}
	second := f.openRW(held)
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	two := uint64(2)
	if err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Handle: &first.Handle, Size: &two}); err != nil {
		t.Fatalf("ftruncate through the first handle = %v", err)
	}
	for name, h := range map[string]fuseops.HandleID{"first": first.Handle, "second": second.Handle} {
		if got := mustReadHandle(t, f, held, h); got != "Xl" {
			t.Fatalf("after the ftruncate, the %s handle reads %q, want Xl", name, got)
		}
	}
}

// A backing file that an earlier version left read-only opens only for
// reading, so a handle opened for writing holds it through a read-only
// descriptor until a write through the overlay grants the owner write. An
// unlink with no write first grants it too, so the handle still truncates and
// writes its copy.
func TestUnlinkedCopyOfReadOnlyBackingFileStaysWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a read-only file for writing")
	}
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	writeThroughHandle(t, f, held, "X")
	ov, ok, err := f.fs.engine.Overlay.Lookup(f.ctx, "file.txt")
	if err != nil || !ok {
		t.Fatalf("overlay entry of file.txt = %t, %v", ok, err)
	}
	if err := os.Chmod(ov.BackingPath, 0o444); err != nil {
		t.Fatal(err)
	}
	open := f.openRW(held)
	if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	two := uint64(2)
	if err := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Handle: &open.Handle, Size: &two}); err != nil {
		t.Fatalf("ftruncate through the handle = %v", err)
	}
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Offset: 2, Data: []byte("zz")}); err != nil {
		t.Fatalf("write through the handle = %v", err)
	}
	if got := mustReadHandle(t, f, held, open.Handle); got != "Xlzz" {
		t.Fatalf("the handle reads %q, want Xlzz", got)
	}
}

// A rename with no handle open on its paths pins nothing, so it needs no
// backing file: a rename of an overlay file whose backing file is gone still
// moves the entry.
func TestRenameWithNoOpenHandleNeedsNoBackingFile(t *testing.T) {
	f := newCacheFixture(t)
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "a.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("data")}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	ov, ok, err := f.fs.engine.Overlay.Lookup(f.ctx, "a.txt")
	if err != nil || !ok {
		t.Fatalf("overlay entry of a.txt = %t, %v", ok, err)
	}
	if err := os.Remove(ov.BackingPath); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "a.txt", NewParent: fuseops.RootInodeID, NewName: "b.txt"}); err != nil {
		t.Fatalf("rename of a.txt, whose backing file is gone, = %v", err)
	}
	if got := f.lookup(fuseops.RootInodeID, "b.txt"); got != create.Entry.Child {
		t.Fatalf("b.txt looks up as inode %d, want %d, the renamed file", got, create.Entry.Child)
	}
}

// writeThroughHandle writes data at offset 0 of inode through a handle it
// opens for writing and releases, which gives the path an overlay file.
func writeThroughHandle(t *testing.T, f *cacheFixture, inode fuseops.InodeID, data string) {
	t.Helper()
	writer := &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_RDWR}
	if err := f.fs.OpenFile(f.ctx, writer); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: inode, Handle: writer.Handle, Data: []byte(data)}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: writer.Handle}); err != nil {
		t.Fatal(err)
	}
}

// A retired inode whose handles hold no copy of their own, such as one whose
// path came back changed, keeps what its handles read: a setattr of it
// returns ESTALE and changes neither its content nor its attributes.
func TestSetattrOfRetiredInodeWithoutOwnCopyReturnsESTALE(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-AAAA", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	open := f.openRW(held)
	mustReadHandle(t, f, held, open.Handle)
	for _, nodes := range [][]model.BaseNode{nil, {baseFile("file.txt", "blob-BBBBBBBB", 0o100644)}} {
		f.switchTo(nodes...)
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	size := uint64(2)
	mode := os.FileMode(0o600)
	for name, op := range map[string]*fuseops.SetInodeAttributesOp{
		"ftruncate": {Inode: held, Handle: &open.Handle, Size: &size},
		"fchmod":    {Inode: held, Mode: &mode},
	} {
		if got := kernelErrno(f.fs.SetInodeAttributes(f.ctx, op)); got != syscall.ESTALE {
			t.Errorf("%s of the retired inode = errno %v, want ESTALE", name, got)
		}
	}
	get := &fuseops.GetInodeAttributesOp{Inode: held}
	if err := f.fs.GetInodeAttributes(f.ctx, get); err != nil || get.Attributes.Mode.Perm() != 0o644 {
		t.Fatalf("getattr of the retired inode = mode %v, %v; want 0644", get.Attributes.Mode.Perm(), err)
	}
	if got := mustReadHandle(t, f, held, open.Handle); got != "blob-AAAA" {
		t.Fatalf("the handle reads %q, want blob-AAAA", got)
	}
}

// An unlink of a path, or a rename over it, can arrive with no lookup first
// after a switch brought the path back as another file and before that
// switch's pass. The held inode is retired first, so its handles keep the
// content they held and are not pinned to a copy of the other file.
func TestUnlinkOrRenameOverChangedReturnedPathKeepsHandleContent(t *testing.T) {
	for _, op := range []string{"unlink", "rename over"} {
		t.Run(op, func(t *testing.T) {
			f := newCacheFixtureOn(t, BackendFSKit, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			if op == "rename over" {
				create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "other.txt", Mode: 0o644}
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
			f.switchTo(baseFile("file.txt", "blob-BBBB", 0o100644))
			var err error
			if op == "unlink" {
				err = f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"})
			} else {
				err = f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "other.txt", NewParent: fuseops.RootInodeID, NewName: "file.txt"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := readHandle(f, held, open.Handle); err != nil || got != "blob-A" {
				t.Fatalf("read through the held handle after the %s = %q, %v; want blob-A, which it held", op, got, err)
			}
			if got := kernelErrno(f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("X")})); got != syscall.ESTALE {
				t.Fatalf("write through the held handle after the %s = errno %v, want ESTALE", op, got)
			}
		})
	}
}

// getattr of a held inode reports the size of what its handle reads, even
// when the handle cached an older generation than the last attributes the
// kernel received. When handles read different content, the most recently
// opened one decides, every time.
func TestGetattrSizeMatchesWhatTheHandleReads(t *testing.T) {
	getattrSize := func(t *testing.T, f *cacheFixture, id fuseops.InodeID) uint64 {
		t.Helper()
		op := &fuseops.GetInodeAttributesOp{Inode: id}
		if err := f.fs.GetInodeAttributes(f.ctx, op); err != nil {
			t.Fatalf("getattr of inode %d = %v", id, err)
		}
		return op.Attributes.Size
	}
	switchAndPass := func(t *testing.T, f *cacheFixture, nodes ...model.BaseNode) {
		t.Helper()
		f.switchTo(nodes...)
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("own copy written after unlink", func(t *testing.T) {
		f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
		held := f.lookup(fuseops.RootInodeID, "file.txt")
		open := f.open(held)
		before := &fuseops.GetInodeAttributesOp{Inode: held}
		if err := f.fs.GetInodeAttributes(f.ctx, before); err != nil {
			t.Fatal(err)
		}
		if err := f.fs.Unlink(f.ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "file.txt"}); err != nil {
			t.Fatal(err)
		}
		if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Offset: 6, Data: []byte("-longer")}); err != nil {
			t.Fatal(err)
		}
		if got, size := mustReadHandle(t, f, held, open.Handle), getattrSize(t, f, held); size != uint64(len(got)) {
			t.Fatalf("getattr size %d, the handle reads %q", size, got)
		}
		after := &fuseops.GetInodeAttributesOp{Inode: held}
		if err := f.fs.GetInodeAttributes(f.ctx, after); err != nil {
			t.Fatal(err)
		}
		if !after.Attributes.Mtime.After(before.Attributes.Mtime) {
			t.Fatalf("getattr mtime %v after a write to the own copy, want later than the base file's %v", after.Attributes.Mtime, before.Attributes.Mtime)
		}
	})
	t.Run("path came back changed", func(t *testing.T) {
		f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
		held := f.lookup(fuseops.RootInodeID, "file.txt")
		open := f.open(held)
		switchAndPass(t, f)
		switchAndPass(t, f, baseFile("file.txt", "blob-CCCCCCCCCC", 0o100644))
		if got, size := mustReadHandle(t, f, held, open.Handle), getattrSize(t, f, held); size != uint64(len(got)) {
			t.Fatalf("getattr size %d, the handle reads %q", size, got)
		}
	})
	t.Run("handle cached an older generation", func(t *testing.T) {
		f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
		held := f.lookup(fuseops.RootInodeID, "file.txt")
		open := f.open(held)
		mustReadHandle(t, f, held, open.Handle)
		switchAndPass(t, f, baseFile("file.txt", "blob-BBBBBBBBBBBB", 0o100644))
		f.getattr(held)
		switchAndPass(t, f)
		switchAndPass(t, f, baseFile("file.txt", "blob-C", 0o100644))
		if got, size := mustReadHandle(t, f, held, open.Handle), getattrSize(t, f, held); size != uint64(len(got)) {
			t.Fatalf("getattr size %d, the handle reads %q", size, got)
		}
	})
	t.Run("two handles read different blobs", func(t *testing.T) {
		f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
		held := f.lookup(fuseops.RootInodeID, "file.txt")
		older := f.open(held)
		mustReadHandle(t, f, held, older.Handle)
		switchAndPass(t, f, baseFile("file.txt", "blob-BBBBBBBB", 0o100644))
		f.getattr(held)
		newer := f.open(held)
		mustReadHandle(t, f, held, newer.Handle)
		switchAndPass(t, f, baseFile("file.txt", "blob-DDDDDDDDDDDDDDDD", 0o100644))
		f.getattr(held)
		switchAndPass(t, f)
		switchAndPass(t, f, baseFile("file.txt", "blob-C", 0o100644))
		want := mustReadHandle(t, f, held, newer.Handle)
		for range 20 {
			if size := getattrSize(t, f, held); size != uint64(len(want)) {
				t.Fatalf("getattr size %d, the newest handle reads %q", size, want)
			}
		}
	})
}

// A create handle holds no file. Once a commit drops the file's overlay entry
// and a HEAD change keeps its path away, the inode holds no content to answer
// a getattr from. It answers ESTALE, not ENOENT, which would make the macFUSE
// kernel extension revoke the vnode; a write through the handle still
// recreates the path.
func TestGetattrOfAwayInodeWithNoContentReturnsESTALE(t *testing.T) {
	other := baseFile("other.txt", "blob-O", 0o100644)
	f := newCacheFixture(t, other)
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	held := create.Entry.Child
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: create.Handle, Data: []byte("data\n")}); err != nil {
		t.Fatal(err)
	}
	for _, nodes := range [][]model.BaseNode{{other, baseFile("file.txt", blobOID("data\n"), 0o100644)}, {other}} {
		f.switchTo(nodes...)
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	if got := kernelErrno(f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: held})); got != syscall.ESTALE {
		t.Fatalf("getattr while file.txt is away = errno %v, want ESTALE", got)
	}
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: create.Handle, Offset: 5, Data: []byte("more\n")}); err != nil {
		t.Fatalf("write through the create handle = %v", err)
	}
	if got := f.lookup(fuseops.RootInodeID, "file.txt"); got != held {
		t.Fatalf("file.txt, which the write recreated, looks up as inode %d, want %d", got, held)
	}
}

// readHandle reads up to 64 bytes through handle h of inode.
func readHandle(f *cacheFixture, inode fuseops.InodeID, h fuseops.HandleID) (string, error) {
	read := &fuseops.ReadFileOp{Inode: inode, Handle: h, Size: 64, Dst: make([]byte, 64)}
	err := f.fs.ReadFile(f.ctx, read)
	return string(read.Dst[:read.BytesRead]), err
}

func mustReadHandle(t *testing.T, f *cacheFixture, inode fuseops.InodeID, h fuseops.HandleID) string {
	t.Helper()
	got, err := readHandle(f, inode, h)
	if err != nil {
		t.Fatalf("read through handle %d of inode %d: %v", h, inode, err)
	}
	return got
}

// openErrno opens id and returns the errno the kernel receives, releasing the
// handle an open made. A retired inode gets ESTALE.
func openErrno(f *cacheFixture, id fuseops.InodeID) syscall.Errno {
	op := &fuseops.OpenFileOp{Inode: id}
	err := f.fs.OpenFile(f.ctx, op)
	if err == nil {
		_ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: op.Handle})
	}
	return kernelErrno(err)
}
