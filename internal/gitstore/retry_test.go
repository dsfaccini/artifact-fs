package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type stagedErrorContext struct {
	context.Context
	errAt int
	calls int
}

func (c *stagedErrorContext) Err() error {
	c.calls++
	if c.calls >= c.errAt {
		return context.Canceled
	}
	return nil
}

func TestCloneBloblessRetriesTransientTransportErrors(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "clone-count")
	commandPath := filepath.Join(tmp, "git-commands")
	fakeGit := filepath.Join(bin, "git")
	fakeGitScript := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$AFS_GIT_COMMANDS\"\n" +
		"case \"$1\" in\n" +
		"  clone)\n" +
		"    count=0; [ -f \"$AFS_CLONE_COUNT\" ] && count=$(cat \"$AFS_CLONE_COUNT\")\n" +
		"    count=$((count + 1)); printf '%s' \"$count\" > \"$AFS_CLONE_COUNT\"\n" +
		"    if [ \"$count\" -lt 3 ]; then\n" +
		"      printf '%s\\n' 'error: RPC failed; HTTP 503 curl 22' 'fetch-pack: unexpected disconnect while reading sideband packet' 'fatal: early EOF' 'fatal: index-pack failed' >&2\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    for arg in \"$@\"; do dest=\"$arg\"; done\n" +
		"    mkdir -p \"$dest/.git\"\n" +
		"    ;;\n" +
		"  read-tree) ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeGit, []byte(fakeGitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_COUNT", countPath)
	t.Setenv("AFS_GIT_COMMANDS", commandPath)

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = []time.Duration{0, 0}
	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:   "https://log-user:log-secret@example.com/openai-sites",
			GitDir: filepath.Join(tmp, "repo.git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://user:super-secret@example.com/org/repo.git",
			Branch:    "main",
		},
	}
	if err := store.CloneBlobless(context.Background(), cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(count); got != "3" {
		t.Fatalf("clone attempts = %q, want 3", got)
	}
	commands, err := os.ReadFile(commandPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "super-secret") || strings.Contains(string(commands), "user@example.com") {
		t.Fatalf("credential appeared in git command: %s", commands)
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, `"msg":"`+logGitOperationAttemptFailed+`"`) ||
		!strings.Contains(logOutput, `"msg":"`+logGitOperationRecovered+`"`) ||
		!strings.Contains(logOutput, `"error":"error: RPC failed; HTTP 503 curl 22`) ||
		strings.Contains(logOutput, "super-secret") || strings.Contains(logOutput, "log-secret") {
		t.Fatalf("clone retry logs lack the failure or show a credential: %s", logs.String())
	}
}

func TestCloneBloblessDoesNotRetryPermanentError(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "clone-count")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\ncount=0; [ -f \"$AFS_CLONE_COUNT\" ] && count=$(cat \"$AFS_CLONE_COUNT\")\ncount=$((count + 1)); printf '%s' \"$count\" > \"$AFS_CLONE_COUNT\"\nprintf '%s\\n' 'remote: credential-user' 'remote: super-secret' 'fatal: Authentication failed for https://credential-user:super-secret@example.com/org/repo.git; HTTP 401' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_COUNT", countPath)

	store := New(nil)
	store.gitRetryDelays = []time.Duration{0, 0}
	err := store.CloneBlobless(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{GitDir: filepath.Join(tmp, "repo.git")}, RepoSpec: model.RepoSpec{RemoteURL: "https://credential-user:super-secret@example.com/org/repo.git", Branch: "main"}})
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) {
		t.Fatalf("error = %T %v, want GitOperationError", err, err)
	}
	if operationErr.Operation != GitOperationClone || operationErr.Attempts != 1 || operationErr.Retryable {
		t.Fatalf("GitOperationError = %+v, want one non-retryable clone attempt", operationErr)
	}
	if strings.Contains(err.Error(), "credential-user") || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("clone error leaked credentials: %v", err)
	}
	count, readErr := os.ReadFile(countPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(count) != "1" {
		t.Fatalf("clone attempts = %q, want 1", count)
	}
}

