//go:build !windows

package fusefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

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
		}},
		{"path returns changed", func(f *cacheFixture) string {
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			if err := f.mount.InvalidateChanged(); err != nil {
				f.t.Fatal(err)
			}
			return "blob-B"
		}},
		{"lookup before the pass", func(f *cacheFixture) string {
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			f.lookup(fuseops.RootInodeID, "file.txt")
			return "blob-B"
		}},
		{"write before the pass", func(f *cacheFixture) string {
			f.switchTo(baseFile("file.txt", "blob-B", 0o100644))
			return "blob-B"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
			held := f.lookup(fuseops.RootInodeID, "file.txt")
			open := f.open(held)
			f.switchTo()
			if err := f.mount.InvalidateChanged(); err != nil {
				t.Fatal(err)
			}
			want := tt.lose(f)
			if got := kernelErrno(f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: held, Handle: open.Handle, Data: []byte("old")})); got != syscall.ESTALE {
				t.Fatalf("write through the stale inode = errno %v, want ESTALE", got)
			}
			if got := kernelErrno(f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: held})); got != syscall.ESTALE {
				t.Fatalf("getattr of the stale inode = errno %v, want ESTALE", got)
			}
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
	if errno := kernelErrno(f.fs.GetInodeAttributes(f.ctx, &fuseops.GetInodeAttributesOp{Inode: held})); errno != syscall.ESTALE {
		t.Fatalf("getattr of the unlinked inode = errno %v, want ESTALE", errno)
	}
}

// An open of an unmapped inode can race a create that takes its path and
// retires it. The open must not hand out a handle on the created file. The
// test runs the create inside the open whenever the lock the open holds lets
// it.
func TestOpenRacingACreateGetsNoHandleOnTheCreatedFile(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-A", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	ov := hookOverlay(f)
	created := createInside(f, &ov.beforeLookup)
	open := &fuseops.OpenFileOp{Inode: held}
	err := f.fs.OpenFile(f.ctx, open)
	if err == nil {
		_ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	}
	if *created == 0 {
		t.Fatal("the create did not run inside the open")
	}
	if got := kernelErrno(err); got != syscall.ESTALE {
		t.Fatalf("open of inode %d, which a create retired during the open, = errno %v, want ESTALE", held, got)
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
	created := createInside(f, &ov.beforeLookup)
	size := uint64(2)
	setErr := f.fs.SetInodeAttributes(f.ctx, &fuseops.SetInodeAttributesOp{Inode: held, Handle: &open.Handle, Size: &size})
	if *created != 0 {
		if got := readWholeFile(t, f.fs, *created); got != "mine" {
			t.Fatalf("a create landed inside the ftruncate through inode %d (%v), and the file it made holds %q, want mine", held, setErr, got)
		}
	}
}

// createInside sets hook to create file.txt holding "mine" whenever the lock
// the running op holds lets another op run, and returns where the created
// inode lands.
func createInside(f *cacheFixture, hook *func()) *fuseops.InodeID {
	var created fuseops.InodeID
	*hook = func() {
		if !f.fs.handleOps.TryRLock() {
			return
		}
		f.fs.handleOps.RUnlock()
		create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
		if err := f.fs.CreateFile(f.ctx, create); err != nil {
			f.t.Errorf("CreateFile: %v", err)
			return
		}
		if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: []byte("mine")}); err != nil {
			f.t.Error(err)
		}
		_ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
		created = create.Entry.Child
	}
	return &created
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
// a listing, after a removal, before a file create, or before a lookup.
type hookedOverlay struct {
	model.OverlayStore
	afterRename, beforeList, afterList, afterRemove, beforeCreate, beforeLookup func()
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
	return o.OverlayStore.CreateFile(ctx, path, mode)
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

// waitRecheck returns once done is closed, or once a goroutine waits for the
// handleOps read lock in unmapStillMissing: the switch pass's re-check, which
// a Rename op holds off until it has moved its mappings.
func waitRecheck(done <-chan struct{}) {
	buf := make([]byte, 1<<20)
	for {
		select {
		case <-done:
			return
		default:
		}
		n := runtime.Stack(buf, true)
		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "[sync.RWMutex.RLock") && strings.Contains(g, "(*ArtifactFuse).unmapStillMissing") {
				return
			}
		}
		runtime.Gosched()
	}
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
}
