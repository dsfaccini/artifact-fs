package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestAdoptGitDirPreservesGitStateAndScopesConfig(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
	runCmd(t, "git", "init", "--initial-branch=main", source)
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", source, "add", "README.md")
	runCmd(t, "git", "-C", source, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
	runCmd(t, "git", "-C", source, "remote", "add", "contributor", "https://example.invalid/repository.git")
	runCmd(t, "git", "-C", source, "config", "branch.topic.remote", "contributor")
	runCmd(t, "git", "-C", source, "config", "branch.topic.merge", "refs/heads/upstream-topic")
	runCmd(t, "git", "-C", source, "config", "core.fsmonitor", "false")
	runCmd(t, "git", "-C", source, "config", "core.filemode", "false")
	runCmd(t, "git", "-C", source, "worktree", "add", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", worktree, "add", "README.md")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(gitDir, "index")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeHEAD := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "HEAD"))
	// Exercise the migration Git requires when enabling worktreeConfig.
	runCmd(t, "git", "-C", source, "config", "core.worktree", source)
	svc, err := New(ctx, filepath.Join(dir, "state"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{Name: "adopted", GitDir: gitDir, MountPath: worktree, AdoptGitDir: true, Enabled: true}
	if err := svc.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.AdoptGitDir || !stored.RemoteRefreshDisabled || stored.Branch != "HEAD" || stored.FetchRef != "" {
		t.Fatalf("adopted config policy: %+v", stored)
	}
	if err := svc.mountRepo(ctx, stored); err == nil || !strings.Contains(err.Error(), "adopted mountpoint must be empty") {
		t.Fatalf("adoption must not hide native working files: %v", err)
	}
	if err := svc.Prepare(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatalf("adoption changed the staged index: %v", err)
	}
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "HEAD")); got != beforeHEAD {
		t.Fatalf("HEAD changed to %s", got)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "README.md")); err != nil || string(data) != "unstaged\n" {
		t.Fatalf("unstaged data changed: %q, %v", data, err)
	}
	for _, checkout := range []string{source, worktree} {
		if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", checkout, "rev-parse", "--show-toplevel")); got != checkout {
			t.Fatalf("worktree identity %q, want %q", got, checkout)
		}
		if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", checkout, "remote", "get-url", "contributor")); got != "https://example.invalid/repository.git" {
			t.Fatalf("remote changed: %s", got)
		}
	}
	for key, want := range map[string]string{"branch.topic.remote": "contributor", "branch.topic.merge": "refs/heads/upstream-topic"} {
		if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "config", "--get", key)); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if err := svc.git.ConfigureFSMonitorHook(ctx, stored, filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", source, "config", "--get", "core.fsmonitor")); got != "false" {
		t.Fatalf("FSMonitor leaked into native checkout: %s", got)
	}
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", source, "config", "--get", "core.filemode")); got != "false" {
		t.Fatalf("filemode leaked into native checkout: %s", got)
	}
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "config", "--get", "core.filemode")); got != "true" {
		t.Fatalf("adopted filemode = %s", got)
	}
	if err := svc.SetRefresh(ctx, cfg.Name, time.Second, false); err == nil {
		t.Fatal("adoption allowed remote refresh to be enabled")
	}
	if err := svc.FetchNow(context.Background(), cfg.Name); err == nil {
		t.Fatal("adoption allowed a daemon fetch")
	}
}

func TestAdoptGitDirRejectsAcquisitionOptions(t *testing.T) {
	for _, cfg := range []model.RepoConfig{
		{AdoptGitDir: true},
		{AdoptGitDir: true, GitDir: "/git", RemoteURL: "https://example.invalid/repo"},
		{AdoptGitDir: true, GitDir: "/git", PreparedGitDir: true},
		{AdoptGitDir: true, GitDir: "/git", FetchRef: "main"},
		{AdoptGitDir: true, GitDir: "/git", Branch: "refs/heads/main"},
		{AdoptGitDir: true, GitDir: "/git", HistoryDepth: 1},
		{AdoptGitDir: true, GitDir: "/git", RequiredCommit: strings.Repeat("a", 40)},
	} {
		if err := normalizeSourceConfig(&cfg); err == nil {
			t.Fatalf("accepted incompatible adoption config: %+v", cfg)
		}
	}
}
