package fusefs

import (
	"github.com/dsfaccini/fuse"
	"golang.org/x/sys/unix"
)

func platformMountConfig(cfg *fuse.MountConfig) {
	cfg.FuseImpl = fuse.FUSEImplMacFUSE
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
