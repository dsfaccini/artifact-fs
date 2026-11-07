//go:build !windows

package fusefs

import (
	"context"
	"os"
	"slices"
	"testing"

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
	f := newCacheFixture(t, baseFile("file.txt", "old", 0o100644))
	held := f.lookup(fuseops.RootInodeID, "file.txt")
	f.switchTo()
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "file.txt", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
	})
	if create.Entry.Child == held {
		t.Fatalf("created file took inode %d of the file the switch removed", held)
	}
}
