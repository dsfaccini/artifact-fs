package fusefs

import (
	"context"
	"log/slog"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse/fuseops"
)

// Without FUSE_PARALLEL_DIROPS the kernel serializes every lookup and readdir
// in a directory.
func TestMountRepoEnablesParallelDirOps(t *testing.T) {
	repo := model.RepoConfig{Name: "repo", MountPath: t.TempDir()}
	for _, cfg := range mountRepoConfigs(t, repo, slog.New(slog.DiscardHandler)) {
		if !cfg.EnableParallelDirOps {
			t.Fatal("mount config leaves parallel directory ops off")
		}
	}
}

func TestSetXattrIsUnsupported(t *testing.T) {
	fs := &ArtifactFuse{}
	if got := kernelErrno(fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Name: "user.test"})); got != syscall.ENOSYS {
		t.Fatalf("SetXattr = %v, want ENOSYS", got)
	}
}
