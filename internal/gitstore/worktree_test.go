package gitstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestConfigureStatusOptimizationDoesNotWriteIndex(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "README.md")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	gitDir := filepath.Join(repo, ".git")
	indexLock := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(indexLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", GitDir: gitDir, MountPath: repo}}
	if err := New(nil).ConfigureStatusOptimization(ctx, cfg, tmp); err != nil {
		t.Fatalf("ConfigureStatusOptimization: %v", err)
	}
	if _, err := os.Stat(indexLock); err != nil {
		t.Fatalf("daemon changed caller-owned index lock: %v", err)
	}
	if _, err := runGit(ctx, gitDir, "config", "--unset", "core.fsmonitor"); err != nil {
		t.Fatalf("disable test fsmonitor hook: %v", err)
	}
	if err := os.Remove(indexLock); err != nil {
		t.Fatal(err)
	}
}

// A daemon configures the hook again while Git may run it. The new hook goes
// to a file beside it and replaces it by rename, so until the rename the hook
// path is the whole old hook, which a Git can exec, and after it the new one.
// The space in the state root makes the hook the script in the git dir.
func TestReconfiguringTheFSMonitorHookLeavesTheOpenHookWhole(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	gitDir := filepath.Join(repo, ".git")
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", GitDir: gitDir, MountPath: repo}}
	stateRoot := filepath.Join(tmp, "state root")
	store := New(nil)
	if err := store.ConfigureFSMonitorHook(t.Context(), cfg, stateRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := runGit(context.Background(), gitDir, "config", "--unset", "core.fsmonitor"); err != nil {
			t.Errorf("disable test fsmonitor hook: %v", err)
		}
	})
	hookPath := fsmonitorHookPath(gitDir)
	opened, err := os.Open(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	before, err := opened.Stat()
	if err != nil {
		t.Fatal(err)
	}

	renaming := false
	store.testHookBeforeHookRename = func(path, _ string) {
		renaming = true
		current, err := os.Stat(path)
		if err != nil {
			t.Errorf("hook path before the rename: %v", err)
		} else if !os.SameFile(before, current) {
			t.Error("the hook path left the old hook before the new one was renamed over it")
		}
	}
	if err := store.ConfigureFSMonitorHook(t.Context(), cfg, stateRoot); err != nil {
		t.Fatal(err)
	}
	if !renaming {
		t.Fatal("reconfiguring wrote the hook without a rename over it")
	}
	after, err := os.Stat(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("reconfiguring rewrote the hook file in place, which a Git running it holds")
	}
	if after.Mode().Perm() != 0o755 {
		t.Fatalf("hook mode = %v, want 0755", after.Mode().Perm())
	}
	if entries, err := filepath.Glob(filepath.Join(filepath.Dir(hookPath), ".*")); err != nil || len(entries) != 0 {
		t.Fatalf("temporary hook files left = %v, %v", entries, err)
	}
}

// Reconfiguring the hook link renames a new link over it, so a Git that runs
// the hook meanwhile finds the old link or the new one.
func TestReconfiguringTheFSMonitorHookLinkLeavesALink(t *testing.T) {
	t.Parallel()
	tmp := gittest.PrivateTempDir(t)
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	gitDir := filepath.Join(repo, ".git")
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", GitDir: gitDir, MountPath: repo}}
	store := New(nil)
	if err := store.ConfigureFSMonitorHook(t.Context(), cfg, tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := runGit(context.Background(), gitDir, "config", "--unset", "core.fsmonitor"); err != nil {
			t.Errorf("disable test fsmonitor hook: %v", err)
		}
	})
	link := FSMonitorHookLink(tmp, cfg.ID)
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	renaming := false
	var stagedInfo os.FileInfo
	store.testHookBeforeHookRename = func(path, staged string) {
		renaming = true
		if stagedInfo, err = os.Lstat(staged); err != nil {
			t.Errorf("new link before the rename: %v", err)
		}
		current, err := os.Lstat(path)
		if err != nil {
			t.Errorf("hook link before the rename: %v", err)
		} else if !os.SameFile(before, current) {
			t.Error("the hook path left the old link before the new one was renamed over it")
		}
		if target, err := os.Readlink(staged); err != nil || target != exe {
			t.Errorf("new link before the rename = %q, %v; want a link to %q", target, err, exe)
		}
	}
	if err := store.ConfigureFSMonitorHook(t.Context(), cfg, tmp); err != nil {
		t.Fatal(err)
	}
	if !renaming {
		t.Fatal("reconfiguring made the link without a rename over it")
	}
	// A rename keeps the staged link's inode; a link made again at the path
	// gets a new one.
	if after, err := os.Lstat(link); err != nil || stagedInfo == nil || !os.SameFile(stagedInfo, after) {
		t.Fatalf("the hook link after reconfiguring is not the staged link renamed over it (lstat: %v)", err)
	}
	if target, err := os.Readlink(link); err != nil || target != exe {
		t.Fatalf("hook link = %q, %v; want a link to %q", target, err, exe)
	}
	if entries, err := filepath.Glob(filepath.Join(filepath.Dir(link), ".*")); err != nil || len(entries) != 0 {
		t.Fatalf("temporary hook links left = %v, %v", entries, err)
	}
}

