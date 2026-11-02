//go:build fskit

package fusefs

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jacobsa/fuse"
)

func TestFSKitUnavailableMatchesOnlyMachineWideFailures(t *testing.T) {
	result := func(code int32) error {
		return fmt.Errorf("mount (background): %w", fuse.FSKitMountError{Result: code})
	}
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"framework unavailable":       {err: fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable), want: true},
		"unsupported OS version":      {err: result(fuse.FSKitMountResultUnsupportedOSVersion), want: true},
		"helper tools install failed": {err: result(fuse.FSKitMountResultHelperToolsInstallationFailed), want: true},
		"extension not found":         {err: result(fuse.FSKitMountResultFileSystemExtensionNotFound), want: true},
		"extension requires approval": {err: result(fuse.FSKitMountResultFileSystemExtensionRequiresApproval), want: true},
		"mount point failure":         {err: result(fuse.FSKitMountResultCreatingMountPointFailed)},
		"unexpected failure":          {err: result(fuse.FSKitMountResultUnexpectedFailure)},
		"other failure":               {err: errors.New("FSKit transport failed")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := fskitUnavailable(test.err); got != test.want {
				t.Fatalf("fskitUnavailable(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
