package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestPrepareExistingCloneRejectsCredentialedRemoteBeforeSetURL(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, "repo.git")
	worktree := filepath.Join(tmp, "worktree")
	run(t, "git", "init", "--separate-git-dir", gitDir, "--initial-branch", "master", worktree)
	run(t, "git", "-C", worktree, "remote", "add", "origin", "https://github.com/org/repo.git")

	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "set-url-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\ncase \"$*\" in *\"remote set-url\"*) : > \"$GIT_SET_URL_MARKER\";; esac\nexec /usr/bin/git \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SET_URL_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := New(nil)
	for _, remote := range []string{
		"ssh:/git:secret@github.com/org/repo.git",
		"ssh//git:secret@github.com/org/repo.git",
		"ssh:/git:pa/ss@github.com/org/repo.git",
		"alice:ghp_secret@github.com:org/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: gitDir, RemoteURL: remote, Branch: "master", FetchRef: "master"}
			err := store.PrepareExistingCloneNonInteractive(context.Background(), cfg)
			if err == nil {
				t.Fatal("expected credentialed remote rejection")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked credential: %v", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("git remote set-url was invoked before rejecting credentialed remote")
			}
		})
	}
}

func TestExistingCloneCredentialOperationsKeepCredentialsOutOfArguments(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(tmp, "commands")
	credentials := filepath.Join(tmp, "credentials")
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$AFS_GIT_COMMANDS\"\n" +
		"if [ \"$*\" = 'remote get-url origin' ]; then printf '%s\\n' 'https://example.com/old/repo.git'; exit 0; fi\n" +
		"if [ \"$1\" = 'fetch' ]; then printf '%s:%s\\n' \"$ARTIFACT_FS_GIT_USERNAME\" \"$ARTIFACT_FS_GIT_PASSWORD\" > \"$AFS_GIT_CREDENTIALS\"; fi\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_GIT_COMMANDS", commands)
	t.Setenv("AFS_GIT_CREDENTIALS", credentials)

	store := New(nil)
	repo := model.RepoConfig{
		Name:      "repo",
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https://alice:super-secret@example.com/org/repo.git",
		Branch:    "main",
	}
	if err := store.ConfigureRemoteWithCredentials(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if err := store.FetchRefWithCredentials(context.Background(), repo, "main"); err != nil {
		t.Fatal(err)
	}

	commandData, err := os.ReadFile(commands)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commandData), "alice") || strings.Contains(string(commandData), "super-secret") {
		t.Fatalf("credentials appeared in git arguments: %s", commandData)
	}
	if !strings.Contains(string(commandData), "remote set-url origin https://example.com/org/repo.git") {
		t.Fatalf("sanitized remote was not configured: %s", commandData)
	}
	credentialData, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if string(credentialData) != "alice:super-secret\n" {
		t.Fatalf("credential helper environment = %q", credentialData)
	}
}

func TestValidatePreparedGitDirRejectsCredentialedOrigin(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")
	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "https://ghp_secret@github.com/org/repo.git")

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
	store := New(nil)
	err := store.ValidatePreparedGitDir(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected credentialed origin rejection")
	}
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Fatalf("error leaked origin credential: %v", err)
	}
}

func TestValidatePreparedGitDirRejectsMalformedCredentialedOrigin(t *testing.T) {
	for _, remote := range []string{
		"ssh:/git:secret@github.com/org/repo.git",
		"alice:ghp_secret@github.com:org/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			tmp := t.TempDir()
			preparedGitDir := filepath.Join(tmp, "prepared.git")
			preparedWorktree := filepath.Join(tmp, "prepared")
			run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
			run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", remote)

			cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
			store := New(nil)
			if err := store.ValidatePreparedGitDir(context.Background(), cfg); err == nil {
				t.Fatal("expected malformed credentialed origin rejection")
			}
		})
	}
}

func TestValidatePreparedGitDirAllowsAtInHTTPSOriginPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")
	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "https://git.example.com/team/repo@2026.git")

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
	store := New(nil)
	if err := store.ValidatePreparedGitDir(context.Background(), cfg); err != nil {
		t.Fatalf("ValidatePreparedGitDir: %v", err)
	}
}

