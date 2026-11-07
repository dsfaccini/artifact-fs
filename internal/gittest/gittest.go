// Package gittest holds helpers for tests that run git.
package gittest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// PartialCloneRemote commits files on branch main of a bare repository that
// serves partial clones, and returns its URL. A blobless clone of it holds no
// blobs, so every read of one fetches from it.
func PartialCloneRemote(t testing.TB, files map[string]string) string {
	t.Helper()
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	bare := filepath.Join(tmp, "remote.git")
	run(t, "", "init", "-q", "-b", "main", work)
	for name, content := range files {
		path := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, work, "add", ".")
	run(t, work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "files")
	run(t, "", "init", "-q", "--bare", "-b", "main", bare)
	run(t, bare, "config", "uploadpack.allowFilter", "true")
	run(t, work, "push", "-q", bare, "main")
	return "file://" + bare
}

func run(t testing.TB, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// IsolateConfig makes git, in this process and every process it starts, read
// neither the user's global config nor the system config, so tests see git's
// defaults on every machine. Call it from TestMain.
func IsolateConfig() {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// RequireConfigIsolated fails t unless git ignores a global and a system config
// planted for it, which holds only after IsolateConfig ran.
func RequireConfigIsolated(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[gittest]\n\tscope = global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	system := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(system, []byte("[gittest]\n\tscope = system\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	out, err := exec.Command("git", "config", "--get-all", "gittest.scope").Output()
	if len(out) != 0 || err == nil {
		t.Fatalf("git read gittest.scope = %q from a machine config; TestMain must call gittest.IsolateConfig", out)
	}
}

// ExpireOnFile returns a context whose deadline passes once path exists, so a
// test chooses the step a deadline interrupts instead of racing a timer. The
// context reports a deadline, as a real timeout does, and ends early when
// parent ends or cancel is called.
func ExpireOnFile(parent context.Context, path string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, time.Hour)
	expiring := &expiringContext{Context: ctx, done: make(chan struct{})}
	go func() {
		defer close(expiring.done)
		for {
			if _, err := os.Stat(path); err == nil {
				expiring.expired = true
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return expiring, cancel
}

type expiringContext struct {
	context.Context
	done chan struct{}
	// expired is written before done closes and read only after.
	expired bool
}

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	if c.expired {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// IsolatePrompts keeps the machine's prompt programs and SSH settings out of
// a test's Git commands; IsolateConfig already isolates Git config. It sets
// SSH_ASKPASS, which Git falls back to for its own prompts, to a program that
// records each run in the returned file.
func IsolatePrompts(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"GIT_ASKPASS", "GIT_SSH", "GIT_SSH_COMMAND", "SSH_ASKPASS_REQUIRE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	dir := t.TempDir()
	runs := filepath.Join(dir, "askpass-runs")
	askpass := filepath.Join(dir, "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho \"$*\" >> '"+runs+"'\necho guessed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_ASKPASS", askpass)
	return runs
}

// RequireNoPrompt fails t if the program IsolatePrompts set ran.
func RequireNoPrompt(t testing.TB, runs string) {
	t.Helper()
	if data, err := os.ReadFile(runs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git ran SSH_ASKPASS: %q, %v", data, err)
	}
}

// UnauthorizedRemote returns an HTTP remote that answers every request with 401.
func UnauthorizedRemote(t testing.TB) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/repo.git"
}
