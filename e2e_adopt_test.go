//go:build !windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// TestE2EAdoptedWorktree mounts a linked worktree adopted from a native
// checkout. Its subject is mounting an adopted registration, so it keeps its
// own mount.
func TestE2EAdoptedWorktree(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)
	skipIfFSKitHEADMove(t, "a commit through the adopted worktree")
	skipIfOverMountBudget(t, 1)

	base := filepath.Join(t.TempDir(), "base")
	run(t, "", "git", "clone", createLocalTestRepo(t), base)
	run(t, base, "git", "checkout", "main")
	root := t.TempDir()
	mountDir := newE2EMountDir(t)
	mountPath := filepath.Join(mountDir, repoName)
	run(t, base, "git", "worktree", "add", "--no-checkout", "-b", "topic", mountPath, "main")
	gitDir := strings.TrimSpace(run(t, mountPath, "git", "rev-parse", "--absolute-git-dir"))
	run(t, "", "git", "--git-dir", gitDir, "read-tree", "HEAD")
	run(t, base, "git", "worktree", "lock", "--reason", "ArtifactFS e2e", mountPath)
	// Adoption imports no native working files, so the gitfile moves out of the mountpoint.
	if err := os.Rename(filepath.Join(mountPath, ".git"), filepath.Join(t.TempDir(), "topic.gitfile")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	svc, err := daemon.New(ctx, root, logging.NewJSONLogger(os.Stderr, slog.LevelWarn))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ctl := newE2EController(t, root)
	if err := ctl.AddRepo(ctx, model.RepoConfig{Name: repoName, AdoptGitDir: true, GitDir: gitDir, MountRoot: mountDir, Enabled: true}); err != nil {
		cancel()
		_ = svc.Close()
		t.Fatalf("adopt worktree: %v", err)
	}
	errCh := startE2EDaemon(ctx, svc)
	if err := waitForMount(ctl, mountPath, 60*time.Second); err != nil {
		cancel()
		_ = svc.Close()
		t.Fatalf("adopted worktree mount: %v", err)
	}
	defer stopE2EDaemon(t, svc, cancel, errCh, mountPath)
	primeE2EMountPolling(t, mountPath)
	initial, err := ctl.Status(ctx, repoName)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := readFileStr(t, filepath.Join(mountPath, ".git")), "gitdir: "+gitDir+"\n"; got != want {
		t.Fatalf("mounted .git = %q, want %q", got, want)
	}
	// git status answers through the hook that the daemon installs, which runs this binary (see TestMain).
	if got, want := strings.TrimSpace(gitCmd(t, mountPath, "config", "--get", "core.fsmonitor")), filepath.Join(gitDir, "hooks", "artifact-fs-fsmonitor"); got != want {
		t.Fatalf("core.fsmonitor = %q, want %q", got, want)
	}
	assertGitStatus(t, mountPath, map[string]string{})
	writeTestFile(t, mountPath, "README.md", "# Edited through the adopted worktree\n")
	assertGitStatus(t, mountPath, map[string]string{"README.md": " M"})

	gitCmd(t, mountPath, "-c", "user.name=E2E Test", "-c", "user.email=e2e@test", "commit", "-am", "commit through the adopted worktree")
	head := strings.TrimSpace(gitCmd(t, mountPath, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(run(t, base, "git", "rev-parse", "refs/heads/topic")); got != head {
		t.Fatalf("base checkout sees topic at %s, want the commit made through the mount, %s", got, head)
	}
	waitForCondition(t, 15*time.Second, "HEAD reconcile after the commit", func() (bool, string) {
		st, err := ctl.Status(ctx, repoName)
		if err != nil {
			return false, err.Error()
		}
		return st.CurrentHEADOID == head && st.SnapshotGeneration == initial.SnapshotGeneration+1 && !st.DirtyOverlay,
			fmt.Sprintf("head=%s generation=%d dirty=%v", st.CurrentHEADOID, st.SnapshotGeneration, st.DirtyOverlay)
	})
	assertGitStatus(t, mountPath, map[string]string{})
}
