package fusefs

import (
	"github.com/jacobsa/fuse"
)

func platformMountConfig(cfg *fuse.MountConfig) {
	// FUSEImplFuseT is the zero value and the correct choice for Linux.
	cfg.FuseImpl = fuse.FUSEImplFuseT
}

// platformMount ignores backend: Linux has only the kernel transport.
func platformMount(_ string, mount func(fskit bool) (*fuse.MountedFileSystem, error)) (*fuse.MountedFileSystem, string, error) {
	mfs, err := mount(false)
	return mfs, BackendKernel, err
}
