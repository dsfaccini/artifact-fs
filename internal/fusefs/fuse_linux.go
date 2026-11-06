package fusefs

import (
	"syscall"

	"github.com/dsfaccini/fuse"
)

// setXattrErrno answers every SetXattr: the overlay stores no extended attributes.
const setXattrErrno = syscall.ENOSYS

func platformMountConfig(cfg *fuse.MountConfig) {
	// FUSEImplFuseT is the zero value and the correct choice for Linux.
	cfg.FuseImpl = fuse.FUSEImplFuseT
	// Without it the kernel serializes lookups and readdirs in a directory.
	// Every adapter op takes its own locks.
	cfg.EnableParallelDirOps = true
}

// platformMount ignores backend: Linux has only the kernel transport.
func platformMount(_ string, mount func(fskit bool) (*fuse.MountedFileSystem, error)) (*fuse.MountedFileSystem, string, error) {
	mfs, err := mount(false)
	return mfs, BackendKernel, err
}