func TestFSMonitorHookScriptQuotesArgs(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	pwned := filepath.Join(tmp, "pwned")
	hookPath := filepath.Join(tmp, "artifact-fs-fsmonitor")
	script := fsmonitorHookScript(tmp, "/bin/false", "repo$(touch "+pwned+")")
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	_ = exec.Command("sh", hookPath).Run()
	if _, err := os.Stat(pwned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated hook allowed shell command substitution; stat err = %v", err)
	}
}

func TestEnsureIndexInitializedPreservesStagedEntries(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "intent.txt"), []byte("intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "add", "-N", "intent.txt")
	// A hook that ran would discard the staged entries.
	dir := t.TempDir()
	fsmonitorCalled, fsmonitorHook := filepath.Join(dir, "fsmonitor-called"), filepath.Join(dir, "fsmonitor")
	if err := os.WriteFile(fsmonitorHook, []byte("#!/bin/sh\n: > '"+fsmonitorCalled+"'\ngit read-tree HEAD\nprintf 'token\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "core.fsmonitor", fsmonitorHook)

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	indexPath := filepath.Join(cfg.GitDir, "index")
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(nil).EnsureIndexInitialized(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fsmonitorCalled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fsmonitor hook ran during index validation: %v", err)
	}
	// Git writes an index by renaming a new file over it.
	if after, err := os.Stat(indexPath); err != nil || !os.SameFile(before, after) {
		t.Fatalf("index validation rewrote the index: %v", err)
	}
	staged := runOutput(t, "git", "-C", repo, "-c", "core.fsmonitor=false", "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "tracked.txt") {
		t.Fatalf("staged entries = %q, want tracked.txt", staged)
	}
	debug := runOutput(t, "git", "-C", repo, "-c", "core.fsmonitor=false", "ls-files", "--debug", "intent.txt")
	if !strings.Contains(debug, "flags: 20004000") {
		t.Fatalf("intent-to-add entry was not preserved:\n%s", debug)
	}
}

func TestEnsureIndexInitializedRejectsEmptyAlternateIndexPath(t *testing.T) {
	t.Setenv("GIT_INDEX_FILE", "")
	err := New(nil).EnsureIndexInitialized(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{GitDir: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "GIT_INDEX_FILE") {
		t.Fatalf("EnsureIndexInitialized error = %v, want empty GIT_INDEX_FILE error", err)
	}
}

func TestEnsureIndexInitializedCreatesMissingIndex(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	indexPath := filepath.Join(repo, ".git", "index")
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	if err := New(nil).EnsureIndexInitialized(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runOutput(t, "git", "-C", repo, "ls-files")); got != "tracked.txt" {
		t.Fatalf("index entries = %q, want tracked.txt", got)
	}
}

func TestBootstrapCleanIndexForFSMonitorMarksSnapshotEntriesWithoutReadingBlobs(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	oid := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "read-tree", "HEAD")

	gitDir := filepath.Join(repo, ".git")
	indexPath := filepath.Join(gitDir, "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	hook := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'token\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "core.fsmonitor", hook)
	postIndexChange := filepath.Join(gitDir, "hooks", "post-index-change")
	postIndexChangeCalled := filepath.Join(t.TempDir(), "post-index-change-called")
	if err := os.WriteFile(postIndexChange, []byte("#!/bin/sh\nprintf called > \"$AFS_POST_INDEX_CHANGE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_POST_INDEX_CHANGE", postIndexChangeCalled)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	logPath := filepath.Join(bin, "git.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AFS_GIT_LOG\"\nif [ \"$1\" = cat-file ]; then exit 88; fi\nexec \"$AFS_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_GIT_LOG", logPath)
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: gitDir, MountPath: repo}}
	bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, oid, nil)
	if err != nil || !bootstrapped {
		t.Fatalf("BootstrapCleanIndexForFSMonitor = %v, %v; want true, nil", bootstrapped, err)
	}
	if got := strings.TrimSpace(runOutput(t, "git", "-C", repo, "ls-files", "-f")); got != "h tracked.txt" {
		t.Fatalf("fsmonitor-valid entries = %q, want h tracked.txt", got)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("index was not updated")
	}
	afterMode, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterMode.Mode().Perm() != mode.Mode().Perm() {
		t.Fatalf("index permissions = %v, want %v", afterMode.Mode().Perm(), mode.Mode().Perm())
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "cat-file") {
		t.Fatalf("bootstrap read blob content: %s", logged)
	}
	if _, err := os.Stat(postIndexChangeCalled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private-index bootstrap invoked post-index-change hook: %v", err)
	}
	beforeCanceled, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, oid, func() (bool, error) {
		return false, context.Canceled
	}); bootstrapped || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation bootstrap = %v, %v; want false, canceled", bootstrapped, err)
	}
	if afterCanceled, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(afterCanceled, beforeCanceled) {
		t.Fatalf("canceled validation changed index: err=%v", err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(cancelCtx, cfg, oid, func() (bool, error) {
		cancel()
		return true, nil
	}); bootstrapped || !errors.Is(err, context.Canceled) {
		t.Fatalf("context-canceled validation bootstrap = %v, %v; want false, canceled", bootstrapped, err)
	}
	cancel()
	if afterCanceled, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(afterCanceled, beforeCanceled) {
		t.Fatalf("context-canceled validation changed index: err=%v", err)
	}
	if _, err := os.Stat(indexPath + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled validation left index lock: %v", err)
	}
}

func TestBootstrapCleanIndexForFSMonitorSkipsStagedIndexAndExistingLock(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	oid := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	indexPath := filepath.Join(cfg.GitDir, "index")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	stagedIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, oid, nil); err != nil || bootstrapped {
		t.Fatalf("staged bootstrap = %v, %v; want false, nil", bootstrapped, err)
	}
	if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, stagedIndex) {
		t.Fatalf("staged index changed: err=%v", err)
	}
	run(t, "git", "-C", repo, "read-tree", "HEAD")
	cleanIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := indexPath + ".lock"
	if err := os.WriteFile(lockPath, []byte("another writer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, oid, nil); err != nil || bootstrapped {
		t.Fatalf("locked bootstrap = %v, %v; want false, nil", bootstrapped, err)
	}
	if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, cleanIndex) {
		t.Fatalf("index changed while locked: err=%v", err)
	}
}

