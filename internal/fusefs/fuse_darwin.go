package fusefs

import (
	"syscall"

	"github.com/dsfaccini/fuse"
)

// setXattrErrno answers every SetXattr. ENOSYS would make macOS store the
// attributes in AppleDouble ._ files beside each file, so darwin fails the
// write instead.
const setXattrErrno = syscall.EPERM

// negativeLookupEntries is off: the kernel extension mounts with novncache,
// which disables its name cache, and FSKit takes no invalidations to drop a
// negative entry when the name appears.
const negativeLookupEntries = false

// keepBasePageCache is off, so no open asks to keep a file's cached pages.
// The kernel extension ignores the flag either way. It drops a file's pages on
// an open only for direct I/O, which the mount never asks for, or for
// FOPEN_PURGE_UBC, which the fuse fork never sets. Otherwise it drops them on
// InvalidateInode, and drops the pages past the end on a smaller size. macFUSE 5's
// extension is closed source; this follows the open-source osxfuse extension:
// https://github.com/osxfuse/kext/blob/534773bd3135f04b7f6fb801faa2f8991daf4a6b/osxfuse/fuse_vnops.c#L2356-L2390
const keepBasePageCache = false

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
