//go:build !windows

package fusefs

import (
	"context"
	"errors"
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

func TestGatedFileSystemFailedGateReturnsEIO(t *testing.T) {
	next := &recordingFS{}
	gate := NewReadyGate(false)
	gate.MarkFailed(errors.New("clone failed"))
	fs := NewGatedFileSystem(next, gate)

	err := fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{})
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("LookUpInode error = %v, want EIO", err)
	}
	if got := next.lookups.Load(); got != 0 {
		t.Fatalf("lookups after failed gate = %d, want 0", got)
	}
}
