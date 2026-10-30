//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse/fuseops"
)

func TestReadDirRefreshesReusedFSKitHandle(t *testing.T) {
	for _, test := range []struct {
		name  string
		fskit bool
	}{
		{name: "ordinary handle"},
		{name: "FSKit reused handle", fskit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLocalOverlayFuseFixture(t)
			f.fs.fskit = test.fskit
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
			if !bytes.Contains(read(0), []byte("value.txt\x00")) {
				t.Fatal("initial enumeration omitted the existing file")
			}
			if _, err := f.store.CreateFile(ctx, "src/nested/added.txt", 0o644); err != nil {
				t.Fatal(err)
			}
			if got := read(1); len(got) != 0 {
				t.Fatal("a continued enumeration changed its snapshot")
			}
			if got := bytes.Contains(read(0), []byte("added.txt\x00")); got != test.fskit {
				t.Fatalf("rewound enumeration includes new file=%v, want %v", got, test.fskit)
			}
			if err := f.store.Remove(ctx, "src/nested/value.txt"); err != nil {
				t.Fatal(err)
			}
			if got := bytes.Contains(read(0), []byte("value.txt\x00")); got == test.fskit {
				t.Fatalf("rewound enumeration includes deleted file=%v, want %v", got, !test.fskit)
			}
			if err := f.fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: open.Handle}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFSKitRejectsXattrFallback(t *testing.T) {
	for _, test := range []struct {
		name  string
		fskit bool
		want  error
	}{
		{name: "ordinary backend", want: syscall.ENOSYS},
		{name: "FSKit", fskit: true, want: syscall.EPERM},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := &ArtifactFuse{fskit: test.fskit}
			if err := fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Name: "com.apple.FinderInfo"}); err != test.want {
				t.Fatalf("SetXattr error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestFSKitGitFileAttributesRemainStable(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	f.fs.fskit = true
	ctx := context.Background()
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: ".git"}
	if err := f.fs.LookUpInode(ctx, lookup); err != nil {
		t.Fatal(err)
	}
	if lookup.Entry.Attributes.Mtime.IsZero() {
		t.Fatal("synthetic Git file has no modification time")
	}
	getattr := &fuseops.GetInodeAttributesOp{Inode: lookup.Entry.Child}
	if err := f.fs.GetInodeAttributes(ctx, getattr); err != nil {
		t.Fatal(err)
	}
	if getattr.Attributes != lookup.Entry.Attributes {
		t.Fatal("unchanged synthetic Git file attributes changed between lookup and stat")
	}
}
