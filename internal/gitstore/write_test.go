package gitstore

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// waitForFile waits until path exists, and fails t when done delivers first.
func waitForFile(t *testing.T, path string, done <-chan error) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("returned %v before %s appeared", err, filepath.Base(path))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// newShortGraceStore is New with a write grace short enough that a test whose
// fake git hangs until it is stopped does not wait out defaultWriteGrace. Use
// it where the grace is not the test's subject.
func newShortGraceStore(logger *slog.Logger) *Store {
	s := New(logger)
	s.writeGrace = 10 * time.Millisecond
	return s
}

// A cancel that lands while a local write runs does not stop the write. Git
// creates config.lock before it installs the handler that removes the lock
// on SIGTERM, so a SIGTERM in between would leave the lock, and every later
// config write would fail. The fake git holds the status optimization's last
// config write until its context has ended, and marks a SIGTERM.
func TestACanceledLocalWriteRunsToItsEnd(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, "repo.git")
	run(t, "git", "init", "--bare", gitDir)
	started, released, termed := filepath.Join(tmp, "started"), filepath.Join(tmp, "released"), filepath.Join(tmp, "termed")
	realGit := wrapGit(t, tmp, "#!/bin/sh\ncase \" $* \" in\n*\" core.untrackedCache true \"*)\n  trap ': > \"$AFS_TERMED\"; exit 143' TERM\n  : > \"$AFS_STARTED\"\n  i=0\n  while [ ! -e \"$AFS_RELEASED\" ] && [ \"$i\" -lt 6000 ]; do sleep 0.01; i=$((i+1)); done\n  ;;\nesac\nexec \"$AFS_REAL_GIT\" \"$@\"\n")
	t.Setenv("AFS_STARTED", started)
	t.Setenv("AFS_RELEASED", released)
	t.Setenv("AFS_TERMED", termed)

	store := New(nil)
	// The grace outlasts the test, so a SIGTERM can only come from the cancel.
	store.writeGrace = time.Hour
	outlived := make(chan struct{})
	var once sync.Once
	store.testHookWriteOutlived = func() { once.Do(func() { close(outlived) }) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}}
	returned := make(chan error, 1)
	go func() { returned <- store.ConfigureStatusOptimization(ctx, cfg, filepath.Join(tmp, "state")) }()
	waitForFile(t, started, returned)
	cancel()
	select {
	case <-outlived:
	case err := <-returned:
		t.Fatalf("ConfigureStatusOptimization returned %v while its write waited, want the write to outlive the cancel", err)
	}
	if _, err := os.Stat(termed); err == nil {
		t.Fatal("the cancel sent the running config write SIGTERM")
	}
	if err := os.WriteFile(released, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-returned; err != nil {
		t.Fatalf("ConfigureStatusOptimization = %v, want the write that ran to succeed", err)
	}
	if value := strings.TrimSpace(runOutput(t, realGit, "--git-dir", gitDir, "config", "--get", "core.untrackedCache")); value != "true" {
		t.Fatalf("core.untrackedCache = %q, want true", value)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "config.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config.lock after the write: %v, want none", err)
	}
	// The package's tests do not answer the hook.
	run(t, realGit, "--git-dir", gitDir, "config", "--unset", "core.fsmonitor")
}

// A cancel stops a fetch at once: a fetch contacts a remote and can hang, so
// it is no local write. The write grace outlasts the test, so a fetch that
// outlived the cancel would run until the fake's sleep ends.
func TestACanceledFetchStops(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	run(t, "git", "init", "--initial-branch", "main", remote)
	run(t, "git", "-C", remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")
	clone := filepath.Join(tmp, "clone")
	run(t, "git", "clone", "file://"+remote, clone)
	started, finished := filepath.Join(tmp, "started"), filepath.Join(tmp, "finished")
	wrapGit(t, tmp, "#!/bin/sh\nif [ \"$1\" = fetch ]; then\n  : > \"$AFS_STARTED\"\n  sleep 60\n  : > \"$AFS_FINISHED\"\n  exit 1\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n")
	t.Setenv("AFS_STARTED", started)
	t.Setenv("AFS_FINISHED", finished)

	store := New(nil)
	store.writeGrace = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: filepath.Join(clone, ".git")},
		RepoSpec:  model.RepoSpec{RemoteURL: "file://" + remote, Branch: "refs/heads/main", FetchRef: "refs/heads/main"},
	}
	returned := make(chan error, 1)
	go func() { returned <- store.FetchNonInteractive(ctx, cfg) }()
	waitForFile(t, started, returned)
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchNonInteractive = %v, want context canceled", err)
	}
	if _, err := os.Stat(finished); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fetch ran to its end after the cancel: %v", err)
	}
}
