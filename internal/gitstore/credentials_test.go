package gitstore

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

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
		RepoPaths: model.RepoPaths{
			Name:   "repo",
			GitDir: filepath.Join(tmp, "repo.git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://alice:super-secret@example.com/org/repo.git",
			Branch:    "main",
		},
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
	if !strings.Contains(string(commandData), "remote set-url -- origin https://example.com/org/repo.git") {
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

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: preparedGitDir}, RepoSpec: model.RepoSpec{Branch: "master"}}
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

			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: preparedGitDir}, RepoSpec: model.RepoSpec{Branch: "master"}}
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

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: preparedGitDir}, RepoSpec: model.RepoSpec{Branch: "master"}}
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

func TestCredentialEnvRejectsUserinfoBeforeLaterSeparator(t *testing.T) {
	t.Parallel()
	if safeURL, _, err := credentialEnv("alice:s3cret@host/a://b@c"); err == nil {
		t.Fatalf("credentialEnv passed %q to git", safeURL)
	}
}

func TestCredentialEnvRejectsUsernameBehindHTTPSRemoteHelper(t *testing.T) {
	t.Parallel()
	if safeURL, _, err := credentialEnv("gcrypt::https://ghp_token@github.com/org/repo.git"); err == nil {
		t.Fatalf("credentialEnv passed %q to git", safeURL)
	}
	// Behind a helper address that is not http(s), a username alone names an ambient profile.
	const profile = "codecommit::us-east-1://demo-profile@MyRepositoryName"
	if safeURL, _, err := credentialEnv(profile); err != nil || safeURL != profile {
		t.Fatalf("credentialEnv(%q) = %q, %v; want it unchanged", profile, safeURL, err)
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
		RepoPaths: model.RepoPaths{
			GitDir: filepath.Join(tmp, "repo.git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://user:bad%zz@example.com/org/repo.git",
			Branch:    "main",
		},
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

func TestCloneBloblessRejectsOptionRemoteBeforeGit(t *testing.T) {
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

	// A ref outside refs/heads/ takes the ls-remote path, where git runs an
	// injected --upload-pack value.
	for _, branch := range []string{"main", "refs/tags/v1"} {
		t.Run(branch, func(t *testing.T) {
			cfg := model.RepoConfig{
				RepoPaths: model.RepoPaths{
					GitDir: filepath.Join(tmp, "repo.git"),
				},
				RepoSpec: model.RepoSpec{
					RemoteURL: "--upload-pack=a@b:c",
					Branch:    branch,
				},
			}
			err := New(nil).CloneBlobless(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
				t.Fatalf("CloneBlobless error = %v, want leading-dash rejection", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("git was invoked before rejecting the remote")
			}
		})
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
		RepoPaths: model.RepoPaths{
			GitDir: filepath.Join(tmp, "repo.git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https:/user:ghp_secret@github.com/org/repo.git",
			Branch:    "main",
		},
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
		RepoPaths: model.RepoPaths{
			GitDir: filepath.Join(tmp, "repo.git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https//ghp_secret%zz@github.com/org/repo.git",
			Branch:    "main",
		},
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
		RepoPaths: model.RepoPaths{
			GitDir: filepath.Join(tmp, "repo.git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "git:secret@github.com:org/repo.git",
			Branch:    "main",
		},
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
		RepoPaths: model.RepoPaths{
			GitDir: filepath.Join(tmp, "repo.git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://user:123/ss@example.com/org/repo.git",
			Branch:    "main",
		},
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

func TestCloneBloblessAcceptsAtSignInPathAfterHostAndPort(t *testing.T) {
	commit, _ := serveSSHFromLocalRepo(t)
	for _, remote := range []string{
		"ssh://host.example.com:22/org/repo@v1.git",
		"ssh://git@host.example.com:22/org/repo@v1.git",
		"ssh://git@bücher.example:22/org/repo@v1.git",
		"ssh://git@[fe80::1%25en0]/org/repo@v1.git",
		"ssh://[fe80::1%25en0]:22/org/repo@v1.git",
	} {
		gitDir := filepath.Join(t.TempDir(), "repo.git")
		if err := New(nil).CloneBlobless(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}}); err != nil {
			t.Fatalf("CloneBlobless(%q): %v", remote, err)
		}
		if got := strings.TrimSpace(runOutput(t, "git", "--git-dir", gitDir, "rev-parse", "HEAD")); got != commit {
			t.Fatalf("clone of %q HEAD = %s, want %s", remote, got, commit)
		}
	}
}

func TestCredentialEnvAcceptsAtSignInPathAfterIDNOrZonedHost(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"https://tok@bücher.example:8443/org/repo@v1.git",
		"https://tok@[fe80::1%25en0]:8443/org/repo@v1.git",
		"https://[fe80::1%25en0]/org/repo@v1.git",
		"git://[fe80::1%25en0]/org/repo@v1.git",
	} {
		safeURL, _, err := credentialEnv(remote)
		if err != nil {
			t.Errorf("credentialEnv(%q): %v", remote, err)
		}
		if strings.Contains(safeURL, "tok") {
			t.Errorf("credentialEnv(%q) passes %q to git", remote, safeURL)
		}
	}
}

func TestCloneBloblessRejectsPasswordHoldingSlashBeforeGit(t *testing.T) {
	_, contacted := serveSSHFromLocalRepo(t)
	// url.Parse reads host "user" and port "1234", but no real host precedes
	// the "/", so "user:1234/secret" is userinfo.
	const remote = "ssh://user:1234/secret@host/repo"
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := New(nil).CloneBlobless(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}}); err == nil {
		t.Fatalf("CloneBlobless(%q) succeeded", remote)
	}
	if _, err := os.Stat(contacted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git ran SSH for %q, which holds a password", remote)
	}
}

func TestCredentialChecksAgreeOnMisparsedCredentials(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ remote, password string }{
		// url.Parse reads host "user" and port "1234", and a path holding the "@".
		{"ssh://user:1234/secret@host/repo", "1234/secret"},
		{"git://alice:22/s3cret@host", "22/s3cret"},
		{"ssh://hostzz:22/org/repo@v1.git", "22/org/repo"},
		// The userinfo url.Parse reads ends early, and the URL left without it
		// still holds credentials.
		{"https://alice:p@ss/w0rd@github.com/org/repo.git", "ss/w0rd"},
		{"ssh://git@host.example.com/a://u2:pw2zq@h2zz/r", "pw2zq"},
		{"https://user:tok@localhost/a://u2:pw2zq@h2zz/r", "pw2zq"},
		{"ssh://git@gitserver:2222/org/repo@v1.git", "gitserver:2222/org/repo"},
		{"https://user:tok@bücher.example/a://u2:pw2zq@h2zz/r", "pw2zq"},
		{"ssh://git@[fe80::1%25en0]/a://u2:pw2zq@h2zz/r", "pw2zq"},
	} {
		if _, _, err := credentialEnv(tt.remote); err == nil {
			t.Errorf("credentialEnv accepted %q", tt.remote)
		}
		if u, err := url.Parse(tt.remote); err == nil && u.User != nil {
			u.User = nil
			if _, _, err := credentialEnv(u.String()); err == nil {
				t.Errorf("credentialEnv accepts %q without its userinfo, so it is not the same check", tt.remote)
			}
		}
		if !auth.HasInlineCredentials(tt.remote) {
			t.Errorf("HasInlineCredentials(%q) = false", tt.remote)
		}
		if got := auth.RedactRemoteURL(tt.remote); strings.Contains(got, tt.password) {
			t.Errorf("RedactRemoteURL(%q) = %q shows %q", tt.remote, got, tt.password)
		}
	}
}

func TestCloneBloblessReportsServerStderrWithCaseFoldingText(t *testing.T) {
	// "Ⱥ" is 2 bytes and lowercases to 3, so an index found in lowercased text
	// overruns the original. A server controls what its transport prints.
	tmp := t.TempDir()
	ssh := filepath.Join(tmp, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' 'denied x:ȺȺȺȺ://a@b here' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", ssh)
	t.Setenv("GIT_SSH_VARIANT", "ssh")
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: filepath.Join(tmp, "repo.git")}, RepoSpec: model.RepoSpec{RemoteURL: "ssh://host.example.com/org/repo.git", Branch: "main"}}
	err := New(nil).CloneBlobless(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("CloneBlobless error = %v, want the server's message", err)
	}
}

// serveSSHFromLocalRepo makes Git's SSH transport serve every remote from a new
// one-commit repository, whatever the URL's host, port or path. It returns the
// commit and a file the transport creates when Git runs it.
func serveSSHFromLocalRepo(t *testing.T) (commit, contacted string) {
	t.Helper()
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	run(t, "git", "init", "--initial-branch", "main", src)
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", src, "add", "file.txt")
	run(t, "git", "-C", src, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, "git", "-C", src, "config", "uploadpack.allowFilter", "true")
	contacted = filepath.Join(tmp, "contacted")
	ssh := filepath.Join(tmp, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\n: > '"+contacted+"'\nexec git-upload-pack '"+src+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", ssh)
	t.Setenv("GIT_SSH_VARIANT", "ssh")
	return strings.TrimSpace(runOutput(t, "git", "-C", src, "rev-parse", "HEAD")), contacted
}
