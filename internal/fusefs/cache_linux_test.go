package fusefs

import (
	"os"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/fuseops"
)

// A lookup of a missing name answers a negative entry, which the kernel
// caches. Each op that makes a name through the mount replies with the new
// entry, which replaces the negative one in the kernel, and a lookup finds it.
func TestNegativeEntryGivesWayToNamesMadeThroughTheMount(t *testing.T) {
	f := newCacheFixture(t, baseFile("value.txt", "value", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	for _, name := range []string{"file", "dir", "link", "renamed"} {
		op := &fuseops.LookUpInodeOp{Parent: root, Name: name}
		if err := f.fs.LookUpInode(f.ctx, op); err != nil {
			t.Fatalf("lookup of missing %q: %v, want a negative entry", name, err)
		}
		if op.Entry.Child != 0 || !op.Entry.EntryExpiration.After(time.Now()) {
			t.Fatalf("lookup of missing %q = inode %d expiring %v, want a negative entry the kernel caches", name, op.Entry.Child, op.Entry.EntryExpiration)
		}
	}

	create := &fuseops.CreateFileOp{Parent: root, Name: "file", Mode: 0o644}
	if err := f.fs.CreateFile(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	mkdir := &fuseops.MkDirOp{Parent: root, Name: "dir", Mode: os.ModeDir | 0o755}
	if err := f.fs.MkDir(f.ctx, mkdir); err != nil {
		t.Fatal(err)
	}
	symlink := &fuseops.CreateSymlinkOp{Parent: root, Name: "link", Target: "value.txt"}
	if err := f.fs.CreateSymlink(f.ctx, symlink); err != nil {
		t.Fatal(err)
	}
	value := f.lookup(root, "value.txt")
	if err := f.fs.Rename(f.ctx, &fuseops.RenameOp{OldParent: root, OldName: "value.txt", NewParent: root, NewName: "renamed"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fuseops.InodeID{"file": create.Entry.Child, "dir": mkdir.Entry.Child, "link": symlink.Entry.Child, "renamed": value} {
		if want == 0 {
			t.Fatalf("the op that made %q replied with no entry", name)
		}
		if got := f.lookup(root, name); got != want {
			t.Fatalf("lookup of %q after it was made = inode %d, want %d", name, got, want)
		}
	}
}

// A write through a handle recreates a file that vanished in a switch. No
// reply tells the kernel, so the mount invalidates the negative entry a lookup
// left for the name.
func TestWriteRecreatingVanishedFileInvalidatesItsNegativeEntry(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "old", 0o100644))
	var root fuseops.InodeID = fuseops.RootInodeID
	inode := f.lookup(root, "file.txt")
	open := f.open(inode)
	f.switchTo()
	if child := f.lookup(root, "file.txt"); child != 0 {
		t.Fatalf("lookup after the switch removed the file = inode %d", child)
	}

	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: inode, Handle: open.Handle, Data: []byte("new")}); err != nil {
		t.Fatal(err)
	}
	<-f.notifier.sent
	if _, entries := f.notifier.recorded(); len(entries) != 1 || entries[0] != (kernelEntry{root, "file.txt"}) {
		t.Fatalf("invalidated entries = %v, want the negative entry of file.txt", entries)
	}
	if child := f.lookup(root, "file.txt"); child != inode {
		t.Fatalf("lookup after the write = inode %d, want %d", child, inode)
	}
}

// The kernel keeps a base file's cached pages across an open only when the
// inode's previous open served the same blob, and no report since changed the
// blob its reads serve.
func TestOpenKeepsPageCacheOnlyForTheBlobItLastServed(t *testing.T) {
	f := newCacheFixture(t, baseFile("file.txt", "blob-a", 0o100644))
	inode := f.lookup(fuseops.RootInodeID, "file.txt")
	keeps := func() bool {
		t.Helper()
		return f.open(inode).KeepPageCache
	}
	if keeps() {
		t.Fatal("first open kept the page cache")
	}
	if !keeps() {
		t.Fatal("second open of the same blob dropped the page cache")
	}

	// Same size and no commit time: only the blob tells the two apart.
	f.switchTo(baseFile("file.txt", "blob-b", 0o100644))
	if !keeps() {
		t.Fatal("open that still serves the old blob dropped the page cache")
	}
	f.getattr(inode)
	// Reads through the open handles now serve blob-b, so after a switch back
	// the pages may hold either blob.
	f.switchTo(baseFile("file.txt", "blob-a", 0o100644))
	f.getattr(inode)
	if keeps() {
		t.Fatal("open after the blob changed twice kept the page cache")
	}
	if !keeps() {
		t.Fatal("second open of the same blob dropped the page cache")
	}

	write := f.open(inode)
	if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: inode, Handle: write.Handle, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if keeps() {
		t.Fatal("open of the overlay file kept the page cache")
	}
}
