package gitstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestCommitTimestamp(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", ".")
	commit := exec.Command("git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	commit.Env = append(os.Environ(), "GIT_AUTHOR_DATE=@1000000000 +0000", "GIT_COMMITTER_DATE=@1700000000 +0000")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	store := New(nil)
	oid, _, err := store.ResolveHEAD(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.CommitTimestamp(t.Context(), cfg, oid)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 1700000000 {
		t.Fatalf("CommitTimestamp = %d, want the committer date 1700000000", ts)
	}
}

func TestSetBatchPoolSizeUpdatesExistingAndNewPools(t *testing.T) {
	t.Parallel()
	store := New(nil)
	first, err := store.getPool("/tmp/repo-a.git")
	if err != nil {
		t.Fatal(err)
	}
	if first.maxSize != 4 {
		t.Fatalf("initial pool maxSize = %d, want 4", first.maxSize)
	}

	store.SetBatchPoolSize(12)
	if first.maxSize != 12 {
		t.Fatalf("updated existing pool maxSize = %d, want 12", first.maxSize)
	}
	second, err := store.getPool("/tmp/repo-b.git")
	if err != nil {
		t.Fatal(err)
	}
	if second.maxSize != 12 {
		t.Fatalf("new pool maxSize = %d, want 12", second.maxSize)
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	_ = runOutput(t, name, args...)
}

func assertArtifactWorktreeConfig(t *testing.T, ctx context.Context, gitDir string) {
	t.Helper()
	for key, want := range map[string]string{
		"core.filemode":   "true",
		"core.ignorecase": "false",
		"core.symlinks":   "true",
	} {
		got, err := runGit(ctx, gitDir, "config", "--local", "--get", key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func runOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
	return string(out)
}
