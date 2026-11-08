//go:build race && !windows

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// Every e2e mount installs this test binary as the FSMonitor hook. An adopted
// worktree takes the same hook without a mount.
func TestFSMonitorHookSkipsRaceExitSleep(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
	run(t, "", "git", "init", "--initial-branch=main", source)
	writeTestFile(t, source, "README.md", "base\n")
	run(t, source, "git", "add", "README.md")
	run(t, source, "git", "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
	run(t, source, "git", "worktree", "add", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(run(t, worktree, "git", "rev-parse", "--path-format=absolute", "--git-dir"))
	root := filepath.Join(dir, "state")
	ctl := newE2EController(t, root)
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := ctl.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	repos, err := ctl.ListRepos(ctx)
	if err != nil || len(repos) != 1 {
		t.Fatalf("ListRepos = %d repos, %v; want the adopted one", len(repos), err)
	}
	if err := gitstore.New(nil).ConfigureFSMonitorHook(ctx, repos[0], root); err != nil {
		t.Fatal(err)
	}
	gittest.RequireHookSkipsRaceExitSleep(t, worktree)
}
