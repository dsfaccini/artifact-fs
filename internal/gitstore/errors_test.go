package gitstore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestBoundedGitErrorPreservesClassificationAfterTruncation(t *testing.T) {
	var stderr boundedGitError
	stderr.limit = 16
	_, _ = stderr.Write([]byte(strings.Repeat("x", 32)))
	_, _ = stderr.Write([]byte(" HTTP 503 Service Unavailable"))
	if !stderr.Retryable() {
		t.Fatal("bounded stderr lost transient classification after truncation")
	}
	if got := stderr.String(); len(got) > stderr.limit+len(gitErrorTruncatedLabel)+1 || !strings.Contains(got, gitErrorTruncatedLabel) {
		t.Fatalf("bounded stderr = %q, want capped output with truncation label", got)
	}

	var permanent boundedGitError
	permanent.limit = 16
	_, _ = permanent.Write([]byte("HTTP 503"))
	_, _ = permanent.Write([]byte(" permission denied"))
	if permanent.Retryable() {
		t.Fatal("local permanent failure was classified as retryable")
	}
}

func TestRedactTruncatedCredentialPrefix(t *testing.T) {
	credential := "super-secret-token"
	message := "fatal: authentication failed: super-secret-tok\n" + gitErrorTruncatedLabel
	got := redactTruncatedCredentialPrefix(message, []string{credential})
	if strings.Contains(got, "super-secret") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("truncated credential prefix leaked in %q", got)
	}
}

func TestBuildTreeIndexDropsPartialTokenAtLazyFetchStderrBound(t *testing.T) {
	for _, remote := range []string{
		"https://alice:s3cretpassword@example.invalid/repo.git",
		"ssh://alice:s3cretpassword@example.invalid/repo.git",
		"gcrypt::rsync://alice:s3cretpassword@example.invalid/repo.git",
	} {
		fakeGitStderrBoundInsidePassword(t, remote)
		_, err := New(nil).BuildTreeIndex(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", GitDir: t.TempDir()}}, strings.Repeat("a", 40))
		if err == nil || strings.Contains(err.Error(), "s3cr") {
			t.Errorf("BuildTreeIndex error for %s = ...%q, want a failure without the partial password", remote, errorTail(err))
		}
	}
}

func TestResolveHEADDropsPartialTokenAtStderrBound(t *testing.T) {
	fakeGitStderrBoundInsidePassword(t, "https://alice:s3cretpassword@example.invalid/repo.git")
	_, _, err := New(nil).ResolveHEAD(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", GitDir: t.TempDir()}})
	if err == nil || strings.Contains(err.Error(), "s3cr") {
		t.Errorf("ResolveHEAD error = ...%q, want a failure without the partial password", errorTail(err))
	}
}

// fakeGitStderrBoundInsidePassword puts a git on PATH that fails after
// printing remote to stderr, with the stderr bound falling just after the
// password's "s3cr".
func fakeGitStderrBoundInsidePassword(t *testing.T, remote string) {
	t.Helper()
	lead := remote[:strings.Index(remote, "s3cr")+len("s3cr")]
	pad := maxGitErrorBytes - len("fatal: ") - len(" ") - len(lead)
	script := gittest.FakeGitScript("printf 'fatal: ' >&2\nawk 'BEGIN { for (i = 0; i < " + strconv.Itoa(pad) + "; i++) printf \"x\" }' >&2\nprintf ' %s\\n' '" + remote + "' >&2\nexit 128\n")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func errorTail(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	return msg[max(0, len(msg)-120):]
}
