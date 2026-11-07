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
	repo := model.RepoPaths{Name: "repo", MountPath: t.TempDir()}
	for _, attempt := range mountRepoAttempts(t, repo, slog.New(slog.DiscardHandler), unredacted) {
		if !attempt.cfg.EnableParallelDirOps {
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

// With DisableWritebackCaching, FUSE_AUTO_INVAL_DATA makes the kernel drop a
// file's cached pages when its mtime changes, not only its size.
func TestMountRepoEnablesAutoInvalData(t *testing.T) {
	repo := model.RepoPaths{Name: "repo", MountPath: t.TempDir()}
	for _, attempt := range mountRepoAttempts(t, repo, slog.New(slog.DiscardHandler), unredacted) {
		if !attempt.cfg.EnableAutoInvalData || !attempt.cfg.DisableWritebackCaching {
			t.Fatalf("mount config: EnableAutoInvalData %t, DisableWritebackCaching %t; want both", attempt.cfg.EnableAutoInvalData, attempt.cfg.DisableWritebackCaching)
		}
	}
}
