package fusefs

import (
	"os"

	"github.com/jacobsa/fuse"
)

func platformMountConfig(cfg *fuse.MountConfig) {
	cfg.FuseImpl = fuse.FUSEImplMacFUSE
	if os.Getenv("ARTIFACT_FS_FUSE_BACKEND") == "fskit" {
		if cfg.Options == nil {
			cfg.Options = make(map[string]string)
		}
		cfg.Options["backend"] = "fskit"
	}
}
