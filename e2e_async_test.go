//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestE2EAsyncPreparedGitDirBlocksUntilReady(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)
	skipIfOverMountBudget(t, 1)

	remoteURL := os.Getenv("AFS_E2E_REPO")
	if remoteURL == "" {
		remoteURL = createLocalTestRepo(t)
	}
	preparedGitDir, preparedWorktree := createPreparedGitDir(t, remoteURL)
	_ = preparedWorktree

	unblock := filepath.Join(t.TempDir(), "unblock-fetch")
	installBlockingGitFetchWrapper(t, unblock)

	repo := newAsyncPreparedE2ERepo(t, preparedGitDir, "main")

	waitForCondition(t, 10*time.Second, "async repo preparing", func() (bool, string) {
		st, err := repo.ctl.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		if st.State == string(model.PrepareStatePreparing) {
			return true, ""
		}
		return false, "state=" + st.State
	})

	done := make(chan error, 1)
	go func() {
		entries, err := os.ReadDir(repo.mountPath)
		if err == nil && len(entries) == 0 {
			err = os.ErrNotExist
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("ReadDir returned before async prepare was released: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	if err := os.WriteFile(unblock, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadDir after prepare release: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ReadDir did not unblock after async prepare completed")
	}

	primeE2EMountPolling(t, repo.mountPath)
	entries := lsDir(t, repo.mountPath)
	assertContains(t, entries, ".git")
	assertContains(t, entries, "README.md")
	assertGitStatus(t, repo.mountPath, map[string]string{})
}

func TestE2EAsyncPreparedGitDirFailureThenRetry(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)
	skipIfOverMountBudget(t, 1)

	remoteURL := os.Getenv("AFS_E2E_REPO")
	if remoteURL == "" {
		remoteURL = createLocalTestRepo(t)
	}
	preparedGitDir, preparedWorktree := createPreparedGitDir(t, "file://"+filepath.Join(t.TempDir(), "missing.git"))
	repo := newAsyncPreparedE2ERepo(t, preparedGitDir, "main")

	waitForCondition(t, 10*time.Second, "async prepare failure", func() (bool, string) {
		st, err := repo.ctl.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		if st.State == string(model.PrepareStateFailed) && st.PrepareError != "" {
			return true, ""
		}
		return false, "state=" + st.State + " prepare_error=" + st.PrepareError
	})

	if _, err := os.ReadDir(repo.mountPath); !errors.Is(err, syscall.EIO) {
		t.Fatalf("ReadDir after prepare failure = %v, want EIO", err)
	}

	gitCmd(t, preparedWorktree, "remote", "set-url", "origin", remoteURL)
	if _, err := repo.ctl.Prepare(context.Background(), repoName); err != nil {
		t.Fatalf("prepare retry: %v", err)
	}

	waitForCondition(t, 30*time.Second, "async prepare retry ready", func() (bool, string) {
		st, err := repo.ctl.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		if st.State == "mounted" && st.PrepareError == "" {
			return true, ""
		}
		return false, "state=" + st.State + " prepare_error=" + st.PrepareError
	})

	primeE2EMountPolling(t, repo.mountPath)
	entries := lsDir(t, repo.mountPath)
	assertContains(t, entries, "README.md")
	assertGitStatus(t, repo.mountPath, map[string]string{})
}

func createPreparedGitDir(t *testing.T, remoteURL string) (gitDir string, worktree string) {
	t.Helper()
	tmp := t.TempDir()
	gitDir = filepath.Join(tmp, "prepared.git")
	worktree = filepath.Join(tmp, "prepared")
	run(t, "", "git", "init", "--separate-git-dir", gitDir, "--initial-branch", "main", worktree)
	run(t, worktree, "git", "remote", "add", "origin", remoteURL)
	return gitDir, worktree
}

func installBlockingGitFetchWrapper(t *testing.T, unblockPath string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	// The wait gives up after 60 seconds, so a fetch the test never unblocks
	// ends with the test.
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "fetch" ]; then
    waited=0
    while [ ! -f "$AFS_ASYNC_GIT_UNBLOCK" ] && [ "$waited" -lt 1200 ]; do
      sleep 0.05
      waited=$((waited + 1))
    done
    break
  fi
done
exec "$AFS_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("AFS_ASYNC_GIT_UNBLOCK", unblockPath)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newAsyncPreparedE2ERepo(t *testing.T, preparedGitDir string, fetchRef string) *mountedE2ERepo {
	t.Helper()
	root, err := os.MkdirTemp("", "artifact-fs-e2e-async-root-*")
	if err != nil {
		t.Fatal(err)
	}
	mountDir, err := os.MkdirTemp("", "artifact-fs-e2e-async-mount-*")
	if err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	mountPath := filepath.Join(mountDir, repoName)
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		_ = os.RemoveAll(mountDir)
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	logs := &synchronizedBuffer{}
	svc, err := daemon.New(ctx, root, newE2ELogger(logs))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ctl := newE2EController(t, root)

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:      repoName,
			ID:        model.RepoID(repoName),
			MountRoot: mountDir,
			GitDir:    preparedGitDir,
		},
		RepoSpec: model.RepoSpec{
			Branch:         "main",
			PreparedGitDir: true,
			FetchRef:       fetchRef,
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: 5 * time.Minute,
		},
		Enabled: true,
	}
	if err := ctl.AddRepoWithOptions(ctx, cfg, daemon.AddRepoOptions{Async: true}); err != nil {
		cancel()
		_ = svc.Close()
		t.Fatalf("add-repo async prepared-gitdir: %v", err)
	}

	errCh := startE2EDaemon(ctx, svc)

	if err := waitForMount(ctl, mountPath, 60*time.Second); err != nil {
		cancel()
		_ = svc.Close()
		t.Fatal(err)
	}

	repo := &mountedE2ERepo{
		root:      root,
		mountDir:  mountDir,
		mountPath: mountPath,
		svc:       svc,
		ctl:       ctl,
		cancel:    cancel,
		errCh:     errCh,
		logs:      logs,
	}
	t.Cleanup(func() {
		if t.Failed() {
			logFuseRecords(t, repo.logs)
		}
		repo.close(t)
	})
	return repo
}
