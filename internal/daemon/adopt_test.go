package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
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
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
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
	if _, err := svc.ctl().Prepare(ctx, cfg.Name); err != nil {
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
	if err := svc.ctl().SetRefresh(ctx, cfg.Name, model.RefreshPolicy{RefreshInterval: time.Second}); err == nil {
		t.Fatal("adoption allowed remote refresh to be enabled")
	}
	if err := svc.ctl().FetchNow(context.Background(), cfg.Name); err == nil {
		t.Fatal("adoption allowed a daemon fetch")
	}
}

func TestRemoveRepoStopsAdoptedFSMonitor(t *testing.T) {
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
	runCmd(t, "git", "-C", source, "worktree", "add", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	root := filepath.Join(dir, "state")
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.git.ConfigureFSMonitorHook(ctx, stored, root); err != nil {
		t.Fatal(err)
	}
	// The installed hook would run this test binary. Answer as an empty
	// overlay does instead: nothing changed since the last token.
	hookPath := filepath.Join(gitDir, "hooks", "artifact-fs-fsmonitor")
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "config", "--get", "core.fsmonitor")); got != hookPath {
		t.Fatalf("core.fsmonitor = %q, want %q", got, hookPath)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nprintf 'artifact-fs:adopted:1\\000'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runCmdOutput(t, "git", "-C", worktree, "status", "--porcelain"); got != "" {
		t.Fatalf("adopted worktree status = %q", got)
	}
	if err := svc.ctl().RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("native edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runCmdOutput(t, "git", "-C", worktree, "status", "--porcelain"); got != " M README.md\n" {
		t.Fatalf("status after remove-repo = %q, want the native edit", got)
	}
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", source, "config", "--get", "extensions.worktreeConfig")); got != "true" {
		t.Fatalf("extensions.worktreeConfig = %q", got)
	}
	// Removal still succeeds when core.fsmonitor is already unset or the
	// worktree's Git metadata is gone.
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.ctl().RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatalf("remove-repo with core.fsmonitor unset: %v", err)
	}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gitDir); err != nil {
		t.Fatal(err)
	}
	if err := svc.ctl().RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatalf("remove-repo without Git metadata: %v", err)
	}
}

func TestAdoptGitDirRejectsAcquisitionOptions(t *testing.T) {
	ctl, err := NewController(t.Context(), t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	for _, cfg := range []model.RepoConfig{
		{RepoSpec: model.RepoSpec{AdoptGitDir: true}},
		{RepoSpec: model.RepoSpec{AdoptGitDir: true, RemoteURL: "https://example.invalid/repo"}, RepoPaths: model.RepoPaths{GitDir: "/git"}},
		{RepoSpec: model.RepoSpec{AdoptGitDir: true, PreparedGitDir: true}, RepoPaths: model.RepoPaths{GitDir: "/git"}},
		{RepoSpec: model.RepoSpec{AdoptGitDir: true, FetchRef: "main"}, RepoPaths: model.RepoPaths{GitDir: "/git"}},
		{RepoSpec: model.RepoSpec{AdoptGitDir: true, Branch: "refs/heads/main"}, RepoPaths: model.RepoPaths{GitDir: "/git"}},
		{RepoSpec: model.RepoSpec{AdoptGitDir: true, HistoryDepth: 1}, RepoPaths: model.RepoPaths{GitDir: "/git"}},
		{RepoSpec: model.RepoSpec{AdoptGitDir: true, RequiredCommit: strings.Repeat("a", 40)}, RepoPaths: model.RepoPaths{GitDir: "/git"}},
	} {
		cfg.Name = "adopted"
		if err := ctl.AddRepo(t.Context(), cfg); err == nil {
			t.Fatalf("accepted incompatible adoption config: %+v", cfg)
		}
		if _, err := ctl.registry.GetRepo(t.Context(), cfg.Name); err == nil {
			t.Fatalf("registered incompatible adoption config: %+v", cfg)
		}
	}
}

func TestAdoptGitDirWarnsAboutCredentialedOriginOncePerPrepare(t *testing.T) {
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
	// Remote detection starts at the "git//" inside the password, so a redacted
	// origin field would keep "alice:pwq". The password is not hex, so a logged
	// object ID cannot hold it.
	const origin = "gcrypt::rsync://alice:pwqgit//xyz@example.invalid/repository.git"
	runCmd(t, "git", "-C", source, "remote", "add", "origin", origin)
	// The README's adoption steps leave the mountpoint empty.
	runCmd(t, "git", "-C", source, "worktree", "add", "--no-checkout", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	runCmd(t, "git", "--git-dir", gitDir, "read-tree", "HEAD")
	if err := os.Rename(filepath.Join(worktree, ".git"), filepath.Join(dir, "topic.gitfile")); err != nil {
		t.Fatal(err)
	}

	logs := &lockedBuffer{}
	svc, err := New(ctx, filepath.Join(dir, "state"), slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	warnings := func() int { return strings.Count(logs.String(), "adopted worktree origin embeds credentials") }
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatalf("adoption with a credentialed origin failed: %v", err)
	}
	stored, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PrepareState != "" || stored.PrepareError != "" {
		t.Fatalf("adopted repo prepare state=%q error=%q, want ready", stored.PrepareState, stored.PrepareError)
	}
	if got := warnings(); got != 1 {
		t.Fatalf("add-repo logged %d credentialed-origin warnings, want 1: %s", got, logs.String())
	}

	svc.mount = fakeMount
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running[stored.ID]
	svc.mu.Unlock()
	if rt == nil || rt.mfs == nil {
		t.Fatal("adopted repo is not mounted after syncRepos")
	}
	if got := warnings(); got != 2 {
		t.Fatalf("add-repo and one mount logged %d credentialed-origin warnings, want 2: %s", got, logs.String())
	}
	if strings.Contains(logs.String(), "pwq") || strings.Contains(logs.String(), `"origin":`) {
		t.Fatalf("warning logged the origin: %s", logs.String())
	}
	if strings.Contains(logs.String(), "example.invalid/repository.git") {
		t.Fatalf("warning kept the remote's location: %s", logs.String())
	}
	if got := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "remote", "get-url", "origin")); got != origin {
		t.Fatalf("adoption changed origin to %q", got)
	}
}

func TestAdoptedMountConfiguresWorktreeOnce(t *testing.T) {
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
	runCmd(t, "git", "-C", source, "worktree", "add", "--no-checkout", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	runCmd(t, "git", "--git-dir", gitDir, "read-tree", "HEAD")
	if err := os.Rename(filepath.Join(worktree, ".git"), filepath.Join(dir, "topic.gitfile")); err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, filepath.Join(dir, "state"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	spawnLog := filepath.Join(dir, "git-spawns.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AFS_GIT_SPAWN_LOG\"\nexec \"$AFS_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("AFS_GIT_SPAWN_LOG", spawnLog)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc.mount = fakeMount
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running[stored.ID]
	svc.mu.Unlock()
	if rt == nil || rt.mfs == nil {
		t.Fatal("adopted repo is not mounted after syncRepos")
	}
	data, err := os.ReadFile(spawnLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("adopted daemon mount ran %d git commands", strings.Count(string(data), "\n"))
	// ConfigureAdoptedWorktree is the only caller of --git-common-dir.
	if got := strings.Count(string(data), "rev-parse --path-format=absolute --git-common-dir\n"); got != 1 {
		t.Fatalf("adopted mount validated the worktree %d times, want 1:\n%s", got, data)
	}
}
