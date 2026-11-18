package slimtree

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/gittest"
)

// heldWrite is a fake git first on PATH that holds each git whose arguments
// contain hold until released exists, and marks a SIGTERM in termed. Once
// released, it runs the real git and marks finished. Its outlived closes once
// the context of a git that gitWrite runs ends. Its grace outlasts the test,
// so a SIGTERM can only come from that end.
type heldWrite struct {
	started, released, termed, finished string
	outlived                            chan struct{}
}

// holdWrite installs a heldWrite. Its test is serial: it sets PATH,
// writeGrace and writeOutlived.
func holdWrite(t *testing.T, hold string) heldWrite {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := heldWrite{started: filepath.Join(dir, "started"), released: filepath.Join(dir, "released"), termed: filepath.Join(dir, "termed"),
		finished: filepath.Join(dir, "finished"), outlived: make(chan struct{})}
	if err := execfile.Write(filepath.Join(dir, "git"), []byte("#!/bin/sh\ncase \" $* \" in *\""+hold+"\"*)\n"+
		"  trap ': > \"$AFS_TERMED\"; exit 143' TERM\n  : > \"$AFS_STARTED\"\n  i=0\n"+
		"  while [ ! -e \"$AFS_RELEASED\" ] && [ \"$i\" -lt 6000 ]; do sleep 0.01; i=$((i+1)); done\n"+
		"  "+shellQuote(realGit)+" \"$@\"; s=$?; : > \"$AFS_FINISHED\"; exit $s\n  ;;\nesac\n"+
		"exec "+shellQuote(realGit)+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_STARTED", h.started)
	t.Setenv("AFS_RELEASED", h.released)
	t.Setenv("AFS_TERMED", h.termed)
	t.Setenv("AFS_FINISHED", h.finished)
	var once sync.Once
	grace, hook := writeGrace, writeOutlived
	writeGrace, writeOutlived = time.Hour, func() { once.Do(func() { close(h.outlived) }) }
	t.Cleanup(func() { writeGrace, writeOutlived = grace, hook })
	return h
}

// waitStarted waits until the held git started, and fails t when returned
// delivers first.
func (h heldWrite) waitStarted(t *testing.T, returned <-chan int) {
	t.Helper()
	for {
		if _, err := os.Stat(h.started); err == nil {
			return
		}
		select {
		case code := <-returned:
			t.Fatalf("slimtree exited %d before the held git started", code)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitFinished waits until the released git finished, and fails t when a
// SIGTERM stopped it.
func (h heldWrite) waitFinished(t *testing.T) {
	t.Helper()
	for {
		if _, err := os.Stat(h.finished); err == nil {
			return
		}
		if _, err := os.Stat(h.termed); err == nil {
			t.Fatal("a SIGTERM stopped the released git before it finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// outlive ends the held git's context with end once the git started, checks
// that the git outlived it, and releases the git. It fails t when returned
// delivers first.
func (h heldWrite) outlive(t *testing.T, end func(), returned <-chan int) {
	t.Helper()
	h.waitStarted(t, returned)
	end()
	select {
	case <-h.outlived:
	case code := <-returned:
		t.Fatalf("slimtree exited %d while the held git waited, want the git to outlive its context", code)
	}
	if _, err := os.Stat(h.termed); err == nil {
		t.Fatal("the end of its context sent the running git SIGTERM")
	}
	if err := os.WriteFile(h.released, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireNoGitLock fails t when gitDir holds a lock file that a git left.
// apfsclone's registration lock stays by design.
func requireNoGitLock(t *testing.T, gitDir string) {
	t.Helper()
	err := filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(d.Name(), ".lock") && d.Name() != apfsclone.RegistrationLock {
			t.Errorf("git left %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A Ctrl-C that lands while rm writes the recovery ref, while it unlocks an
// artifactfs worktree, or while git worktree remove runs, does not stop the
// git. Git creates a lock file, such as the ref's lock or the index.lock of
// the git status that git worktree remove runs, before it installs the
// handler that removes its lock files on SIGTERM, so a SIGTERM in between
// would leave the lock in the user's repository. A recovery ref's lock would
// stay for good: a rerun writes a ref of another name.
func TestACanceledRmRunsItsWritesToTheirEnd(t *testing.T) {
	// apfsclone makes main-feat on feat with new. artifactfs makes the
	// stopped wt on wt.
	apfsclone := func(t *testing.T) (string, string, string, func(context.Context) int) {
		homeAfterFirstRun(t)
		cloneWith(t, nil)
		main := newRepo(t)
		path := filepath.Join(filepath.Dir(main), "main-feat")
		if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
			t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
		}
		return main, path, "feat", func(ctx context.Context) int {
			code, _, _ := slimtree(ctx, "rm", "main-feat")
			return code
		}
	}
	artifactfs := func(t *testing.T) (string, string, string, func(context.Context) int) {
		main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
		return main, r.Path, "wt", func(ctx context.Context) int {
			code, _, _ := linux(ctx, "rm", "--force", "wt")
			return code
		}
	}
	for _, tc := range []struct {
		name, hold string
		setup      func(t *testing.T) (main, path, branch string, rm func(context.Context) int)
		// removed is set when the held git is rm's last write.
		removed bool
	}{
		{"recovery ref", " update-ref refs/slimtree/", apfsclone, false},
		{"worktree remove", " worktree remove ", apfsclone, true},
		{"artifactfs recovery ref", " update-ref refs/slimtree/", artifactfs, false},
		{"artifactfs worktree unlock", " worktree unlock ", artifactfs, false},
		{"artifactfs worktree remove", " worktree remove ", artifactfs, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			main, path, branch, rm := tc.setup(t)
			head := gitIn(t, main, "rev-parse", "refs/heads/"+branch)
			h := holdWrite(t, tc.hold)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			returned := make(chan int, 1)
			go func() { returned <- rm(ctx) }()
			h.outlive(t, cancel, returned)
			// rm starts no git once the context ended, and a write that
			// was its last leaves only the prune to fail.
			want := 1
			if tc.removed {
				want = 0
			}
			if code := <-returned; code != want {
				t.Fatalf("rm exited %d, want %d", code, want)
			}
			if got := gitIn(t, main, "for-each-ref", "--format=%(objectname)", "refs/slimtree/"); got != head {
				t.Fatalf("recovery refs point at %q, want %s", got, head)
			}
			if tc.removed {
				requireNoWorktree(t, main, path)
			}
			requireNoGitLock(t, filepath.Join(main, ".git"))
		})
	}
}

// rm checks a worktree for changes with a git status that writes nothing.
// A git status that may take index.lock refreshes the index under it, and a
// Ctrl-C right after git creates the lock would leave it, so every later git
// add or checkout in the worktree fails. A file whose stat data the index no
// longer matches makes such a status write the index. The fake git holds git
// worktree remove, which runs after the check. An artifactfs worktree's index
// lies in its git dir, outside the mount, and checkout-index leaves no stat
// data in it.
func TestRmChecksAWorktreeWithoutWritingItsIndex(t *testing.T) {
	t.Run("apfsclone", func(t *testing.T) {
		homeAfterFirstRun(t)
		cloneWith(t, nil)
		main := newRepo(t)
		path := filepath.Join(filepath.Dir(main), "main-feat")
		if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
			t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
		}
		past := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(path, "README.md"), past, past); err != nil {
			t.Fatal(err)
		}
		index := strings.TrimSpace(gitIn(t, path, "rev-parse", "--path-format=absolute", "--git-path", "index"))
		requireRmLeavesTheIndex(t, index, func() int {
			code, _, _ := slimtree(t.Context(), "rm", "main-feat")
			return code
		})
		requireNoWorktree(t, main, path)
	})
	t.Run("artifactfs", func(t *testing.T) {
		main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
		servedArtifactFS(t, &r, "mounted")
		requireRmLeavesTheIndex(t, filepath.Join(r.GitDir, "index"), func() int {
			code, _, _ := linux(t.Context(), "rm", "wt")
			return code
		})
		requireNoWorktree(t, main, r.Path)
	})
}

// requireRmLeavesTheIndex runs rm, which must exit 0, and fails t when the
// index changed before rm's git worktree remove started.
func requireRmLeavesTheIndex(t *testing.T, index string, rm func() int) {
	t.Helper()
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	h := holdWrite(t, " worktree remove ")
	returned := make(chan int, 1)
	go func() { returned <- rm() }()
	h.waitStarted(t, returned)
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Error("rm's check rewrote the worktree's index")
	}
	if err := os.WriteFile(h.released, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := <-returned; code != 0 {
		t.Fatalf("rm exited %d, want 0", code)
	}
}

// A rollback whose deadline passes while it deletes the branch new made, or
// unsets the tracking config new wrote, does not stop the write. The
// deadline expires once the held write started.
func TestARollbackPastItsDeadlineEndsItsWrite(t *testing.T) {
	for _, tc := range []struct{ name, hold string }{
		{"branch", " update-ref -d refs/heads/feat "},
		{"tracking config", " config --unset-all branch.feat.remote "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeAfterFirstRun(t)
			main := newRepo(t)
			expire := filepath.Join(t.TempDir(), "expire")
			rollbackContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
				return gittest.ExpireOnFile(ctx, expire)
			}
			t.Cleanup(func() { rollbackContext = nil })
			// The first clone ends new, which then rolls back.
			path := filepath.Join(filepath.Dir(main), "main-feat")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cloneWith(t, func(src, dst string) error {
				if strings.HasPrefix(dst, path+string(filepath.Separator)) {
					cancel()
				}
				return copyClone(src, dst)
			})
			h := holdWrite(t, tc.hold)

			returned := make(chan int, 1)
			go func() {
				code, _, _ := slimtree(ctx, "new", "feat", "--from", "topic", "--track")
				returned <- code
			}()
			h.outlive(t, func() {
				if err := os.WriteFile(expire, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}, returned)
			if code := <-returned; code != 1 {
				t.Fatalf("new exited %d, want 1", code)
			}
			switch tc.name {
			case "branch":
				gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
			case "tracking config":
				gitFindsNothing(t, main, "config", "branch.feat.remote")
			}
			requireNoGitLock(t, filepath.Join(main, ".git"))
		})
	}
}

// A Ctrl-C that lands while prune deletes a recovery ref does not stop the
// delete. prune's wait for its repository ends at the Ctrl-C after at most
// worktreeListGrace, so the test waits for the fake's finished mark, not for
// prune.
func TestACanceledPruneDeletesTheRecoveryRef(t *testing.T) {
	homeDir := pruneHome(t)
	commonDir, commit := commitRepo(t)
	work := filepath.Join(filepath.Dir(filepath.Dir(commonDir)), "repo-feat")
	old, oldRef := archiveAt(t, homeDir, work, commonDir, time.Now().Add(-31*24*time.Hour), commit, commit)
	if got := recoveryRefs(t, commonDir); got != oldRef+"\n" {
		t.Fatalf("recovery refs before prune = %q, want %s", got, oldRef)
	}
	h := holdWrite(t, " update-ref --no-deref -d "+oldRef+" ")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	returned := make(chan int, 1)
	go func() {
		code, _, _ := slimtree(ctx, "prune")
		returned <- code
	}()
	h.outlive(t, cancel, returned)
	h.waitFinished(t)
	if code := <-returned; code == 0 {
		t.Fatal("prune exited 0 after the Ctrl-C, want a failure")
	}
	if got := recoveryRefs(t, commonDir); got != "" {
		t.Fatalf("recovery refs after prune = %q, want none", got)
	}
	// The Ctrl-C ended prune before it deleted the archive, which the next
	// prune deletes.
	requireExists(t, old)
	requireNoGitLock(t, commonDir)
}
