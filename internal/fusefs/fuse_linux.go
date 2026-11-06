package fusefs

import (
	"syscall"

	"github.com/dsfaccini/fuse"
)

// negativeLookupEntries answers a lookup of a missing name with a negative
// entry. Creating the name through the mount replaces the kernel's negative
// dentry, so the cache hides only names that appear outside the mount, such
// as after a HEAD change, and only for the entry TTL.
const negativeLookupEntries = true

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
