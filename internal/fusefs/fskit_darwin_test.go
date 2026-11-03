//go:build fskit

package fusefs

import (
	"fmt"
	"testing"

	"github.com/jacobsa/fuse"
)

func TestPlatformMountWithFSKitTagFallsBackWhenFSKitIsUnavailable(t *testing.T) {
	unavailable := fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable)
	for _, test := range []struct {
		name        string
		kextLoaded  bool
		wantMounts  []bool
		wantBackend string
		wantErr     error
	}{
		{name: "kext loaded", kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel},
		{name: "kext not loaded", wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErr: fuse.ErrFSKitUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			setMacFUSEKextLoaded(t, test.kextLoaded)
			var mounts []bool
			_, backend, err := platformMount(BackendAuto, func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				if fskit {
					return nil, unavailable
				}
				return nil, nil
			})
			assertMountResult(t, mounts, backend, err, test.wantMounts, test.wantBackend, test.wantErr)
		})
	}
}
