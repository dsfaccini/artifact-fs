package gitstore

import (
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// A fetch that fails before its remote lookup returns reports "git command failed" without git's message.
func TestFetchKeepsTheCauseOfAFailureBeforeItsRemoteLookup(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	const cause = "fatal: protocol error: bad pack header"
	fetchFailed := filepath.Join(tmp, "fetch-failed")
	// The lookup answers half a second after the fetch failed, so it still
	// runs when the fetch reports its failure.
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  'remote get-url origin')\n" +
		"    i=0\n" +
		"    while [ ! -e \"$AFS_FETCH_FAILED\" ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done\n" +
		"    sleep 0.5\n" +
		"    echo /configured/repo.git;;\n" +
		"  fetch*) : > \"$AFS_FETCH_FAILED\"; printf '%s\\n' '" + cause + "' >&2; exit 128;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_FETCH_FAILED", fetchFailed)

	store := New(nil)
	store.gitRetryDelays = nil
	cfg := model.RepoConfig{Name: "repo", GitDir: filepath.Join(tmp, "repo.git"), RemoteURL: "/configured/repo.git", Branch: "refs/heads/main"}
	for _, c := range []struct {
		name  string
		fetch func() error
	}{
		{"Store.Fetch", func() error { return store.Fetch(t.Context(), cfg) }},
		{"Store.FetchRefNonInteractive", func() error { return store.FetchRefNonInteractive(t.Context(), cfg, "main") }},
	} {
		if err := os.Remove(fetchFailed); err != nil && !errors.Is(err, iofs.ErrNotExist) {
			t.Fatal(err)
		}
		if err := c.fetch(); err == nil || !strings.Contains(err.Error(), cause) {
			t.Errorf("%s returned %v; want the error to keep git's message %q", c.name, err, cause)
		}
	}
}

// A fetch of a prepared git dir sets GIT_SSH_COMMAND, which overrides the git dir's core.sshCommand.
func TestPreparedGitDirFetchHonorsCoreSSHCommand(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	pathSSHRan := filepath.Join(tmp, "path-ssh-ran")
	coreSSHRan := filepath.Join(tmp, "core-ssh-ran")
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\n: > \"$AFS_PATH_SSH_RAN\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	coreSSH := filepath.Join(tmp, "core-ssh-command")
	if err := os.WriteFile(coreSSH, []byte("#!/bin/sh\n: > \"$AFS_CORE_SSH_RAN\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_PATH_SSH_RAN", pathSSHRan)
	t.Setenv("AFS_CORE_SSH_RAN", coreSSHRan)
	// Git lets a set GIT_SSH_COMMAND, even an empty one, override
	// core.sshCommand, so the test runs git without it.
	t.Setenv("GIT_SSH_COMMAND", "")
	if err := os.Unsetenv("GIT_SSH_COMMAND"); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(tmp, "repo.git")
	run(t, "git", "init", "-q", "--bare", gitDir)
	run(t, "git", "--git-dir", gitDir, "remote", "add", "origin", "ssh://git@example.invalid/repo.git")
	run(t, "git", "--git-dir", gitDir, "config", "core.sshCommand", coreSSH)

	store := New(nil)
	store.gitRetryDelays = nil
	cfg := model.RepoConfig{Name: "repo", GitDir: gitDir, PreparedGitDir: true, Branch: "refs/heads/main"}
	fetchErr := store.FetchRefNonInteractive(t.Context(), cfg, "refs/heads/main")
	_, coreErr := os.Stat(coreSSHRan)
	_, pathErr := os.Stat(pathSSHRan)
	switch {
	case coreErr == nil:
	case pathErr == nil:
		t.Errorf("FetchRefNonInteractive ran the ssh on PATH, not the git dir's core.sshCommand; want git to run core.sshCommand")
	default:
		t.Errorf("FetchRefNonInteractive ran no ssh (fetch error: %v); want git to run the git dir's core.sshCommand", fetchErr)
	}
}
