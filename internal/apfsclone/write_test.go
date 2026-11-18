package apfsclone

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitForFile waits until path exists, and fails t when returned delivers
// first.
func waitForFile(t *testing.T, path string, returned <-chan error) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-returned:
			t.Fatalf("returned %v before %s appeared", err, filepath.Base(path))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// requireNoGitLock fails t when gitDir holds a lock file that a git left,
// or a worktree that git worktree add still locks as initializing.
func requireNoGitLock(t *testing.T, gitDir string) {
	t.Helper()
	err := filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if (strings.HasSuffix(name, ".lock") && name != RegistrationLock) || (name == "locked" && filepath.Base(filepath.Dir(filepath.Dir(path))) == "worktrees") {
			t.Errorf("git left %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A cancel that lands while Register's git worktree add or read-tree runs,
// or while Add's update-index --refresh runs, does not stop the git. Git
// creates a lock file, such as a ref's lock or index.lock, before it installs
// the handler that removes its lock files on SIGTERM, so a SIGTERM in between
// would leave the lock in the user's repository. The fake git holds the write
// until its context has ended, and marks a SIGTERM. It is serial: it sets
// PATH, writeGrace and writeOutlived.
func TestACanceledRegistrationWriteRunsToItsEnd(t *testing.T) {
	for _, step := range []string{"worktree add", "read-tree", "update-index"} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t, nil, nil)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			started, released, termed := filepath.Join(dir, "started"), filepath.Join(dir, "released"), filepath.Join(dir, "termed")
			write(t, filepath.Join(dir, "bin/git"), "#!/bin/sh\ncase \" $* \" in *\" "+step+" \"*)\n"+
				"  trap ': > \"$AFS_TERMED\"; exit 143' TERM\n  : > \"$AFS_STARTED\"\n  i=0\n"+
				"  while [ ! -e \"$AFS_RELEASED\" ] && [ \"$i\" -lt 6000 ]; do sleep 0.01; i=$((i+1)); done\n  ;;\nesac\n"+
				"exec "+shellQuote(realGit)+" \"$@\"\n", 0o755)
			t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AFS_STARTED", started)
			t.Setenv("AFS_RELEASED", released)
			t.Setenv("AFS_TERMED", termed)
			// The grace outlasts the test, so a SIGTERM can only come from the
			// cancel.
			outlived := make(chan struct{})
			var once sync.Once
			grace, hook := writeGrace, writeOutlived
			writeGrace, writeOutlived = time.Hour, func() { once.Do(func() { close(outlived) }) }
			t.Cleanup(func() { writeGrace, writeOutlived = grace, hook })

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			path := filepath.Join(f.worktrees, "held")
			returned := make(chan error, 1)
			o := Options{Base: f.base, Path: path, Commit: "upstream/main", AddArgs: []string{"-q", "-b", "feat/held"}, Clone: copyClone}
			entry := "Register"
			if step == "update-index" {
				entry = "Add"
			}
			go func() {
				var err error
				if entry == "Add" {
					_, err = Add(ctx, o)
				} else {
					_, err = Register(ctx, o)
				}
				returned <- err
			}()
			waitForFile(t, started, returned)
			cancel()
			select {
			case <-outlived:
			case err := <-returned:
				t.Fatalf("%s returned %v while its git %s waited, want the git to outlive the cancel", entry, err, step)
			}
			if _, err := os.Stat(termed); err == nil {
				t.Fatalf("the cancel sent the running git %s SIGTERM", step)
			}
			write(t, released, "", 0o600)
			err = <-returned
			switch step {
			case "worktree add":
				// The rev-parse after git worktree add starts no git once the
				// context ended.
				var incomplete *IncompleteError
				if !errors.As(err, &incomplete) || !errors.Is(err, context.Canceled) {
					t.Fatalf("Register = %v, want an IncompleteError of the cancel", err)
				}
				if got := run(t, f.base, "rev-parse", "--verify", "feat/held"); got != run(t, f.base, "rev-parse", "upstream/main") {
					t.Fatalf("feat/held = %s, want upstream/main", got)
				}
			case "read-tree":
				if err != nil {
					t.Fatalf("Register = %v, want the read-tree that ran to succeed", err)
				}
				if got := run(t, path, "ls-files", "README.md"); got != "README.md" {
					t.Fatalf("the index of %s lists %q, want README.md", path, got)
				}
			case "update-index":
				// The git diff-files after the refresh starts no git once the
				// context ended.
				var incomplete *IncompleteError
				if !errors.As(err, &incomplete) || !errors.Is(err, context.Canceled) {
					t.Fatalf("Add = %v, want an IncompleteError of the cancel", err)
				}
				// The refresh recorded the stat data of every file that
				// matches its blob. hidden.txt holds the base's edit that git
				// status misses, so it alone still differs.
				if got := run(t, path, "diff-files", "--name-only"); got != "hidden.txt" {
					t.Fatalf("git diff-files in %s lists %q, want only hidden.txt", path, got)
				}
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := run(t, f.base, "worktree", "list", "--porcelain"); !strings.Contains(got, "worktree "+resolved+"\n") {
				t.Fatalf("git worktree list = %q, want %s", got, resolved)
			}
			requireNoGitLock(t, filepath.Join(f.base, ".git"))
		})
	}
}