func TestCloneBloblessPreservesTransportFailureAtCallerDeadline(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "clone-count")
	stderrWritten := filepath.Join(tmp, "stderr-written")
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"count=0; [ -f \"$AFS_CLONE_COUNT\" ] && count=$(cat \"$AFS_CLONE_COUNT\")\n" +
		"count=$((count + 1)); printf '%s' \"$count\" > \"$AFS_CLONE_COUNT\"\n" +
		"printf '%s\\n' 'error: RPC failed; HTTP 500 curl 22' >&2\n" +
		": > \"$AFS_STDERR_WRITTEN\"\n" +
		"exec sleep 10\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_COUNT", countPath)
	t.Setenv("AFS_STDERR_WRITTEN", stderrWritten)

	ctx, cancel := gittest.ExpireOnFile(context.Background(), stderrWritten)
	defer cancel()
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	err := store.CloneBlobless(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{GitDir: filepath.Join(tmp, "repo.git")}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) {
		t.Fatalf("error = %T %v, want GitOperationError", err, err)
	}
	if operationErr.Attempts != 1 || !operationErr.Retryable {
		t.Fatalf("GitOperationError = %+v, want one retryable attempt", operationErr)
	}
	if !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "signal:") {
		t.Fatalf("error did not safely preserve transport failure: %v", err)
	}
	count, readErr := os.ReadFile(countPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(count) != "1" {
		t.Fatalf("clone attempts = %q, want 1", count)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationAttemptFailed+`"`) ||
		!strings.Contains(logOutput, `"timed_out":true`) ||
		!strings.Contains(logOutput, `"error":"error: RPC failed; HTTP 500 curl 22`) {
		t.Fatalf("deadline failure was not logged with its transport cause: %s", logOutput)
	}
}