func TestBootstrapCleanIndexForFSMonitorLeavesCRLFNormalizingPathsToGit(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	// The blobs keep CRLF because the attributes arrive in a later commit, and
	// the worktree files hold the blob bytes, as the mount serves them.
	names := []string{"auto.txt", "auto-lf.txt", "binary.dat", "untext-lf.txt", "unset-lf.txt", "text.txt", "input.txt", "eol-lf.txt"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("a\r\nb\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, "git", "-C", repo, "add", ".")
	run(t, "git", "-C", repo, "commit", "-m", "crlf blobs")
	// check-attr prints -text and the literal text=unset alike, but Git
	// converts the second under eol=lf.
	attributes := "auto.txt text=auto\nauto-lf.txt text=auto eol=lf\nbinary.dat binary\nuntext-lf.txt -text eol=lf\nunset-lf.txt text=unset eol=lf\ntext.txt text\ninput.txt text=input\neol-lf.txt eol=lf\n"
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte(attributes), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", ".gitattributes")
	run(t, "git", "-C", repo, "commit", "-m", "attributes")
	oid := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))

	// Without stat data Git compares every file, which is the answer the
	// bootstrap must not change.
	run(t, "git", "-C", repo, "read-tree", "HEAD")
	want := runOutput(t, "git", "-C", repo, "-c", "core.fsmonitor=false", "status", "--porcelain")
	if want != " M eol-lf.txt\n M input.txt\n M text.txt\n M unset-lf.txt\n" {
		t.Fatalf("status without the bootstrap = %q, want the four CRLF-normalizing paths modified", want)
	}

	run(t, "git", "-C", repo, "read-tree", "HEAD")
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'token\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "core.fsmonitor", hook)
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, oid, nil); err != nil || !bootstrapped {
		t.Fatalf("BootstrapCleanIndexForFSMonitor = %v, %v; want true, nil", bootstrapped, err)
	}
	wantEntries := "h .gitattributes\nh auto-lf.txt\nh auto.txt\nh binary.dat\nH eol-lf.txt\nH input.txt\nH text.txt\nH unset-lf.txt\nH untext-lf.txt"
	if got := strings.TrimSpace(runOutput(t, "git", "-C", repo, "ls-files", "-f")); got != wantEntries {
		t.Fatalf("fsmonitor-valid entries = %q, want %q", got, wantEntries)
	}
	if got := runOutput(t, "git", "-C", repo, "status", "--porcelain"); got != want {
		t.Fatalf("status after the bootstrap = %q, want %q", got, want)
	}
}

