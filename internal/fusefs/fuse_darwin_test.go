package fusefs

import (
	"context"
	"fmt"
	"syscall"
	"testing"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
)

func setMacFUSEKextLoaded(t *testing.T, loaded bool) {
	t.Helper()
	saved := macFUSEKextLoaded
	macFUSEKextLoaded = func() bool { return loaded }
	t.Cleanup(func() { macFUSEKextLoaded = saved })
}

func TestPlatformMountReachesFSKit(t *testing.T) {
	unavailable := fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable)
	for _, test := range []struct {
		name        string
		backend     string
		fskitErr    error
		kextLoaded  bool
		wantMounts  []bool
		wantBackend string
		wantErr     error
	}{
		{name: "fskit", backend: BackendFSKit, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "auto", backend: BackendAuto, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "kernel", backend: BackendKernel, kextLoaded: true, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{name: "auto falls back to a loaded kext", backend: BackendAuto, fskitErr: unavailable, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel},
		{name: "auto keeps an unloaded kext unloaded", backend: BackendAuto, fskitErr: unavailable, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErr: fuse.ErrFSKitUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			setMacFUSEKextLoaded(t, test.kextLoaded)
			var mounts []bool
			_, backend, err := platformMount(test.backend, func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				if fskit {
					return nil, test.fskitErr
				}
				return nil, nil
			})
			assertMountResult(t, mounts, backend, err, test.wantMounts, test.wantBackend, test.wantErr)
		})
	}
}

// Whether FSKit honors negative entries is unverified, so darwin answers a
// missing name with ENOENT on every backend.
func TestLookUpMissingNameReturnsENOENT(t *testing.T) {
	f := newLocalOverlayFuseFixture(t)
	op := &fuseops.LookUpInodeOp{Parent: f.nestInode, Name: "missing"}
	if got := kernelErrno(f.fs.LookUpInode(context.Background(), op)); got != syscall.ENOENT {
		t.Fatalf("lookup of a missing name = %v, want ENOENT", got)
	}
}
