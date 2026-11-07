package fusefs

import (
	"context"
	"log/slog"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
)

// Under noappledouble the kernel extension hides tracked ._ and .DS_Store
// files, which FSKit serves.
func TestMountRepoKeepsAppleDoubleFilesVisible(t *testing.T) {
	setMacFUSEKernelExtensionLoaded(t, true)
	repo := model.RepoPaths{Name: "repo", MountPath: t.TempDir()}
	var kernel bool
	for _, attempt := range mountRepoAttempts(t, repo, slog.New(slog.DiscardHandler), unredacted) {
		if attempt.cfg.Backend != fuse.BackendFSKit {
			kernel = true
		}
		if !attempt.cfg.EnableAppleDouble {
			t.Fatalf("mount config for backend %q keeps noappledouble", attempt.cfg.Backend)
		}
	}
	if !kernel {
		t.Fatal("MountRepoWithGate never tried the kernel extension")
	}
}

// ENOSYS would make macOS store each extended attribute in an AppleDouble ._
// file, on either backend.
func TestSetXattrRefusesAppleDoubleFallback(t *testing.T) {
	fs := &ArtifactFuse{}
	if got := kernelErrno(fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Name: "com.apple.FinderInfo"})); got != syscall.EPERM {
		t.Fatalf("SetXattr = %v, want EPERM", got)
	}
}