// The bootstrap runs before the mount, where the hook issues a token it
// trusts later only for the bootstrap's own query.
func TestBootstrapCleanIndexForFSMonitorMarksItsHookQuery(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	oid := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "read-tree", "HEAD")
	dir := t.TempDir()
	marker, hook := filepath.Join(dir, "marker"), filepath.Join(dir, "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s' \"$"+FSMonitorBootstrapEnv+"\" > '"+marker+"'\nprintf 'token\\0/\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "core.fsmonitor", hook)
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, oid, nil); err != nil || !bootstrapped {
		t.Fatalf("BootstrapCleanIndexForFSMonitor = %v, %v; want true, nil", bootstrapped, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "1" {
		t.Fatalf("%s in the bootstrap's hook query = %q, %v; want 1", FSMonitorBootstrapEnv, got, err)
	}
}

// The bootstrap makes the private worktree whose hooks Git runs under the
// Store's temp directory, not in the system temp directory.
func TestBootstrapCleanIndexForFSMonitorKeepsItsWorktreeInTheStoreTempDir(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	oid := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "read-tree", "HEAD")
	dir := t.TempDir()
	shared, private := filepath.Join(dir, "shared"), filepath.Join(dir, "private")
	if err := errors.Join(os.Mkdir(shared, 0o777), os.Chmod(shared, 0o777), os.Mkdir(private, 0o700)); err != nil {
		t.Fatal(err)
	}
	sharedListing, privateListing, hook := filepath.Join(dir, "shared-listing"), filepath.Join(dir, "private-listing"), filepath.Join(dir, "fsmonitor")
	// The hook runs while the bootstrap's private worktree exists.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nls -A \"$TMPDIR\" > '"+sharedListing+"'\nls -A '"+private+"' > '"+privateListing+"'\nprintf 'token\\0/\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "core.fsmonitor", hook)
	t.Setenv("TMPDIR", shared)
	store := New(nil)
	store.SetTempDir(private)
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := store.BootstrapCleanIndexForFSMonitor(t.Context(), cfg, oid, nil); err != nil || !bootstrapped {
		t.Fatalf("BootstrapCleanIndexForFSMonitor = %v, %v; want true, nil", bootstrapped, err)
	}
	if got, err := os.ReadFile(sharedListing); err != nil || len(got) != 0 {
		t.Fatalf("the system temp directory held %q during the bootstrap, %v; want nothing", got, err)
	}
	got, err := os.ReadFile(privateListing)
	if name, ok := strings.CutSuffix(string(got), "\n"); err != nil || !ok || !strings.HasPrefix(name, "artifact-fs-index-worktree-") || strings.Contains(name, "\n") {
		t.Fatalf("the Store's temp directory held %q during the bootstrap, %v; want one private worktree", got, err)
	}
}

func TestBootstrapCleanIndexForFSMonitorSkipsConversionAttributes(t *testing.T) {
	t.Parallel()
	for _, attribute := range []string{"eol=crlf", "crlf", "filter=convert", "ident", "working-tree-encoding=UTF-16"} {
		t.Run(attribute, func(t *testing.T) {
			t.Parallel()
			repo := filepath.Join(t.TempDir(), "repo")
			run(t, "git", "init", repo)
			run(t, "git", "-C", repo, "config", "user.name", "test")
			run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
			if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", repo, "add", "tracked.txt")
			run(t, "git", "-C", repo, "commit", "-m", "base")
			// The attribute arrives after the blob, so adding it converts nothing.
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("tracked.txt "+attribute+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", repo, "add", ".gitattributes")
			run(t, "git", "-C", repo, "commit", "-m", "attributes")
			oid := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
			run(t, "git", "-C", repo, "read-tree", "HEAD")
			indexPath := filepath.Join(repo, ".git", "index")
			before, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
			if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, oid, nil); err != nil || bootstrapped {
				t.Fatalf("conversion attribute bootstrap = %v, %v; want false, nil", bootstrapped, err)
			}
			if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("conversion attribute changed index: err=%v", err)
			}
		})
	}
}

