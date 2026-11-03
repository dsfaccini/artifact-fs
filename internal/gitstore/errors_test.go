package gitstore

import (
	"strings"
	"testing"
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
