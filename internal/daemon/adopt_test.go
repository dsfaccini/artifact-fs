package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestAdoptGitDirPreservesGitStateAndScopesConfig(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	ctx := t.Context()
	dir := gittest.PrivateTempDir(t)
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
	// overlay does instead: nothing changed since the last token. Replace the
	// link, not the executable it names.
	hookPath := gitstore.FSMonitorHookLink(root, stored.ID)
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "config", "--get", "core.fsmonitor")); got != hookPath {
		t.Fatalf("core.fsmonitor = %q, want %q", got, hookPath)
	}
	if err := os.Remove(hookPath); err != nil {
		t.Fatal(err)
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
	t.Parallel()
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
	t.Parallel()
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
	warnings := func() int { return strings.Count(logs.String(), "adopted worktree remote embeds credentials") }
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

// Missing blobs and trees come from the promisor remote, so adoption warns
// when that remote embeds credentials, also when origin does not.
func TestAdoptGitDirWarnsAboutCredentialedPromisorRemote(t *testing.T) {
	t.Parallel()
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
	runCmd(t, "git", "-C", source, "remote", "add", "origin", "https://example.invalid/repository.git")
	runCmd(t, "git", "-C", source, "remote", "add", "upstream", "https://alice:pwqzz@example.invalid/repository.git")
	runCmd(t, "git", "-C", source, "config", "remote.upstream.promisor", "true")
	runCmd(t, "git", "-C", source, "config", "remote.upstream.partialclonefilter", "blob:none")
	runCmd(t, "git", "-C", source, "worktree", "add", "--no-checkout", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	runCmd(t, "git", "--git-dir", gitDir, "read-tree", "HEAD")

	logs := &lockedBuffer{}
	svc, err := New(ctx, filepath.Join(dir, "state"), slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatalf("adoption with a credentialed promisor remote failed: %v", err)
	}
	if got := strings.Count(logs.String(), "embeds credentials"); got != 1 {
		t.Fatalf("add-repo logged %d credential warnings, want 1: %s", got, logs.String())
	}
	if strings.Contains(logs.String(), "pwqzz") {
		t.Fatalf("warning logged the remote's password: %s", logs.String())
	}
}

// remove-repo hands the worktree back to native Git, so the filesystem
// settings adoption pinned for the mount go too. It unsets the hook link, also
// for a repo whose ID holds a regular expression metacharacter, and the hook
// script in the git dir. The script is the hook under a state root whose link
// path Git would hand to a shell, and an older artifact-fs left it under every
// state root.
func TestRemoveRepoRestoresAdoptedWorktreeFilesystemConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stateDir, repo string
		script               bool
	}{
		{"link", "state", "adopted", false},
		{"script", "state root", "adopted", true},
		{"metacharacter in the ID", "state", "a+b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := gittest.PrivateTempDir(t)
			source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
			runCmd(t, "git", "init", "--initial-branch=main", source)
			runCmd(t, "git", "-C", source, "config", "core.filemode", "false")
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "git", "-C", source, "add", "README.md")
			runCmd(t, "git", "-C", source, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
			runCmd(t, "git", "-C", source, "worktree", "add", "-b", "topic", worktree)
			gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
			root := filepath.Join(dir, tc.stateDir)
			svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: tc.repo, GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
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
			hook := gitstore.FSMonitorHookLink(root, stored.ID)
			if tc.script {
				hook = filepath.Join(gitDir, "hooks", "artifact-fs-fsmonitor")
			}
			worktreeConfig := filepath.Join(gitDir, "config.worktree")
			if got := strings.TrimSpace(runCmdOutput(t, "git", "config", "--file", worktreeConfig, "--get", "core.fsmonitor")); got != hook {
				t.Fatalf("core.fsmonitor = %q, want %q", got, hook)
			}
			pinned := []string{"core.fsmonitor", "fsmonitor.allowRemote", "core.filemode", "core.ignorecase", "core.symlinks"}
			for _, key := range pinned {
				runCmd(t, "git", "config", "--file", worktreeConfig, "--get", key)
			}
			if err := svc.ctl().RemoveRepo(ctx, cfg.Name); err != nil {
				t.Fatal(err)
			}
			for _, key := range pinned {
				if out, err := exec.Command("git", "config", "--file", worktreeConfig, "--get", key).Output(); err == nil {
					t.Fatalf("%s = %q after remove-repo, want it unset", key, out)
				}
			}
			if err := os.Chmod(filepath.Join(worktree, "README.md"), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := runCmdOutput(t, "git", "-C", worktree, "status", "--porcelain"); got != "" {
				t.Fatalf("status after remove-repo and a mode change = %q, want clean as core.filemode=false makes it", got)
			}
		})
	}
}