func TestBootstrapCleanIndexForFSMonitorRequiresSnapshotToMatchHEAD(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	staleOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "commit", "--allow-empty", "-m", "advance HEAD")
	run(t, "git", "-C", repo, "read-tree", "HEAD")
	indexPath := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, staleOID, nil); err != nil || bootstrapped {
		t.Fatalf("stale snapshot bootstrap = %v, %v; want false, nil", bootstrapped, err)
	}
	if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("stale snapshot changed index: err=%v", err)
	}
}

func TestBootstrapCleanIndexForFSMonitorSkipsTrackedEmptyIntentToAdd(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "empty.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "rm", "--cached", "empty.txt")
	run(t, "git", "-C", repo, "add", "-N", "empty.txt")
	indexPath := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, head, nil); err != nil || bootstrapped {
		t.Fatalf("tracked-empty ITA bootstrap = %v, %v; want false, nil", bootstrapped, err)
	}
	if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("tracked-empty ITA index changed: err=%v", err)
	}
}

func TestBootstrapCleanIndexForFSMonitorSkipsConfiguredAttributesFile(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "attrs"), []byte("tracked.txt filter=convert\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "filter.convert.clean", "tr a b")
	run(t, "git", "-C", repo, "add", "attrs", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	run(t, "git", "-C", repo, "config", "core.attributesFile", "attrs")
	head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "read-tree", "HEAD")
	indexPath := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, head, nil); err != nil || bootstrapped {
		t.Fatalf("configured attributes file bootstrap = %v, %v; want false, nil", bootstrapped, err)
	}
	if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("configured attributes file changed index: err=%v", err)
	}
}

func TestBootstrapCleanIndexForFSMonitorSkipsIntentToAddAndUnmergedEntries(t *testing.T) {
	t.Parallel()
	createRepo := func(t *testing.T) (string, string) {
		t.Helper()
		repo := filepath.Join(t.TempDir(), "repo")
		run(t, "git", "init", "--initial-branch=main", repo)
		run(t, "git", "-C", repo, "config", "user.name", "test")
		run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", repo, "add", "tracked.txt")
		run(t, "git", "-C", repo, "commit", "-m", "base")
		return repo, strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	}
	t.Run("intent-to-add", func(t *testing.T) {
		t.Parallel()
		repo, head := createRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "intent.txt"), []byte("intent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", repo, "add", "-N", "intent.txt")
		indexPath := filepath.Join(repo, ".git", "index")
		before, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
		if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, head, nil); err != nil || bootstrapped {
			t.Fatalf("intent-to-add bootstrap = %v, %v; want false, nil", bootstrapped, err)
		}
		if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("intent-to-add index changed: err=%v", err)
		}
	})
	t.Run("unmerged", func(t *testing.T) {
		t.Parallel()
		repo, _ := createRepo(t)
		run(t, "git", "-C", repo, "checkout", "-b", "side")
		if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("side\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", repo, "add", "tracked.txt")
		run(t, "git", "-C", repo, "commit", "-m", "side")
		run(t, "git", "-C", repo, "checkout", "main")
		if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", repo, "add", "tracked.txt")
		run(t, "git", "-C", repo, "commit", "-m", "main")
		merge := exec.Command("git", "-C", repo, "merge", "side")
		if out, err := merge.CombinedOutput(); err == nil {
			t.Fatalf("merge unexpectedly succeeded: %s", out)
		}
		if strings.TrimSpace(runOutput(t, "git", "-C", repo, "ls-files", "--unmerged")) == "" {
			t.Fatal("merge did not create unmerged index entries")
		}
		head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
		indexPath := filepath.Join(repo, ".git", "index")
		before, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
		if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, head, nil); err != nil || bootstrapped {
			t.Fatalf("unmerged bootstrap = %v, %v; want false, nil", bootstrapped, err)
		}
		if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("unmerged index changed: err=%v", err)
		}
	})
}

