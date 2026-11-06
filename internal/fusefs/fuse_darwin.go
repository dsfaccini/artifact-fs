package fusefs

import (
	"syscall"

	"github.com/dsfaccini/fuse"
)

// setXattrErrno answers every SetXattr. ENOSYS would make macOS store the
// attributes in AppleDouble ._ files beside each file, so darwin fails the
// write instead.
const setXattrErrno = syscall.EPERM

func platformMountConfig(cfg *fuse.MountConfig) {
	// noappledouble would make the kernel extension hide tracked ._ and
	// .DS_Store files.
	cfg.EnableAppleDouble = true
}

func platformMount(backend string, mount func(fskit bool) (*fuse.MountedFileSystem, error)) (*fuse.MountedFileSystem, string, error) {
	return mountWithBackend(backend, mount, macFUSEKernelExtensionLoaded)
}

// macFUSEKernelExtensionLoaded is fuse.MacFUSEKernelExtensionLoaded. Tests
// replace it to cover both states.
var macFUSEKernelExtensionLoaded = fuse.MacFUSEKernelExtensionLoaded
