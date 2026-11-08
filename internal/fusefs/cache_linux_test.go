package fusefs

import (
	"fmt"
	"testing"

	"github.com/dsfaccini/fuse/fuseops"
)

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

// A write through a handle fills the kernel's pages with the written bytes.
// After a commit of them and a switch back to the blob the handle's open
// served, at the same size and with no reply in between, the next open must
// not keep those pages. A write to another path that overlaps that open's
// first attempt makes it open again, and must not let the second attempt
// keep them.
func TestOpenAfterAWriteDropsThePageCache(t *testing.T) {
	for _, overlapped := range []bool{false, true} {
		t.Run(fmt.Sprint("overlapped=", overlapped), func(t *testing.T) {
			other := baseFile("other.txt", "blob-other", 0o100644)
			f := newCacheFixture(t, baseFile("file.txt", "blob-a", 0o100644), other)
			inode := f.lookup(fuseops.RootInodeID, "file.txt")
			if err := f.fs.WriteFile(f.ctx, &fuseops.WriteFileOp{Inode: inode, Handle: f.open(inode).Handle, Data: []byte("xxxxxx")}); err != nil {
				t.Fatal(err)
			}
			f.switchTo(baseFile("file.txt", blobOID("xxxxxx"), 0o100644), other)
			f.switchTo(baseFile("file.txt", "blob-a", 0o100644), other)
			if overlapped {
				writeOtherDuringOpens(t, f, f.lookup(fuseops.RootInodeID, "other.txt"), 1)
			}
			if f.open(inode).KeepPageCache {
				t.Fatal("open after the write kept pages that hold the written bytes, not blob-a")
			}
		})
	}
}