func TestBootstrapCleanIndexForFSMonitorSkipsAlternateIndex(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	indexPath := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(t.TempDir(), "alternate.index")
	if err := os.WriteFile(alternate, before, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INDEX_FILE", alternate)
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, head, nil); err != nil || bootstrapped {
		t.Fatalf("alternate-index bootstrap = %v, %v; want false, nil", bootstrapped, err)
	}
	if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("default index changed: err=%v", err)
	}
	if after, err := os.ReadFile(alternate); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("alternate index changed: err=%v", err)
	}
}

func TestBootstrapCleanIndexForFSMonitorSkipsMissingAttributesBlob(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", ".gitattributes", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	attrOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", ":.gitattributes"))
	run(t, "git", "-C", repo, "read-tree", "HEAD")
	indexPath := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(repo, ".git", "objects", attrOID[:2], attrOID[2:])
	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "repo", GitDir: filepath.Join(repo, ".git"), MountPath: repo}}
	if bootstrapped, err := New(nil).BootstrapCleanIndexForFSMonitor(context.Background(), cfg, head, nil); err != nil || bootstrapped {
		t.Fatalf("missing attributes blob bootstrap = %v, %v; want false, nil", bootstrapped, err)
	}
	if after, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("missing attributes blob changed index: err=%v", err)
	}
}

func TestEnsureIndexInitializedPreservesAlternateIndex(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")

	gitDir := filepath.Join(repo, ".git")
	defaultIndex := filepath.Join(gitDir, "index")
	alternateIndex := filepath.Join(t.TempDir(), "alternate.index")
	index, err := os.ReadFile(defaultIndex)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alternateIndex, index, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INDEX_FILE", alternateIndex)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	before := runOutput(t, "git", "-C", repo, "diff", "--cached")
	if !strings.Contains(before, "+staged") {
		t.Fatalf("alternate index was not staged: %q", before)
	}
	fsmonitorCalled := filepath.Join(t.TempDir(), "fsmonitor-called")
	fsmonitorHook := filepath.Join(t.TempDir(), "fsmonitor")
	script := "#!/bin/sh\n: > \"$AFS_FSMONITOR_CALLED\"\ngit read-tree HEAD\nprintf 'token\\n'\n"
	if err := os.WriteFile(fsmonitorHook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_FSMONITOR_CALLED", fsmonitorCalled)
	run(t, "git", "-C", repo, "config", "core.fsmonitor", fsmonitorHook)
	if err := os.Remove(defaultIndex); err != nil {
		t.Fatal(err)
	}

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: gitDir}}
	if err := New(nil).EnsureIndexInitialized(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fsmonitorCalled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fsmonitor hook ran during index validation: %v", err)
	}
	if after := runOutput(t, "git", "-c", "core.fsmonitor=false", "-C", repo, "diff", "--cached"); after != before {
		t.Fatalf("alternate staged diff changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestEnsureIndexInitializedCreatesMissingAlternateIndex(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")

	alternateIndex := filepath.Join(t.TempDir(), "alternate.index")
	t.Setenv("GIT_INDEX_FILE", alternateIndex)
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	if err := New(nil).EnsureIndexInitialized(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runOutput(t, "git", "-C", repo, "ls-files")); got != "tracked.txt" {
		t.Fatalf("alternate index entries = %q, want tracked.txt", got)
	}
}

func TestEnsureIndexInitializedRejectsCorruptIndex(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"truncated":         []byte("DIRC"),
		"invalid signature": append([]byte("NOPE"), make([]byte, 28)...),
		"invalid version":   append([]byte("DIRC\x00\x00\x00\x63"), make([]byte, 24)...),
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := filepath.Join(t.TempDir(), "repo")
			run(t, "git", "init", repo)
			run(t, "git", "-C", repo, "config", "user.name", "test")
			run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
			if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", repo, "add", "tracked.txt")
			run(t, "git", "-C", repo, "commit", "-m", "base")
			gitDir := filepath.Join(repo, ".git")
			if err := os.WriteFile(filepath.Join(gitDir, "index"), contents, 0o644); err != nil {
				t.Fatal(err)
			}

			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: gitDir}}
			if err := New(nil).EnsureIndexInitialized(context.Background(), cfg); err == nil {
				t.Fatal("EnsureIndexInitialized accepted corrupt index")
			}
		})
	}
}
