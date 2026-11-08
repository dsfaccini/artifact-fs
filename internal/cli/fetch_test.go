package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func git(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitOutput runs git and returns its trimmed stdout.
func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// commitToSource commits a README.md that holds content to source's main
// branch, creating the repository first if needed, and returns the commit.
func commitToSource(t *testing.T, source, content string) string {
	t.Helper()
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		git(t, "init", "--initial-branch", "main", source)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", source, "add", "README.md")
	git(t, "-C", source, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", content)
	return gitOutput(t, "-C", source, "rev-parse", "HEAD")
}

// addSyncRepo registers repo, cloned from source, and returns its config.
func addSyncRepo(t *testing.T, root, source string, extra ...string) model.RepoConfig {
	t.Helper()
	if _, stderr, code := runCLI(t, append([]string{"add-repo", "--name", "repo", "--remote", source}, extra...)...); code != 0 {
		t.Fatalf("add-repo exit = %d, stderr=%q", code, stderr)
	}
	return registeredRepo(t, root, "repo")
}

// fetch updates the fetch ref's tracking ref and leaves HEAD where it is.
func TestFetchCLIFetchesTheFetchRef(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ARTIFACT_FS_ROOT", root)
	source := filepath.Join(t.TempDir(), "source")
	first := commitToSource(t, source, "first\n")
	cfg := addSyncRepo(t, root, source)
	second := commitToSource(t, source, "second\n")

	stdout, stderr, code := runCLI(t, "fetch", "--name", "repo")
	if code != 0 {
		t.Fatalf("fetch exit = %d, stderr=%q", code, stderr)
	}
	if stdout != "fetched repo\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "fetched repo\n")
	}
	if got := gitOutput(t, "--git-dir", cfg.GitDir, "rev-parse", "refs/remotes/origin/main"); got != second {
		t.Fatalf("refs/remotes/origin/main = %s, want the new commit %s", got, second)
	}
	if got := gitOutput(t, "--git-dir", cfg.GitDir, "rev-parse", "HEAD"); got != first {
		t.Fatalf("HEAD = %s, want it left at %s", got, first)
	}
}

// fetch refuses a repo whose remote refresh is off, and fetches nothing.
func TestFetchCLIRefusesARepoWithRefreshOff(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ARTIFACT_FS_ROOT", root)
	source := filepath.Join(t.TempDir(), "source")
	first := commitToSource(t, source, "first\n")
	cfg := addSyncRepo(t, root, source, "--refresh", "never")
	commitToSource(t, source, "second\n")

	stdout, stderr, code := runCLI(t, "fetch", "--name", "repo")
	if code == 0 {
		t.Fatalf("fetch of a repo with refresh off succeeded: %q", stdout)
	}
	if want := "remote refresh is disabled for this repository\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if got := gitOutput(t, "--git-dir", cfg.GitDir, "rev-parse", "refs/remotes/origin/main"); got != first {
		t.Fatalf("refs/remotes/origin/main = %s, want it left at %s", got, first)
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
	commitToSource(t, source, "main\n")
	cfg := addSyncRepo(t, root, source)
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