// The mount wrote the hook link under the state root it resolved then. After
// the root moved and a symlink took its old path, remove-repo resolves the
// root to the new path, and still unsets the link the mount wrote.
func TestRemoveRepoUnsetsTheHookLinkAfterTheStateRootMoved(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := gittest.PrivateTempDir(t)
	source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
	runCmd(t, "git", "init", "--initial-branch=main", source)
	commitFile(t, source, "README.md", "base\n")
	runCmd(t, "git", "-C", source, "worktree", "add", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	root := filepath.Join(dir, "state")
	svc, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	err = svc.ctl().AddRepo(ctx, cfg)
	var stored model.RepoConfig
	if err == nil {
		stored, err = svc.registry.GetRepo(ctx, cfg.Name)
	}
	if err == nil {
		err = svc.git.ConfigureFSMonitorHook(ctx, stored, svc.root)
	}
	if closeErr := svc.Close(); err != nil || closeErr != nil {
		t.Fatalf("configure the hook: %v, close: %v", err, closeErr)
	}
	worktreeConfig := filepath.Join(gitDir, "config.worktree")
	if got, want := strings.TrimSpace(runCmdOutput(t, "git", "config", "--file", worktreeConfig, "--get", "core.fsmonitor")), gitstore.FSMonitorHookLink(root, stored.ID); got != want {
		t.Fatalf("core.fsmonitor = %q, want %q", got, want)
	}
	moved := filepath.Join(dir, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, root); err != nil {
		t.Fatal(err)
	}
	ctl, err := NewController(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if err := ctl.RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "config", "--file", worktreeConfig, "--get", "core.fsmonitor").Output(); err == nil {
		t.Fatalf("core.fsmonitor = %q after remove-repo, want it unset", out)
	}
}

// Under a UTF-8 locale Git's "." matches no byte that is not valid UTF-8, and
// a Linux path can hold such a byte. remove-repo still unsets a hook link
// under such a state root.
func TestRemoveRepoUnsetsTheHookLinkUnderAStateRootThatIsNotUTF8(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS refuses a file name that is not valid UTF-8")
	}
	t.Setenv("LC_ALL", "C.UTF-8")
	ctx := t.Context()
	dir := gittest.PrivateTempDir(t)
	source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
	runCmd(t, "git", "init", "--initial-branch=main", source)
	commitFile(t, source, "README.md", "base\n")
	runCmd(t, "git", "-C", source, "worktree", "add", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	root := filepath.Join(dir, "state\xff")
	svc, err := New(ctx, root, discardLogger())
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
	if err := svc.git.ConfigureFSMonitorHook(ctx, stored, svc.root); err != nil {
		t.Fatal(err)
	}
	worktreeConfig := filepath.Join(gitDir, "config.worktree")
	if got, want := strings.TrimSpace(runCmdOutput(t, "git", "config", "--file", worktreeConfig, "--get", "core.fsmonitor")), gitstore.FSMonitorHookLink(root, stored.ID); got != want {
		t.Fatalf("core.fsmonitor = %q, want %q", got, want)
	}
	if err := svc.ctl().RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "config", "--file", worktreeConfig, "--get", "core.fsmonitor").Output(); err == nil {
		t.Fatalf("core.fsmonitor = %q after remove-repo, want it unset", out)
	}
}

// remove-repo unsets only the values adoption pins, so an adoption that
// failed before it wrote any leaves the worktree's own config.worktree alone.
func TestRemoveRepoAfterFailedAdoptionKeepsWorktreeConfig(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, worktree, elsewhere := filepath.Join(dir, "source"), filepath.Join(dir, "worktree"), filepath.Join(dir, "elsewhere")
	runCmd(t, "git", "init", "--initial-branch=main", source)
	commitFile(t, source, "README.md", "base\n")
	runCmd(t, "git", "-C", source, "worktree", "add", "-b", "topic", worktree)
	runCmd(t, "git", "-C", source, "config", "extensions.worktreeConfig", "true")
	runCmd(t, "git", "-C", worktree, "config", "--worktree", "core.symlinks", "false")
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, filepath.Join(dir, "state"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: elsewhere}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err == nil || !strings.Contains(err.Error(), "must match the registered worktree path") {
		t.Fatalf("adoption with a mismatched mount path = %v, want the path mismatch", err)
	}
	if err := svc.ctl().RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "config", "--worktree", "--get", "core.symlinks")); got != "false" {
		t.Fatalf("core.symlinks = %q after remove-repo, want the worktree's own false", got)
	}
}

// A worktree that an older artifact-fs adopted names the hook script in its
// git dir. A mount points core.fsmonitor at the hook link instead, and
// remove-repo unsets the link.
func TestAdoptedMountSwitchesAnOlderHookScriptToTheLink(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := gittest.PrivateTempDir(t)
	source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
	runCmd(t, "git", "init", "--initial-branch=main", source)
	commitFile(t, source, "README.md", "base\n")
	runCmd(t, "git", "-C", source, "worktree", "add", "--no-checkout", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	runCmd(t, "git", "--git-dir", gitDir, "read-tree", "HEAD")
	if err := os.Rename(filepath.Join(worktree, ".git"), filepath.Join(dir, "topic.gitfile")); err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, filepath.Join(dir, "state"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "adopted", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(gitDir, "hooks", "artifact-fs-fsmonitor")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", source, "config", "extensions.worktreeConfig", "true")
	worktreeConfig := filepath.Join(gitDir, "config.worktree")
	runCmd(t, "git", "config", "--file", worktreeConfig, "core.fsmonitor", script)

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
	if got, want := strings.TrimSpace(runCmdOutput(t, "git", "config", "--file", worktreeConfig, "--get", "core.fsmonitor")), gitstore.FSMonitorHookLink(svc.root, stored.ID); got != want {
		t.Fatalf("core.fsmonitor after the mount = %q, want %q", got, want)
	}
	if err := svc.ctl().RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "config", "--file", worktreeConfig, "--get", "core.fsmonitor").Output(); err == nil {
		t.Fatalf("core.fsmonitor = %q after remove-repo, want it unset", out)
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
