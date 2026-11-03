//go:build fskit

package fusefs

import (
	"errors"

	"github.com/jacobsa/fuse"
)

// fskitUnavailable reports whether err means FSKit cannot serve any mount on this
// machine. Other FSKit failures may leave a mount behind, so retrying through the
// kernel transport could mount twice.
func fskitUnavailable(err error) bool {
	if errors.Is(err, fuse.ErrFSKitUnavailable) {
		return true
	}
	var mountErr fuse.FSKitMountError
	if !errors.As(err, &mountErr) {
		return false
	}
	switch mountErr.Result {
	case fuse.FSKitMountResultUnsupportedOSVersion,
		fuse.FSKitMountResultHelperToolsInstallationFailed,
		fuse.FSKitMountResultFileSystemExtensionNotFound,
		fuse.FSKitMountResultFileSystemExtensionRequiresApproval:
		return true
	}
	return false
}