func TestCredentialEnvKeepsSecretsOutOfHelperCommand(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("https://user:p@ss'word@github.com/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL == "" {
		t.Fatal("expected non-empty safe URL")
	}
	if strings.Contains(safeURL, "p@ss") {
		t.Fatalf("safe URL should not contain password: %s", safeURL)
	}
	foundHelper := false
	foundReset := false
	foundScope := false
	foundPassword := false
	for _, e := range env {
		if e == "GIT_CONFIG_VALUE_0=" {
			foundReset = true
		}
		if e == "GIT_CONFIG_KEY_1=credential.https://github.com.helper" {
			foundScope = true
		}
		if val, ok := strings.CutPrefix(e, "GIT_CONFIG_VALUE_1="); ok {
			foundHelper = true
			if strings.Contains(val, "p@ss'word") {
				t.Fatalf("password leaked in helper command: %s", val)
			}
		}
		if e == "ARTIFACT_FS_GIT_PASSWORD=p@ss'word" {
			foundPassword = true
		}
	}
	if !foundReset {
		t.Fatal("expected empty credential.helper reset")
	}
	if !foundHelper {
		t.Fatal("expected GIT_CONFIG_VALUE_1 in env")
	}
	if !foundScope {
		t.Fatalf("expected URL-scoped credential helper, got %v", env)
	}
	if !foundPassword {
		t.Fatalf("expected password env var, got %v", env)
	}
}

func TestCredentialEnvScopesCredentialsToOriginalAuthority(t *testing.T) {
	_, credentialEnvVars, err := credentialEnv("https://alice:secret@example.com:8443/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	fill := func(host string) (string, error) {
		cmd := exec.Command("git", "credential", "fill")
		cmd.Env = append(os.Environ(), credentialEnvVars...)
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		out, err := cmd.Output()
		return string(out), err
	}

	original, err := fill("example.com:8443")
	if err != nil {
		t.Fatalf("credential fill for original authority: %v", err)
	}
	if !strings.Contains(original, "username=alice") || !strings.Contains(original, "password=secret") {
		t.Fatalf("original authority did not receive credentials: %q", original)
	}
	different, _ := fill("other.example.com:8443")
	if strings.Contains(different, "username=") || strings.Contains(different, "password=") {
		t.Fatalf("different authority received credentials: %q", different)
	}
}

func TestCredentialEnvRejectsCredentialedHTTPWithoutAuthority(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("https://alice:secret@/attacker/repo.git"); err == nil {
		t.Fatal("expected missing HTTP authority rejection")
	}
}

func TestCredentialEnvAllowsFileURLPathWithAtSign(t *testing.T) {
	t.Parallel()
	const remote = "file:///tmp/repo@2026.git"
	safeURL, env, err := credentialEnv(remote)
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != remote {
		t.Fatalf("safe URL = %q, want %q", safeURL, remote)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env vars, got %v", env)
	}
}

func TestCredentialEnvAllowsSCPStyleRootPathWithAtSign(t *testing.T) {
	t.Parallel()
	const remote = "git@example.com:repo:v1@2026.git"
	safeURL, env, err := credentialEnv(remote)
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != remote {
		t.Fatalf("safe URL = %q, want %q", safeURL, remote)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env vars, got %v", env)
	}
}

func TestCredentialEnvRejectsQueryAndFragment(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://github.com/org/repo.git?access_token=secret",
		"https://github.com/org/repo.git#access_token=secret",
		"https://github.com/org/repo.git#",
		"git@github.com:org/repo.git?access_token=secret",
		"git@github.com:org/repo.git#access_token=secret",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := credentialEnv(raw); err == nil {
				t.Fatal("expected query or fragment rejection")
			}
		})
	}
}

func TestCredentialEnvTokenAsUsername(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("https://ghp_abc123@github.com/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if strings.Contains(safeURL, "ghp_abc123") {
		t.Fatalf("token should be stripped from safe URL: %s", safeURL)
	}
	if len(env) == 0 {
		t.Fatal("expected credential helper env vars")
	}
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_CONFIG_VALUE_1=") && strings.Contains(e, "ghp_abc123") {
			t.Fatalf("credential helper command leaked token: %s", e)
		}
	}
}

func TestCredentialEnvPreservesSSHUsername(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("ssh://git@github.com/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != "ssh://git@github.com/org/repo.git" {
		t.Fatalf("safe URL = %q, want SSH username preserved", safeURL)
	}
	if len(env) != 0 {
		t.Fatalf("expected no credential helper env for SSH username, got %v", env)
	}
}

func TestCredentialEnvRejectsGitProtocolUsername(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("git://ghp_secret@github.com/org/repo.git"); err == nil {
		t.Fatal("expected git protocol username rejection")
	}
}

func TestCredentialEnvRejectsSSHTokenUsername(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("ssh://ghp_abcdefghijklmnopqrstuvwxyz@github.com/org/repo.git"); err == nil {
		t.Fatal("expected SSH token username rejection")
	}
}

func TestCredentialEnvRejectsSSHPassword(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("ssh://git:secret@github.com/org/repo.git"); err == nil {
		t.Fatal("expected SSH password rejection")
	}
}

func TestCredentialEnvRejectsMalformedSSHPassword(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"ssh:/git:secret@github.com/org/repo.git",
		"ssh:/git:bad%zz@github.com/org/repo.git",
		"alice:ghp_secret@github.com:org/repo.git",
		"x-token-auth:secret@bitbucket.org/org/repo.git",
		"https://ghp_secret/part@example.com/org/repo.git",
		"https://ghp_secret/part@example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := credentialEnv(raw); err == nil {
				t.Fatal("expected malformed SSH password rejection")
			}
		})
	}
}

