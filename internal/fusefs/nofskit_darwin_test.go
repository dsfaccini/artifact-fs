//go:build !fskit

package fusefs

import (
	"fmt"
	"testing"

	"github.com/jacobsa/fuse"
)

func TestPlatformMountWithoutFSKitTagMountsOnlyThroughKernel(t *testing.T) {
	for _, test := range []struct {
		backend     string
		kextLoaded  bool
		wantMounts  []bool
		wantBackend string
		wantErr     error
	}{
		{backend: BackendKernel, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{backend: BackendFSKit, kextLoaded: true, wantBackend: BackendFSKit, wantErr: errFSKitNotBuilt},
		{backend: BackendAuto, kextLoaded: true, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{backend: BackendAuto, wantBackend: BackendFSKit, wantErr: errFSKitNotBuilt},
	} {
		t.Run(fmt.Sprintf("%s kext loaded %v", test.backend, test.kextLoaded), func(t *testing.T) {
			setMacFUSEKextLoaded(t, test.kextLoaded)
			var mounts []bool
			_, backend, err := platformMount(test.backend, func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				return nil, nil
			})
			assertMountResult(t, mounts, backend, err, test.wantMounts, test.wantBackend, test.wantErr)
		})
	}
}
