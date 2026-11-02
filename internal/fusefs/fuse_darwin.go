package fusefs

import (
	"github.com/jacobsa/fuse"
)

func platformMountConfig(cfg *fuse.MountConfig, backend string) {
	cfg.FuseImpl = fuse.FUSEImplMacFUSE
	if backend == "fskit" {
		if cfg.Options == nil {
			cfg.Options = make(map[string]string)
		}
		cfg.Options["backend"] = "fskit"
	}
}
