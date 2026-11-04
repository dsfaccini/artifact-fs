package fusefs

import (
	"context"
	"log/slog"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse/fuseops"
)

// Under noappledouble the kernel extension hides tracked ._ and .DS_Store
// files, which FSKit serves.
func TestMountRepoKeepsAppleDoubleFilesVisible(t *testing.T) {
	setMacFUSEKextLoaded(t, true)
	repo := model.RepoConfig{Name: "repo", MountPath: t.TempDir()}
	var kernel bool
	for _, cfg := range mountRepoConfigs(t, repo, slog.New(slog.DiscardHandler)) {
		if cfg.Options["backend"] != "fskit" {
			kernel = true
		}
		if !cfg.EnableAppleDouble {
			t.Fatalf("mount config with options %v keeps noappledouble", cfg.Options)
		}
	}
	if !kernel {
		t.Fatal("MountRepoWithGate never tried the kernel extension")
	}
}

// ENOSYS would make macOS store each extended attribute in an AppleDouble ._
// file, on either backend.
func TestSetXattrRefusesAppleDoubleFallback(t *testing.T) {
	for _, fskit := range []bool{false, true} {
		fs := &ArtifactFuse{fskit: fskit}
		if got := kernelErrno(fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Name: "com.apple.FinderInfo"})); got != syscall.EPERM {
			t.Fatalf("SetXattr with fskit=%v = %v, want EPERM", fskit, got)
		}
	}
}
