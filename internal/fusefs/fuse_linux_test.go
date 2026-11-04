package fusefs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/fuseops"
)

func TestLookUpMissingNameReturnsNegativeEntry(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	ctx := context.Background()
	lookup := func(name string) fuseops.ChildInodeEntry {
		t.Helper()
		op := &fuseops.LookUpInodeOp{Parent: f.nestInode, Name: name}
		if err := f.fs.LookUpInode(ctx, op); err != nil {
			t.Fatalf("LookUpInode(%q): %v", name, err)
		}
		return op.Entry
	}
	names := []string{"file", "dir", "link", "renamed"}
	for _, name := range names {
		if entry := lookup(name); entry.Child != 0 || !entry.EntryExpiration.After(time.Now()) {
			t.Fatalf("lookup of missing %q = child %d expiring %v, want a negative entry the kernel caches", name, entry.Child, entry.EntryExpiration)
		}
	}

	// Each op that makes a name exist replies with the new entry, which
	// replaces the kernel's negative one.
	create := &fuseops.CreateFileOp{Parent: f.nestInode, Name: "file", Mode: 0o644}
	if err := f.fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	mkdir := &fuseops.MkDirOp{Parent: f.nestInode, Name: "dir", Mode: os.ModeDir | 0o755}
	if err := f.fs.MkDir(ctx, mkdir); err != nil {
		t.Fatal(err)
	}
	symlink := &fuseops.CreateSymlinkOp{Parent: f.nestInode, Name: "link", Target: "value.txt"}
	if err := f.fs.CreateSymlink(ctx, symlink); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Rename(ctx, &fuseops.RenameOp{OldParent: f.nestInode, OldName: "value.txt", NewParent: f.nestInode, NewName: "renamed"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fuseops.InodeID{"file": create.Entry.Child, "dir": mkdir.Entry.Child, "link": symlink.Entry.Child, "renamed": 0} {
		entry := lookup(name)
		if entry.Child == 0 || (want != 0 && entry.Child != want) {
			t.Fatalf("lookup of %q after it was made = child %d, want %d", name, entry.Child, want)
		}
	}
}
