package fusefs

import (
	"syscall"

	"github.com/dsfaccini/fuse"
	"golang.org/x/sys/unix"
)

// setXattrErrno answers every SetXattr. ENOSYS would make macOS store the
// attributes in AppleDouble ._ files beside each file, so darwin fails the
// write instead.
const setXattrErrno = syscall.EPERM

func platformMountConfig(cfg *fuse.MountConfig) {
	cfg.FuseImpl = fuse.FUSEImplMacFUSE
	// noappledouble would make the kernel extension hide tracked ._ and
	// .DS_Store files.
	cfg.EnableAppleDouble = true
}

func platformMount(backend string, mount func(fskit bool) (*fuse.MountedFileSystem, error)) (*fuse.MountedFileSystem, string, error) {
	return mountWithBackend(backend, mount, fskitUnavailable, macFUSEKextLoaded)
}

// macFUSEKextLoaded reports whether the macFUSE kernel extension is loaded; the
// extension registers this sysctl. Tests replace it to cover both states.
var macFUSEKextLoaded = func() bool {
	_, err := unix.SysctlRaw("vfs.generic.macfuse.version.number")
	return err == nil
}
