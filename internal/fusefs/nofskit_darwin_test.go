//go:build !fskit

package fusefs

import (
	"errors"
	"slices"
	"testing"

	"github.com/jacobsa/fuse"
)

func TestPlatformMountWithoutFSKitTagMountsOnlyThroughKernel(t *testing.T) {
	kextLoaded := macFUSEKextLoaded()
	for _, test := range []struct {
		backend     string
		wantMounts  []bool
		wantBackend string
		wantErr     error
	}{
		{backend: BackendKernel, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{backend: BackendFSKit, wantBackend: BackendFSKit, wantErr: errFSKitNotBuilt},
		{backend: BackendAuto, wantMounts: []bool{false}, wantBackend: BackendKernel},
	} {
		if test.backend == BackendAuto && !kextLoaded {
			test.wantMounts, test.wantBackend, test.wantErr = nil, BackendFSKit, errFSKitNotBuilt
		}
		t.Run(test.backend, func(t *testing.T) {
			var mounts []bool
			_, backend, err := platformMount(test.backend, func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				return nil, nil
			})
			if !slices.Equal(mounts, test.wantMounts) {
				t.Fatalf("mount attempts (true is FSKit) = %v, want %v", mounts, test.wantMounts)
			}
			if backend != test.wantBackend {
				t.Fatalf("backend = %q, want %q", backend, test.wantBackend)
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}