func TestCloneBloblessRejectsMalformedCredentialURLBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https://user:bad%zz@example.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "bad%zz") || strings.Contains(err.Error(), "user") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsMalformedHTTPSUserinfoBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https:/user:ghp_secret@github.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "ghp_secret") || strings.Contains(err.Error(), "user") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsMalformedHTTPParseErrorBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https//ghp_secret%zz@github.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "ghp_secret") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsMalformedGitStyleCredentialBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "git:secret@github.com:org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsPathSplitHTTPCredentialsBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https://user:123/ss@example.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "123") || strings.Contains(err.Error(), "ss") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCredentialEnvRejectsHTTPSLikeUserinfoTypos(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https:/user:ghp_secret@github.com/org/repo.git",
		"https//ghp_secret@github.com/org/repo.git",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := credentialEnv(raw); err == nil {
				t.Fatal("expected malformed HTTP-like remote rejection")
			}
		})
	}
}

func TestCredentialEnvAllowsAtInHTTPSPath(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("https://git.example.com/team/repo:v1@2026.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != "https://git.example.com/team/repo:v1@2026.git" {
		t.Fatalf("safe URL = %q", safeURL)
	}
	if len(env) != 0 {
		t.Fatalf("expected no credential helper env, got %v", env)
	}
}

func TestNonInteractiveGitEnvForcesSSHBatchMode(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -o BatchMode=no -i /secrets/deploy_key -o IdentitiesOnly=yes")
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "-i /secrets/deploy_key") {
				t.Fatalf("expected existing identity option to be preserved, got %q", e)
			}
			if strings.Contains(e, "BatchMode=no") {
				t.Fatalf("expected existing BatchMode option to be replaced, got %q", e)
			}
			if strings.Contains(e, "BatchMode=yes") {
				return
			}
			break
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvDefaultSSHBatchMode(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "")
	env := nonInteractiveGitEnv()
	if slices.Contains(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		return
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvStripsQuotedBatchMode(t *testing.T) {
	for _, command := range []string{
		`ssh -o "BatchMode=no" -i /secrets/deploy_key`,
		`ssh -o BatchMode="no" -i /secrets/deploy_key`,
		`ssh -o "BatchMode"=no -i /secrets/deploy_key`,
		`ssh -o 'BatchMode no' -i /secrets/deploy_key`,
		`ssh '-o' 'BatchMode=no' -i /secrets/deploy_key`,
		`ssh -oBatchMode="no" -i /secrets/deploy_key`,
	} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("GIT_SSH_COMMAND", command)
			env := nonInteractiveGitEnv()
			for _, e := range env {
				if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
					if strings.Contains(e, "BatchMode=no") || strings.Contains(e, `BatchMode="no"`) {
						t.Fatalf("expected quoted BatchMode option to be replaced, got %q", e)
					}
					if !strings.Contains(e, "-i /secrets/deploy_key") || !strings.Contains(e, "BatchMode=yes") {
						t.Fatalf("expected identity and BatchMode=yes, got %q", e)
					}
					return
				}
			}
			t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
		})
	}
}

func TestNonInteractiveGitEnvPreservesProxyCommand(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -o ProxyCommand='ssh -o BatchMode=no bastion' -i /secrets/deploy_key")
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "ProxyCommand=ssh -o BatchMode=no bastion") {
				t.Fatalf("expected ProxyCommand to be preserved, got %q", e)
			}
			if !strings.Contains(e, "BatchMode=yes") {
				t.Fatalf("expected top-level BatchMode=yes, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvQuotesShellMetacharacters(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", `ssh -i /tmp/key\ prod -o UserKnownHostsFile=/tmp/known\ hosts`)
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "'/tmp/key prod'") || !strings.Contains(e, "'UserKnownHostsFile=/tmp/known hosts'") {
				t.Fatalf("expected escaped shell paths to be quoted, got %q", e)
			}
			if !strings.Contains(e, "BatchMode=yes") {
				t.Fatalf("expected BatchMode=yes, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvPreservesShellExpansion(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", `ssh -i "$HOME/.ssh/deploy key"`)
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "$HOME/.ssh/deploy key") {
				t.Fatalf("expected HOME expansion to be preserved, got %q", e)
			}
			if strings.Contains(e, "'$HOME/.ssh/deploy key'") {
				t.Fatalf("expected HOME expansion not to be single-quoted, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvPreservesEscapedDollar(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", `ssh -i '/tmp/key$prod dir'`)
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, `/tmp/key\$prod dir`) {
				t.Fatalf("expected escaped dollar to be preserved, got %q", e)
			}
			if strings.Contains(e, `/tmp/key$prod dir`) {
				t.Fatalf("expected literal dollar not to become expandable, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}
