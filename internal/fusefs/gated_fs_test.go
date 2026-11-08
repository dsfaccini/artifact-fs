//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
)

type recordingFS struct {
	fuseutil.NotImplementedFileSystem
	lookups atomic.Int32
}

func (fs *recordingFS) LookUpInode(context.Context, *fuseops.LookUpInodeOp) error {
	fs.lookups.Add(1)
	return nil
}

func TestGatedFileSystemBlocksUntilReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		next := &recordingFS{}
		gate := NewReadyGate(false)
		fs := NewGatedFileSystem(next, gate)

		done := make(chan error, 1)
		go func() {
			done <- fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{})
		}()

		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("LookUpInode returned before ready: %v", err)
		default:
		}
		if got := next.lookups.Load(); got != 0 {
			t.Fatalf("lookups before ready = %d, want 0", got)
		}

		gate.MarkReady()
		if err := <-done; err != nil {
			t.Fatalf("LookUpInode after ready: %v", err)
		}
		if got := next.lookups.Load(); got != 1 {
			t.Fatalf("lookups after ready = %d, want 1", got)
		}
	})
}

// Behind a failed gate, each op that reads or changes the tree returns EIO
// without reaching next. The others pass through, and next, which implements
// none of them, answers ENOSYS.
func TestGatedFileSystemFailedGateReturnsEIO(t *testing.T) {
	gate := NewReadyGate(false)
	gate.MarkFailed(errors.New("clone failed"))
	fs := NewGatedFileSystem(&fuseutil.NotImplementedFileSystem{}, gate)
	ctx := context.Background()
	// Destroy returns nothing to compare.
	covered := map[string]bool{"Destroy": true}
	for _, tc := range []struct {
		name  string
		op    func() error
		gated bool
	}{
		{"StatFS", func() error { return fs.StatFS(ctx, &fuseops.StatFSOp{}) }, false},
		{"LookUpInode", func() error { return fs.LookUpInode(ctx, &fuseops.LookUpInodeOp{}) }, true},
		{"GetInodeAttributes of the root", func() error {
			return fs.GetInodeAttributes(ctx, &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID})
		}, false},
		{"GetInodeAttributes", func() error {
			return fs.GetInodeAttributes(ctx, &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID + 1})
		}, true},
		{"SetInodeAttributes", func() error { return fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{}) }, true},
		{"ForgetInode", func() error { return fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{}) }, false},
		{"BatchForget", func() error { return fs.BatchForget(ctx, &fuseops.BatchForgetOp{}) }, false},
		{"MkDir", func() error { return fs.MkDir(ctx, &fuseops.MkDirOp{}) }, true},
		{"MkNode", func() error { return fs.MkNode(ctx, &fuseops.MkNodeOp{}) }, true},
		{"CreateFile", func() error { return fs.CreateFile(ctx, &fuseops.CreateFileOp{}) }, true},
		{"CreateLink", func() error { return fs.CreateLink(ctx, &fuseops.CreateLinkOp{}) }, true},
		{"CreateSymlink", func() error { return fs.CreateSymlink(ctx, &fuseops.CreateSymlinkOp{}) }, true},
		{"Rename", func() error { return fs.Rename(ctx, &fuseops.RenameOp{}) }, true},
		{"RmDir", func() error { return fs.RmDir(ctx, &fuseops.RmDirOp{}) }, true},
		{"Unlink", func() error { return fs.Unlink(ctx, &fuseops.UnlinkOp{}) }, true},
		{"OpenDir", func() error { return fs.OpenDir(ctx, &fuseops.OpenDirOp{}) }, true},
		{"ReadDir", func() error { return fs.ReadDir(ctx, &fuseops.ReadDirOp{}) }, true},
		{"ReadDirPlus", func() error { return fs.ReadDirPlus(ctx, &fuseops.ReadDirPlusOp{}) }, true},
		{"ReleaseDirHandle", func() error { return fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{}) }, false},
		{"OpenFile", func() error { return fs.OpenFile(ctx, &fuseops.OpenFileOp{}) }, true},
		{"ReadFile", func() error { return fs.ReadFile(ctx, &fuseops.ReadFileOp{}) }, true},
		{"WriteFile", func() error { return fs.WriteFile(ctx, &fuseops.WriteFileOp{}) }, true},
		{"SyncFile", func() error { return fs.SyncFile(ctx, &fuseops.SyncFileOp{}) }, false},
		{"FlushFile", func() error { return fs.FlushFile(ctx, &fuseops.FlushFileOp{}) }, false},
		{"ReleaseFileHandle", func() error { return fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{}) }, false},
		{"ReadSymlink", func() error { return fs.ReadSymlink(ctx, &fuseops.ReadSymlinkOp{}) }, true},
		{"RemoveXattr", func() error { return fs.RemoveXattr(ctx, &fuseops.RemoveXattrOp{}) }, true},
		{"GetXattr", func() error { return fs.GetXattr(ctx, &fuseops.GetXattrOp{}) }, true},
		{"ListXattr", func() error { return fs.ListXattr(ctx, &fuseops.ListXattrOp{}) }, true},
		{"SetXattr", func() error { return fs.SetXattr(ctx, &fuseops.SetXattrOp{}) }, true},
		{"Fallocate", func() error { return fs.Fallocate(ctx, &fuseops.FallocateOp{}) }, true},
		{"SyncFS", func() error { return fs.SyncFS(ctx, &fuseops.SyncFSOp{}) }, true},
	} {
		covered[strings.Fields(tc.name)[0]] = true
		want := syscall.ENOSYS
		if tc.gated {
			want = syscall.EIO
		}
		if got := kernelErrno(tc.op()); got != want {
			t.Errorf("%s = %v, want %v", tc.name, got, want)
		}
	}
	for m := range reflect.TypeFor[fuseutil.FileSystem]().Methods() {
		if !covered[m.Name] {
			t.Errorf("no row for %s", m.Name)
		}
	}
}

func TestGatedFileSystemCanceledWaitReturnsEINTR(t *testing.T) {
	next := &recordingFS{}
	fs := NewGatedFileSystem(next, NewReadyGate(false))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fs.LookUpInode(ctx, &fuseops.LookUpInodeOp{})
	if kernelErrno(err) != syscall.EINTR {
		t.Fatalf("LookUpInode on a canceled wait = %v, want EINTR", err)
	}
	if got := next.lookups.Load(); got != 0 {
		t.Fatalf("lookups passed through = %d, want 0", got)
	}
}
