package daemon

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

func TestStateRootDirectoriesArePrivate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	if err := os.WriteFile(filepath.Join(remote, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", remote, "add", "README.md")
	runCmd(t, "git", "-C", remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "main")

	root := filepath.Join(tmp, "state")
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	// Mounting opens the overlay; a read hydrates a blob into the cache.
	svc.mount = fakeMount
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running[cfg.ID]
	svc.mu.Unlock()
	if rt == nil {
		t.Fatal("repo is not running after syncRepos")
	}
	if err := rt.engine.Do(ctx, nil, func(ctx context.Context, b *fusefs.Blobs) error {
		_, err := rt.engine.Read(ctx, b, "README.md", 0, make([]byte, 16))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Git creates the clone's own directories; mount points are not state.
	root = svc.root
	skip := map[string]bool{cfg.GitDir: true, filepath.Join(root, "mnt"): true}
	visited := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if skip[path] {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		visited[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s mode = %04o, want 0700", path, mode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"config/locks", "repos/repo", "overlays/repo/upper", "cache/blobs/repo"} {
		if !visited[filepath.FromSlash(dir)] {
			t.Errorf("state directory %s was not created", dir)
		}
	}
}

// The SSH wrapper that Git runs lives under the state root, which
// ensureStateRoot checks, and not in the system temp directory. The daemon's
// lives in config/tmp/daemon, which New empties.
func TestSSHWrapperLivesUnderTheStateRoot(t *testing.T) {
	gittest.IsolatePrompts(t)
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	commitFile(t, remote, "README.md", "main\n")
	bin, shared := filepath.Join(tmp, "bin"), filepath.Join(tmp, "shared")
	if err := errors.Join(os.Mkdir(bin, 0o700), os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 255\n"), 0o700), os.Mkdir(shared, 0o777), os.Chmod(shared, 0o777)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", shared)
	for _, tt := range []struct {
		name string
		// open returns a controller on root and the directory, relative to
		// the state root, where its git store writes the SSH wrapper.
		open func(t *testing.T, root string) (*Controller, string)
	}{
		{"controller", func(t *testing.T, root string) (*Controller, string) {
			ctl, err := NewController(t.Context(), root, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ctl.Close() })
			return ctl, filepath.Join("config", "tmp")
		}},
		{"daemon", func(t *testing.T, root string) (*Controller, string) {
			svc, err := New(t.Context(), root, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { svc.Close() })
			return svc.ctl(), filepath.Join("config", "tmp", "daemon")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctl, tempDir := tt.open(t, filepath.Join(tmp, tt.name))
			if err := ctl.AddRepo(t.Context(), model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(shared)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("the system temp directory holds %s; want nothing", entries[0].Name())
			}
			wrappers, err := filepath.Glob(filepath.Join(ctl.root, tempDir, "artifact-fs-ssh-*", "ssh"))
			if err != nil || len(wrappers) != 1 {
				t.Fatalf("SSH wrappers in %s = %q, %v; want one", tempDir, wrappers, err)
			}
		})
	}
}

// New empties config/tmp/daemon, which only the daemon that holds the lock
// uses, so a daemon that stopped without closing its git store leaves nothing
// there for long. It leaves the rest of config/tmp, which the CLI's git
// stores use at the same time.
func TestNewEmptiesTheDaemonTempDir(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "state")
	daemonTemp := filepath.Join(root, "config", "tmp", "daemon")
	cliEntry := filepath.Join(root, "config", "tmp", "artifact-fs-ssh-cli")
	for _, dir := range []string{filepath.Join(daemonTemp, "artifact-fs-ssh-left"), cliEntry} {
		if err := errors.Join(os.MkdirAll(dir, 0o700), os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"), 0o700)); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := New(t.Context(), root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if entries, err := os.ReadDir(daemonTemp); err != nil || len(entries) != 0 {
		t.Fatalf("%s after New = %v, %v; want an empty directory", daemonTemp, entries, err)
	}
	if _, err := os.Stat(filepath.Join(cliEntry, "ssh")); err != nil {
		t.Fatalf("New removed the CLI's SSH wrapper: %v", err)
	}
}

func TestNewRefusesStateRootOthersCanWrite(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		dir  string
		mode os.FileMode
	}{
		{"group-writable root", ".", 0o775},
		{"world-writable root", ".", 0o757},
		{"world-writable config", "config", 0o777},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "state")
			if err := os.MkdirAll(filepath.Join(root, tt.dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(root, tt.dir), tt.mode); err != nil {
				t.Fatal(err)
			}
			svc, err := New(context.Background(), root, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil {
				svc.Close()
				t.Fatal("New accepted a state root others can write")
			}
			if !strings.Contains(err.Error(), "writable by group or others") {
				t.Fatalf("New error = %v, want writable rejection", err)
			}
			if _, err := os.Stat(filepath.Join(root, "config", "repos.sqlite")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("registry was created in a refused state root: %v", err)
			}
		})
	}
}

func TestNewRefusesStateRootOwnedByAnotherUser(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root owns /")
	}
	svc, err := New(context.Background(), "/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		svc.Close()
		t.Fatal("New accepted a state root owned by another user")
	}
	if !strings.Contains(err.Error(), "owned by another user") {
		t.Fatalf("New error = %v, want ownership rejection", err)
	}
}