func TestRetryGitOperationCancellationRacePreservesBothCauses(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	transportErr := errors.New("HTTP 500 transport failure")
	attempts := 0
	err := New(nil).retryGitOperationForRemotes(ctx, GitOperationClone, "repo", nil, func() error {
		attempts++
		cancel()
		return transportErr
	})
	if !errors.Is(err, transportErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want transport and cancellation causes", err)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || !operationErr.Retryable || operationErr.Attempts != 1 {
		t.Fatalf("error = %#v, want one retryable attempt", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestRetryGitOperationCancellationBeforeNextAttemptPreservesFailure(t *testing.T) {
	t.Parallel()
	ctx := &stagedErrorContext{Context: context.Background(), errAt: 3}
	transportErr := errors.New("HTTP 500 transport failure")
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = []time.Duration{0}
	attempts := 0
	err := store.retryGitOperationForRemotes(ctx, GitOperationClone, "repo", nil, func() error {
		attempts++
		return transportErr
	})
	if !errors.Is(err, transportErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want transport and cancellation causes", err)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || !operationErr.Retryable || operationErr.Attempts != 1 {
		t.Fatalf("error = %#v, want the first retryable attempt", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationInterrupted+`"`) || !strings.Contains(logOutput, `"canceled":true`) {
		t.Fatalf("next-attempt cancellation terminal log incomplete: %s", logOutput)
	}
}

func TestRetryGitOperationLogsCancellationDuringBackoff(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = []time.Duration{time.Second}
	// In a synctest bubble the cancel fires 20ms into the backoff, never after it.
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := store.retryGitOperationForRemotes(ctx, GitOperationFetch, "repo", nil, func() error {
			time.AfterFunc(20*time.Millisecond, cancel)
			return errors.New("HTTP 503 transport failure")
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want canceled", err)
		}
	})
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationInterrupted+`"`) ||
		!strings.Contains(logOutput, `"canceled":true`) || !strings.Contains(logOutput, `"attempts":1`) {
		t.Fatalf("backoff cancellation terminal log incomplete: %s", logOutput)
	}
}

// Git's stderr reaches boundedGitError in writes of any size, so a symptom
// split across two writes still classifies the failure.
func TestBoundedGitErrorClassifiesSymptomsSplitAcrossWrites(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		stderr    string
		retryable bool
	}{
		{"error: RPC failed; HTTP 408 curl 22", true},
		{"error: RPC failed; HTTP 500 curl 22", true},
		{"fatal: unable to access remote: The requested URL returned error: 522", true},
		{"fatal: unable to access remote: The requested URL returned error: 524", true},
		{"error: RPC failed; curl 18 transfer closed with 66 bytes remaining to read", true},
		{"error: RPC failed; curl 55 Send failure: Broken pipe", true},
		{"error: RPC failed; curl 56 The TLS connection was non-properly terminated", true},
		{"error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL", true},
		{"fatal: unable to access remote: Operation timed out after 60000 milliseconds", true},
		{"fatal: unable to access remote: Empty reply from server", true},
		{"fatal: unable to access remote: Could not resolve host: github.com", true},
		{"fatal: unable to access remote: Failed to connect to github.com port 443: Couldn't connect to server", true},
		{"fetch-pack: unexpected disconnect while reading sideband packet", true},
		{"fetch-pack: 12 bytes of body are still expected", true},
		{"fetch-pack: 3 bytes of length header were received", true},
		{"fatal: early EOF", true},
		// A generic index-pack summary can follow a local failure that a retry
		// cannot repair.
		{"fatal: unable to write file: Input/output error\nfatal: index-pack failed", false},
		{"error: RPC failed; HTTP 503 curl 22\nfatal: unable to write file: No space left on device", false},
		{"fatal: early EOF\nerror: unable to create temporary file: Permission denied", false},
	} {
		t.Run(tt.stderr, func(t *testing.T) {
			t.Parallel()
			for i := range len(tt.stderr) + 1 {
				stderr := &boundedGitError{limit: maxGitErrorBytes}
				_, _ = stderr.Write([]byte(tt.stderr[:i]))
				_, _ = stderr.Write([]byte(tt.stderr[i:]))
				if stderr.Retryable() != tt.retryable {
					t.Fatalf("Retryable() = %t for stderr split after byte %d, want %t", !tt.retryable, i, tt.retryable)
				}
			}
		})
	}
}

func TestRetryGitOperationBoundedExhaustion(t *testing.T) {
	t.Parallel()
	store := New(nil)
	store.gitRetryDelays = []time.Duration{0, 0}
	attempts := 0
	err := store.retryGitOperationForRemotes(context.Background(), GitOperationClone, "repo", nil, func() error {
		attempts++
		return errors.New("HTTP 500 transport failure")
	})
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || operationErr.Attempts != 3 || !operationErr.Retryable {
		t.Fatalf("error = %#v, want retryable exhaustion after three attempts", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryGitOperationRedactsExactLocalRemote(t *testing.T) {
	t.Parallel()
	const remote = "/private/customer/repo.git"
	sentinel := errors.New("sentinel failure")
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = nil
	err := store.retryGitOperationForRemotes(context.Background(), GitOperationClone, "repo", []string{remote}, func() error {
		return fmt.Errorf("HTTP 503 cloning %s: %w", remote, sentinel)
	})
	if err == nil {
		t.Fatal("expected clone failure")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want preserved sentinel cause", err)
	}
	if strings.Contains(err.Error(), remote) || !strings.Contains(err.Error(), "REDACTED_REMOTE") {
		t.Fatalf("returned error did not redact remote: %v", err)
	}
	if logOutput := logs.String(); strings.Contains(logOutput, remote) || !strings.Contains(logOutput, "REDACTED_REMOTE") {
		t.Fatalf("local remote was not redacted from retry log: %s", logOutput)
	}
}

func TestRetryGitOperationPermanentFailures(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"HTTP 429 Too Many Requests",
		"HTTP 501 Not Implemented",
		"HTTP 505 Version Not Supported",
		"fatal: Authentication failed",
		"fetch-pack failed",
		"packfile could not be read",
		"No space left on device; index-pack failed",
		"Disk quota exceeded; pack is corrupted",
		"Permission denied; early EOF",
		"Read-only file system; invalid index-pack output",
	} {
		t.Run(message, func(t *testing.T) {
			t.Parallel()
			store := New(nil)
			store.gitRetryDelays = []time.Duration{0, 0}
			attempts := 0
			err := store.retryGitOperationForRemotes(context.Background(), GitOperationClone, "repo", nil, func() error {
				attempts++
				return errors.New(message)
			})
			var operationErr *GitOperationError
			if !errors.As(err, &operationErr) || operationErr.Retryable || operationErr.Attempts != 1 {
				t.Fatalf("error = %#v, want one permanent attempt", err)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1", attempts)
			}
		})
	}
}

func TestFetchRefAmbientRetriesTransientFailures(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "fetch-count")
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$*\" = 'remote get-url origin' ]; then exit 0; fi\n" +
		"count=0; [ -f \"$AFS_FETCH_COUNT\" ] && count=$(cat \"$AFS_FETCH_COUNT\")\n" +
		"count=$((count + 1)); printf '%s' \"$count\" > \"$AFS_FETCH_COUNT\"\n" +
		"if [ \"$count\" -lt 3 ]; then printf '%s\\n' 'HTTP 503 Service Unavailable' >&2; exit 1; fi\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_FETCH_COUNT", countPath)

	store := New(nil)
	store.gitRetryDelays = []time.Duration{0, 0}
	repo := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "prepared", GitDir: filepath.Join(tmp, "prepared.git")}, RepoSpec: model.RepoSpec{Branch: "main"}}
	if err := store.FetchRefAmbient(context.Background(), repo, "main"); err != nil {
		t.Fatalf("FetchRefAmbient: %v", err)
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "3" {
		t.Fatalf("fetch attempts = %q, want 3", count)
	}
}
