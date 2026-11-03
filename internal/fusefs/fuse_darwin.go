package fusefs

import (
	"github.com/jacobsa/fuse"
	"golang.org/x/sys/unix"
)

func platformMountConfig(cfg *fuse.MountConfig) {
	cfg.FuseImpl = fuse.FUSEImplMacFUSE
}

// macFUSEKextLoaded reports whether the macFUSE kernel extension is loaded; the
// extension registers this sysctl. Tests replace it to cover both states.
var macFUSEKextLoaded = func() bool {
	_, err := unix.SysctlRaw("vfs.generic.macfuse.version.number")
	return err == nil
}
