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

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestStateRootDirectoriesArePrivate(t *testing.T) {
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
	if err := svc.AddRepo(ctx, model.RepoConfig{Name: "repo", RemoteURL: remote, Branch: "main", Enabled: true}); err != nil {
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
	if _, err := rt.engine.Read(ctx, "README.md", 0, make([]byte, 16)); err != nil {
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

func TestNewRefusesStateRootOthersCanWrite(t *testing.T) {
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
	if err := svc.AddRepoWithOptions(ctx, model.RepoConfig{Name: "repo", RemoteURL: "https://example.invalid/repo.git", Enabled: true}, AddRepoOptions{Async: true}); err != nil {
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

func TestNewNarrowsExistingStateDirectories(t *testing.T) {
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
