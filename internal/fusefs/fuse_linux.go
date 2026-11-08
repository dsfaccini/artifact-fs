package fusefs

import (
	"syscall"

	"github.com/dsfaccini/fuse"
)

// setXattrErrno answers every SetXattr: the overlay stores no extended attributes.
const setXattrErrno = syscall.ENOSYS

// negativeLookupEntries answers a lookup of a missing name with a negative
// entry, which the kernel caches for CacheTTL. MountedFS.InvalidateChanged
// drops it when the name appears outside the mount.
const negativeLookupEntries = true

// keepBasePageCache lets the kernel keep a base file's cached pages across
// opens. See ArtifactFuse.openedBlobLocked for when it does.
const keepBasePageCache = true

func platformMountConfig(cfg *fuse.MountConfig) {
	// Without it the kernel serializes lookups and readdirs in a directory.
	// Every adapter op takes its own locks.
	cfg.EnableParallelDirOps = true
	// With DisableWritebackCaching, the kernel then drops a file's cached
	// pages when new attributes carry a changed mtime, as well as a changed size.
	cfg.EnableAutoInvalData = true
}

// platformMount ignores backend: Linux has only the kernel transport.
func platformMount(_ string, mount func(fskit bool) (*fuse.MountedFileSystem, error)) (*fuse.MountedFileSystem, string, error) {
	mfs, err := mount(false)
	return mfs, BackendKernel, err
}
