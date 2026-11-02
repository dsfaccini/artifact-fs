//go:build fskit

package fusefs

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jacobsa/fuse"
)

func TestMountWithBackendFallsBackOnlyOnMachineWideFSKitFailures(t *testing.T) {
	result := func(code int32) error {
		return fmt.Errorf("mount (background): %w", fuse.FSKitMountError{Result: code})
	}
	for name, test := range map[string]struct {
		fskitErr     error
		wantFallback bool
	}{
		"framework unavailable":       {fskitErr: fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable), wantFallback: true},
		"unsupported OS version":      {fskitErr: result(fuse.FSKitMountResultUnsupportedOSVersion), wantFallback: true},
		"helper tools install failed": {fskitErr: result(fuse.FSKitMountResultHelperToolsInstallationFailed), wantFallback: true},
		"extension not found":         {fskitErr: result(fuse.FSKitMountResultFileSystemExtensionNotFound), wantFallback: true},
		"extension requires approval": {fskitErr: result(fuse.FSKitMountResultFileSystemExtensionRequiresApproval), wantFallback: true},
		"mount point failure":         {fskitErr: result(fuse.FSKitMountResultCreatingMountPointFailed)},
		"unexpected failure":          {fskitErr: result(fuse.FSKitMountResultUnexpectedFailure)},
		"other failure":               {fskitErr: errors.New("FSKit transport failed")},
	} {
		t.Run(name, func(t *testing.T) {
			var mounts []bool
			mount := func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				if fskit {
					return nil, test.fskitErr
				}
				return nil, nil
			}
			_, backend, err := mountWithBackend(BackendAuto, mount, fskitUnavailable, func() bool { return true })
			if test.wantFallback {
				assertMountResult(t, mounts, backend, err, []bool{true, false}, BackendKernel, nil)
			} else {
				assertMountResult(t, mounts, backend, err, []bool{true}, BackendFSKit, test.fskitErr)
			}
		})
	}
}