func TestNewKeepsResolvedStateRootAfterLinkRetarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	original, attacker := filepath.Join(tmp, "original"), filepath.Join(tmp, "attacker")
	for _, dir := range []string{original, attacker} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(tmp, "state")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, link, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// Whoever owns the link can repoint it after New checks the target.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatal(err)
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.invalid/repo.git"}, Enabled: true}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.GitDir, cfg.OverlayDir, cfg.BlobCacheDir, cfg.MetaDBPath} {
		if !strings.HasPrefix(path, resolved+string(filepath.Separator)) {
			t.Errorf("repo path %s is not under the state root New resolved, %s", path, resolved)
		}
	}
	if entries, err := os.ReadDir(attacker); err != nil || len(entries) != 0 {
		t.Fatalf("the service wrote through the retargeted link: %v, %v", entries, err)
	}
}

// A relative state root resolves against the working directory once. A
// relative path would fail each store's SQLite URI, which reads its first
// segment as an authority, and the hook link Git runs from the worktree.
func TestRelativeStateRootGivesGitAHookItCanRun(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	worktree := filepath.Join(dir, "worktree")
	runCmd(t, "git", "init", "--initial-branch", "main", worktree)
	commitFile(t, worktree, "README.md", "base\n")
	svc, err := New(ctx, "state", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", GitDir: filepath.Join(worktree, ".git"), MountPath: worktree}}
	if err := svc.git.ConfigureFSMonitorHook(ctx, cfg, svc.root); err != nil {
		t.Fatal(err)
	}
	if hook, want := requireGitRunsFSMonitorHook(t, worktree), gitstore.FSMonitorHookLink(filepath.Join(dir, "state"), cfg.ID); hook != want {
		t.Fatalf("core.fsmonitor = %q, want %q", hook, want)
	}
}

func TestNewNarrowsExistingStateDirectories(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "state")
	// Earlier versions created the root and its subdirectories 0755.
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, filepath.Join(root, "config")} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := New(context.Background(), root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc.Close()
	for dir, want := range map[string]os.FileMode{root: 0o755, filepath.Join(root, "config"): 0o700} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", dir, got, want)
		}
	}
}

func TestNewRefusesStateRootLinkOwnedByAnotherUser(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Only root can chown a symlink, so report the link as another user's.
	realLstat := lstat
	t.Cleanup(func() { lstat = realLstat })
	lstat = func(path string, st *unix.Stat_t) error {
		err := realLstat(path, st)
		if path == link {
			st.Uid++
		}
		return err
	}

	svc, err := New(context.Background(), link, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		svc.Close()
		t.Fatal("New accepted a state root link another user owns")
	}
	if !strings.Contains(err.Error(), "owned by another user") {
		t.Fatalf("New error = %v, want ownership rejection", err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("New wrote into the link's target: %v, %v", entries, err)
	}
}

func TestNewRefusesDanglingStateRootLinkBeforeCreatingItsTarget(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "missing")
	link := filepath.Join(tmp, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Only root can chown a symlink, so report the link as another user's.
	realLstat := lstat
	t.Cleanup(func() { lstat = realLstat })
	lstat = func(path string, st *unix.Stat_t) error {
		err := realLstat(path, st)
		if path == link {
			st.Uid++
		}
		return err
	}

	// With the trailing slash, macOS's mkdir follows the dangling link.
	svc, err := New(context.Background(), link+"/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		svc.Close()
		t.Fatal("New accepted a state root link another user owns")
	}
	if !strings.Contains(err.Error(), "owned by another user") {
		t.Fatalf("New error = %v, want ownership rejection", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New created the dangling link's target: %v", err)
	}
}
