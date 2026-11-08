package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/registry"
)

func git(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// fetch runs git in a process group of its own, which a terminal stops when
// it reads a prompt, so a remote that asks for credentials must fail the
// fetch instead of prompting.
func TestFetchCLIFailsWithoutPrompting(t *testing.T) {
	askpassRuns := gittest.IsolatePrompts(t)
	root := t.TempDir()
	t.Setenv("ARTIFACT_FS_ROOT", root)
	source := filepath.Join(t.TempDir(), "source")
	git(t, "init", "--initial-branch", "main", source)
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", source, "add", "README.md")
	git(t, "-C", source, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "main")
	if _, stderr, code := runCLI(t, "add-repo", "--name", "repo", "--remote", source); code != 0 {
		t.Fatalf("add-repo exit = %d, stderr=%q", code, stderr)
	}
	ctx := context.Background()
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := reg.GetRepo(ctx, "repo")
	if closeErr := reg.Close(); err != nil || closeErr != nil {
		t.Fatalf("get repo: %v, close: %v", err, closeErr)
	}
	git(t, "--git-dir", cfg.GitDir, "remote", "set-url", "origin", gittest.UnauthorizedRemote(t))

	stdout, stderr, code := runCLI(t, "fetch", "--name", "repo")
	if code == 0 {
		t.Fatalf("fetch from a remote that refuses every request succeeded: %q", stdout)
	}
	if !strings.Contains(stderr, "terminal prompts disabled") {
		t.Fatalf("stderr = %q, want the fetch's non-interactive failure", stderr)
	}
	gittest.RequireNoPrompt(t, askpassRuns)
}
