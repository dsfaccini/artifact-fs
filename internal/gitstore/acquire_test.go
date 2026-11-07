package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestPrepareFetchedBranchPreservesStagedEntries(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	run(t, "git", "-C", repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "feature.txt")
	run(t, "git", "-C", repo, "commit", "-m", "feature")
	featureOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "checkout", "main")
	run(t, "git", "-C", repo, "update-ref", "refs/remotes/origin/feature", featureOID)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "intent.txt"), []byte("intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "add", "-N", "intent.txt")

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: filepath.Join(repo, ".git")}}
	if err := New(nil).PrepareFetchedBranch(context.Background(), cfg, "feature"); err != nil {
		t.Fatal(err)
	}
	staged := runOutput(t, "git", "-C", repo, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "tracked.txt") {
		t.Fatalf("staged entries = %q, want tracked.txt", staged)
	}
	debug := runOutput(t, "git", "-C", repo, "ls-files", "--debug", "intent.txt")
	if !strings.Contains(debug, "flags: 20004000") {
		t.Fatalf("intent-to-add entry was not preserved:\n%s", debug)
	}
}

func TestPrepareFetchedBranchConflictDoesNotMoveHEAD(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	baseOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))

	run(t, "git", "-C", repo, "checkout", "-b", "future")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "remote")
	futureOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "checkout", "main")
	run(t, "git", "-C", repo, "branch", "-D", "future")
	run(t, "git", "-C", repo, "update-ref", "refs/remotes/origin/feature", futureOID)

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: filepath.Join(repo, ".git")}}
	err := New(nil).PrepareFetchedBranch(context.Background(), cfg, "feature")
	if err == nil {
		t.Fatal("expected staged conflict")
	}
	if head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD")); head != baseOID {
		t.Fatalf("HEAD = %s, want %s", head, baseOID)
	}
	if ref := strings.TrimSpace(runOutput(t, "git", "-C", repo, "symbolic-ref", "--short", "HEAD")); ref != "main" {
		t.Fatalf("HEAD ref = %q, want main", ref)
	}
	if staged := runOutput(t, "git", "-C", repo, "show", ":tracked.txt"); staged != "staged\n" {
		t.Fatalf("staged content = %q, want staged", staged)
	}
	if err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/feature").Run(); err == nil {
		t.Fatal("feature branch was created despite index conflict")
	}
}

// fetchedAheadRepo is a repository on main whose remoteRef, one commit ahead
// of main, was fetched with no local branch at it. It returns the git dir and
// both commits.
func fetchedAheadRepo(t *testing.T, dir string, remoteRef string) (gitDir string, baseOID string, aheadOID string) {
	t.Helper()
	repo := filepath.Join(dir, "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	baseOID = strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "checkout", "-b", "ahead")
	if err := os.WriteFile(filepath.Join(repo, "ahead.txt"), []byte("ahead\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "ahead.txt")
	run(t, "git", "-C", repo, "commit", "-m", "ahead")
	aheadOID = strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "checkout", "main")
	run(t, "git", "-C", repo, "branch", "-D", "ahead")
	run(t, "git", "-C", repo, "update-ref", remoteRef, aheadOID)
	return filepath.Join(repo, ".git"), baseOID, aheadOID
}

// wrapGit puts a git on PATH that runs script, which reaches the real git as
// "$AFS_REAL_GIT", and returns the real git's path.
func wrapGit(t *testing.T, dir string, script string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return realGit
}

// A rollback that needs two steps ends at one deadline, so it cannot outlast
// the daemon's drain of the preparation it rolls back.
func TestPrepareFetchedBranchRollbackStepsShareOneDeadline(t *testing.T) {
	tmp := t.TempDir()
	gitDir, _, _ := fetchedAheadRepo(t, tmp, "refs/remotes/origin/feature")
	// Pointing HEAD at the branch fails, which takes a two-step rollback. The
	// rollback's first git command hangs past its deadline, and a git command
	// that starts after it marks itself late.
	failed := filepath.Join(tmp, "symbolic-ref-failed")
	firstStep := filepath.Join(tmp, "first-step")
	lateStep := filepath.Join(tmp, "late-step")
	wrapGit(t, tmp, "#!/bin/sh\nif [ \"$1\" = symbolic-ref ] && [ \"$2\" = HEAD ] && [ \"$#\" = 3 ]; then\n  : > \"$AFS_FAILED\"\n  exit 1\nfi\nif [ -e \"$AFS_FAILED\" ]; then\n  if [ ! -e \"$AFS_FIRST_STEP\" ]; then\n    : > \"$AFS_FIRST_STEP\"\n    sleep 60\n    exit 1\n  fi\n  : > \"$AFS_LATE_STEP\"\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n")
	t.Setenv("AFS_FAILED", failed)
	t.Setenv("AFS_FIRST_STEP", firstStep)
	t.Setenv("AFS_LATE_STEP", lateStep)

	// A deadline that expires on the first step's marker would expire at once
	// in a step that made its own context, so it could not tell one deadline
	// from one per step. A timer can.
	store := New(nil)
	store.rollbackContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}}
	if err := store.PrepareFetchedBranch(context.Background(), cfg, "feature"); err == nil {
		t.Fatal("PrepareFetchedBranch succeeded although HEAD could not move")
	}
	if _, err := os.Stat(firstStep); err != nil {
		t.Fatalf("the rollback's first git command did not run: %v", err)
	}
	if _, err := os.Stat(lateStep); err == nil {
		t.Fatal("a rollback git command started after the rollback's deadline passed")
	}
}

// A rollback whose ref restore uses up its deadline has already restored the
// index, so the index matches HEAD.
func TestPrepareFetchedBranchRollbackRestoresTheIndexFirst(t *testing.T) {
	tmp := t.TempDir()
	gitDir, baseOID, _ := fetchedAheadRepo(t, tmp, "refs/remotes/origin/feature")
	// Pointing HEAD at the branch fails, then the ref restore hangs until the
	// rollback's deadline passes.
	refRestore := filepath.Join(tmp, "ref-restore")
	realGit := wrapGit(t, tmp, "#!/bin/sh\nif [ \"$1\" = symbolic-ref ] && [ \"$2\" = HEAD ] && [ \"$#\" = 3 ]; then\n  : > \"$AFS_FAILED\"\n  exit 1\nfi\nif [ \"$1\" = update-ref ] && [ -e \"$AFS_FAILED\" ]; then\n  : > \"$AFS_REF_RESTORE\"\n  sleep 60\n  exit 1\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n")
	t.Setenv("AFS_FAILED", filepath.Join(tmp, "symbolic-ref-failed"))
	t.Setenv("AFS_REF_RESTORE", refRestore)

	store := New(nil)
	store.rollbackContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return gittest.ExpireOnFile(context.WithoutCancel(ctx), refRestore)
	}
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}}
	if err := store.PrepareFetchedBranch(context.Background(), cfg, "feature"); err == nil {
		t.Fatal("PrepareFetchedBranch succeeded although HEAD could not move")
	}
	if head := strings.TrimSpace(runOutput(t, realGit, "--git-dir", gitDir, "rev-parse", "HEAD")); head != baseOID {
		t.Fatalf("HEAD = %s, want %s", head, baseOID)
	}
	if staged := runOutput(t, realGit, "--git-dir", gitDir, "diff-index", "--cached", "--name-only", "HEAD"); staged != "" {
		t.Fatalf("the index differs from HEAD in %q", staged)
	}
}

// A ref update that git completed before a cancel stopped it keeps the
// transition: the index stays at the commit HEAD moved to.
func TestPrepareFetchedBranchKeepsARefUpdateThatLanded(t *testing.T) {
	for _, tt := range []struct {
		name      string
		remoteRef string
		fetchRef  string
		// lands is the git command that completes before the cancel.
		lands string
	}{
		{"branch HEAD is on", "refs/remotes/origin/main", "main", "update-ref"},
		{"detached HEAD", fetchedFullRefRemoteTrackingRef, "refs/pull/10/head", "update-ref"},
		{"HEAD onto the branch", "refs/remotes/origin/feature", "feature", "symbolic-ref"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			gitDir, _, aheadOID := fetchedAheadRepo(t, tmp, tt.remoteRef)
			// The command completes, then hangs until the cancel stops it.
			landed := filepath.Join(tmp, "landed")
			realGit := wrapGit(t, tmp, "#!/bin/sh\nif [ \"$1\" = \"$AFS_LANDS\" ] && [ ! -e \"$AFS_LANDED\" ]; then\n  \"$AFS_REAL_GIT\" \"$@\" || exit\n  : > \"$AFS_LANDED\"\n  sleep 60\n  exit 0\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n")
			t.Setenv("AFS_LANDS", tt.lands)
			t.Setenv("AFS_LANDED", landed)

			ctx, cancel := gittest.ExpireOnFile(context.Background(), landed)
			defer cancel()
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}}
			if err := New(nil).PrepareFetchedBranch(ctx, cfg, tt.fetchRef); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("PrepareFetchedBranch error = %v, want deadline exceeded", err)
			}
			head, err := exec.Command(realGit, "--git-dir", gitDir, "rev-parse", "--verify", "HEAD^{commit}").Output()
			if err != nil {
				t.Fatalf("HEAD does not resolve: %v", err)
			}
			if got := strings.TrimSpace(string(head)); got != aheadOID {
				t.Fatalf("HEAD = %s, want %s", got, aheadOID)
			}
			if staged := runOutput(t, realGit, "--git-dir", gitDir, "diff-index", "--cached", "--name-only", "HEAD"); staged != "" {
				t.Fatalf("the index differs from HEAD in %q", staged)
			}
		})
	}
}

// A forward read-tree that wrote the index before a cancel stopped it is
// undone, so the index matches HEAD again, staged paths included.
func TestPrepareFetchedBranchUndoesAReadTreeThatLanded(t *testing.T) {
	for _, tt := range []struct {
		name      string
		remoteRef string
		fetchRef  string
		staged    bool
	}{
		{"branch", "refs/remotes/origin/main", "main", false},
		{"branch with a staged file", "refs/remotes/origin/main", "main", true},
		{"detached HEAD", fetchedFullRefRemoteTrackingRef, "refs/pull/10/head", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			gitDir, baseOID, _ := fetchedAheadRepo(t, tmp, tt.remoteRef)
			wantStaged := ""
			if tt.staged {
				repo := filepath.Dir(gitDir)
				if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				run(t, "git", "-C", repo, "add", "staged.txt")
				wantStaged = "staged.txt\n"
			}
			// The read-tree writes the index, then hangs until the cancel
			// stops it.
			landed := filepath.Join(tmp, "landed")
			realGit := wrapGit(t, tmp, "#!/bin/sh\nif [ \"$1\" = read-tree ] && [ ! -e \"$AFS_LANDED\" ]; then\n  \"$AFS_REAL_GIT\" \"$@\" || exit\n  : > \"$AFS_LANDED\"\n  sleep 60\n  exit 0\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n")
			t.Setenv("AFS_LANDED", landed)

			ctx, cancel := gittest.ExpireOnFile(context.Background(), landed)
			defer cancel()
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}}
			if err := New(nil).PrepareFetchedBranch(ctx, cfg, tt.fetchRef); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("PrepareFetchedBranch error = %v, want deadline exceeded", err)
			}
			if head := strings.TrimSpace(runOutput(t, realGit, "--git-dir", gitDir, "rev-parse", "HEAD")); head != baseOID {
				t.Fatalf("HEAD = %s, want %s", head, baseOID)
			}
			if staged := runOutput(t, realGit, "--git-dir", gitDir, "diff-index", "--cached", "--name-only", "HEAD"); staged != wantStaged {
				t.Fatalf("the index differs from HEAD in %q, want %q", staged, wantStaged)
			}
		})
	}
}

// A forward read-tree that failed before it wrote the index leaves a staged
// change alone, also one that holds exactly the fetched content.
func TestPrepareFetchedBranchKeepsAStagedChangeAFailedReadTreeLeft(t *testing.T) {
	for _, tt := range []struct {
		name string
		// script fails the first read-tree, writing $AFS_MARKER.
		script string
		// cancel cancels the preparation once $AFS_MARKER exists.
		cancel bool
	}{
		{"canceled", "#!/bin/sh\nif [ \"$1\" = read-tree ] && [ ! -e \"$AFS_MARKER\" ]; then\n  : > \"$AFS_MARKER\"\n  sleep 60\n  exit 1\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n", true},
		{"failed on its own", "#!/bin/sh\nif [ \"$1\" = read-tree ] && [ ! -e \"$AFS_MARKER\" ]; then\n  : > \"$AFS_MARKER\"\n  echo \"fatal: Unable to create 'index.lock': File exists.\" >&2\n  exit 128\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			gitDir, baseOID, _ := fetchedAheadRepo(t, tmp, "refs/remotes/origin/main")
			repo := filepath.Dir(gitDir)
			if err := os.WriteFile(filepath.Join(repo, "ahead.txt"), []byte("ahead\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", repo, "add", "ahead.txt")
			marker := filepath.Join(tmp, "marker")
			realGit := wrapGit(t, tmp, tt.script)
			t.Setenv("AFS_MARKER", marker)

			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = gittest.ExpireOnFile(ctx, marker)
				defer cancel()
			}
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}}
			if err := New(nil).PrepareFetchedBranch(ctx, cfg, "main"); err == nil {
				t.Fatal("PrepareFetchedBranch succeeded although read-tree failed")
			}
			if head := strings.TrimSpace(runOutput(t, realGit, "--git-dir", gitDir, "rev-parse", "HEAD")); head != baseOID {
				t.Fatalf("HEAD = %s, want %s", head, baseOID)
			}
			if staged := runOutput(t, realGit, "--git-dir", gitDir, "diff-index", "--cached", "--name-only", "HEAD"); staged != "ahead.txt\n" {
				t.Fatalf("the index differs from HEAD in %q, want the staged %q", staged, "ahead.txt\n")
			}
		})
	}
}

// A forward read-tree that changed only a gitlink, which
// submodule.<name>.ignore=all hides from diffs, is undone too.
func TestPrepareFetchedBranchUndoesALandedReadTreeOfAnIgnoredGitlink(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitmodules"), []byte("[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", ".gitmodules")
	run(t, "git", "-C", repo, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",sub")
	baseTree := strings.TrimSpace(runOutput(t, "git", "-C", repo, "write-tree"))
	baseOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit-tree", baseTree, "-m", "base"))
	run(t, "git", "-C", repo, "update-index", "--cacheinfo", "160000,"+strings.Repeat("2", 40)+",sub")
	aheadTree := strings.TrimSpace(runOutput(t, "git", "-C", repo, "write-tree"))
	aheadOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit-tree", aheadTree, "-p", baseOID, "-m", "ahead"))
	run(t, "git", "-C", repo, "update-ref", "refs/heads/main", baseOID)
	run(t, "git", "-C", repo, "read-tree", baseOID)
	run(t, "git", "-C", repo, "update-ref", "refs/remotes/origin/main", aheadOID)
	run(t, "git", "-C", repo, "config", "submodule.sub.ignore", "all")
	gitDir := filepath.Join(repo, ".git")
	// The read-tree writes the index, then hangs until the cancel stops it.
	landed := filepath.Join(tmp, "landed")
	realGit := wrapGit(t, tmp, "#!/bin/sh\nif [ \"$1\" = read-tree ] && [ ! -e \"$AFS_LANDED\" ]; then\n  \"$AFS_REAL_GIT\" \"$@\" || exit\n  : > \"$AFS_LANDED\"\n  sleep 60\n  exit 0\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n")
	t.Setenv("AFS_LANDED", landed)

	ctx, cancel := gittest.ExpireOnFile(context.Background(), landed)
	defer cancel()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}}
	if err := New(nil).PrepareFetchedBranch(ctx, cfg, "main"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PrepareFetchedBranch error = %v, want deadline exceeded", err)
	}
	if head := strings.TrimSpace(runOutput(t, realGit, "--git-dir", gitDir, "rev-parse", "HEAD")); head != baseOID {
		t.Fatalf("HEAD = %s, want %s", head, baseOID)
	}
	if staged := runOutput(t, realGit, "--git-dir", gitDir, "diff-index", "--cached", "--name-only", "--ignore-submodules=none", "HEAD"); staged != "" {
		t.Fatalf("the index differs from HEAD in %q", staged)
	}
}

func TestFetchRefAmbientAndPrepareFetchedBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, "git", "-C", work, "push", "origin", "master")

	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "file://"+bare)

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: preparedGitDir}, RepoSpec: model.RepoSpec{Branch: "master"}}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := store.ValidatePreparedGitDir(ctx, cfg); err != nil {
		t.Fatalf("ValidatePreparedGitDir: %v", err)
	}
	if err := store.FetchRefAmbient(ctx, cfg, "master"); err != nil {
		t.Fatalf("FetchRefAmbient: %v", err)
	}
	if err := store.PrepareFetchedBranch(ctx, cfg, "master"); err != nil {
		t.Fatalf("PrepareFetchedBranch: %v", err)
	}
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if ref != "master" {
		t.Fatalf("ref = %q, want master", ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "README.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("README.md not found in prepared tree")
	}
}

func TestPrepareFetchedBranchRefusesPreparedGitDirRewind(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("origin\n"), 0o644)
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "origin")
	run(t, "git", "-C", work, "push", "origin", "master")

	run(t, "git", "clone", bare, preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "checkout", "master")
	localPath := filepath.Join(preparedWorktree, "LOCAL.md")
	os.WriteFile(localPath, []byte("local\n"), 0o644)
	run(t, "git", "-C", preparedWorktree, "add", "LOCAL.md")
	run(t, "git", "-C", preparedWorktree, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "local")
	if err := os.Rename(filepath.Join(preparedWorktree, ".git"), preparedGitDir); err != nil {
		t.Fatal(err)
	}

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: preparedGitDir}, RepoSpec: model.RepoSpec{Branch: "master", PreparedGitDir: true}}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	localOID, err := runGit(ctx, preparedGitDir, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	localOID = strings.TrimSpace(localOID)

	if err := store.FetchRefAmbient(ctx, cfg, "master"); err != nil {
		t.Fatalf("FetchRefAmbient: %v", err)
	}
	err = store.PrepareFetchedBranch(ctx, cfg, "master")
	if err == nil {
		t.Fatal("expected non-fast-forward prepared branch update to fail")
	}
	if strings.Contains(err.Error(), localOID) {
		t.Fatalf("error leaked commit details: %v", err)
	}
	afterOID, err := runGit(ctx, preparedGitDir, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	afterOID = strings.TrimSpace(afterOID)
	if afterOID != localOID {
		t.Fatalf("prepared branch moved to %s, want %s", afterOID, localOID)
	}
}

func TestZeroOIDForGitDirMatchesObjectFormat(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			initArgs := []string{"init"}
			if objectFormat == "sha256" {
				initArgs = append(initArgs, "--object-format=sha256")
			}
			initArgs = append(initArgs, repo)
			run(t, "git", initArgs...)
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(objectFormat+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", repo, "add", "README.md")
			run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", objectFormat)

			gitDir := filepath.Join(repo, ".git")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			zero, err := zeroOIDForGitDir(ctx, gitDir)
			if err != nil {
				t.Fatal(err)
			}
			wantLength := 40
			if objectFormat == "sha256" {
				wantLength = 64
			}
			if len(zero) != wantLength || strings.Trim(zero, "0") != "" {
				t.Fatalf("zero OID = %q, want %d zeros", zero, wantLength)
			}
			head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(ctx, gitDir, "update-ref", "refs/heads/prepared", head, zero); err != nil {
				t.Fatalf("create prepared branch with zero OID: %v", err)
			}
		})
	}
}

// An existing clone moves to another branch the way the daemon refreshes it:
// point origin at the remote, fetch the ref, then prepare the fetched branch.
func TestFetchedBranchReplacesExistingCloneBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "repo.git")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "master")
	run(t, "git", "-C", work, "push", "origin", "master")
	run(t, "git", "-C", work, "checkout", "-b", "dev")
	os.WriteFile(filepath.Join(work, "DEV.md"), []byte("dev\n"), 0o644)
	run(t, "git", "-C", work, "add", "DEV.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "dev")
	run(t, "git", "-C", work, "push", "origin", "dev")

	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + bare, Branch: "master", FetchRef: "master"}}
	if err := store.CloneBlobless(ctx, cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	assertArtifactWorktreeConfig(t, ctx, gitDir)
	cfg.Branch = "dev"
	cfg.FetchRef = "dev"
	if err := store.ConfigureRemoteAmbient(ctx, cfg); err != nil {
		t.Fatalf("ConfigureRemoteAmbient: %v", err)
	}
	if err := store.FetchRefAmbient(ctx, cfg, cfg.FetchRef); err != nil {
		t.Fatalf("FetchRefAmbient: %v", err)
	}
	if err := store.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef); err != nil {
		t.Fatalf("PrepareFetchedBranch: %v", err)
	}
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if ref != "dev" {
		t.Fatalf("ref = %q, want dev", ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "DEV.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("DEV.md not found after existing clone prepare")
	}
}

func TestPrepareSourceIsShallowAndVerifiesRequiredCommit(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	for i, content := range []string{"one\n", "two\n", "three\n"} {
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", work, "add", "README.md")
		run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", fmt.Sprintf("commit %d", i+1))
	}
	run(t, "git", "-C", work, "push", "origin", "main")
	run(t, "git", "-C", work, "checkout", "-b", "side", "HEAD~2")
	if err := os.WriteFile(filepath.Join(work, "SIDE.md"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "SIDE.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "side")
	run(t, "git", "-C", work, "push", "origin", "side")
	run(t, "git", "-C", work, "checkout", "main")
	run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "true")

	tip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	blobOID := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD:README.md"))
	gitDir := filepath.Join(tmp, "snapshot.git")
	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			ID:        "verified",
			Name:      "verified",
			GitDir:    gitDir,
			MountPath: filepath.Join(tmp, "mounted"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL:      "file://" + bare,
			Branch:         "refs/heads/main",
			RequiredCommit: tip,
			HistoryDepth:   1,
		},
	}
	requirement := model.SourceRequirement{Ref: cfg.Branch, RequiredCommit: tip, Depth: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(nil)
	prepared, err := store.PrepareSource(ctx, cfg, requirement)
	if err != nil {
		t.Fatalf("PrepareSource: %v", err)
	}
	assertArtifactWorktreeConfig(t, ctx, gitDir)
	if !prepared.Verified || !prepared.Acquired || prepared.Ref != cfg.Branch || prepared.Commit != tip {
		t.Fatalf("prepared source = %+v", prepared)
	}
	mountedWorktree := cfg.MountPath
	if err := os.MkdirAll(mountedWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountedWorktree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureStatusOptimization(ctx, cfg, tmp); err != nil {
		t.Fatalf("ConfigureStatusOptimization: %v", err)
	}
	if inside := strings.TrimSpace(runOutput(t, "git", "-C", mountedWorktree, "rev-parse", "--is-inside-work-tree")); inside != "true" {
		t.Fatalf("mounted Git directory reported is-inside-work-tree = %q, want true", inside)
	}
	configuredWorktree, err := runGit(ctx, gitDir, "config", "--path", "core.worktree")
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorktree, err := filepath.EvalSymlinks(configuredWorktree)
	if err != nil {
		t.Fatal(err)
	}
	wantWorktree, err := filepath.EvalSymlinks(mountedWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedWorktree != wantWorktree {
		t.Fatalf("core.worktree = %q, want %q", resolvedWorktree, wantWorktree)
	}

	count, err := runGit(ctx, gitDir, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if count != "1" {
		t.Fatalf("snapshot history count = %q, want 1", count)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "shallow")); err != nil {
		t.Fatalf("snapshot clone is not shallow: %v", err)
	}
	fetchRef, err := runGit(ctx, gitDir, "config", "--get-all", "remote.origin.fetch")
	if err != nil {
		t.Fatal(err)
	}
	wantFetchRef := "+refs/heads/main:" + verifiedSourceTrackingRef
	if fetchRef != wantFetchRef {
		t.Fatalf("remote.origin.fetch = %q, want %q", fetchRef, wantFetchRef)
	}
	// The generated hook targets os.Executable, which is the test runner here.
	// Disable it before fetch so newer Git versions do not recurse into tests.
	if _, err := runGit(ctx, gitDir, "config", "--unset", "core.fsmonitor"); err != nil {
		t.Fatalf("disable test fsmonitor hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "NEW.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "NEW.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "advance main")
	run(t, "git", "-C", work, "push", "origin", "main")
	advancedTip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	if err := New(nil).FetchNonInteractive(ctx, cfg); err != nil {
		t.Fatalf("verified source refresh: %v", err)
	}
	count, err = runGit(ctx, gitDir, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if count != "1" {
		t.Fatalf("snapshot history count after refresh = %q, want 1", count)
	}
	trackingTip, err := runGit(ctx, gitDir, "rev-parse", verifiedSourceTrackingRef)
	if err != nil {
		t.Fatal(err)
	}
	if trackingTip != advancedTip {
		t.Fatalf("tracking ref = %q, want advanced source %q", trackingTip, advancedTip)
	}
	trackingCount, err := runGit(ctx, gitDir, "rev-list", "--count", verifiedSourceTrackingRef)
	if err != nil {
		t.Fatal(err)
	}
	if trackingCount != "1" {
		t.Fatalf("tracking history count after refresh = %q, want 1", trackingCount)
	}
	if _, err := runGit(ctx, gitDir, "show-ref", "--verify", "refs/remotes/origin/side"); err == nil {
		t.Fatal("verified source refresh fetched an unrelated branch")
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", blobOID)
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_NO_LAZY_FETCH=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("snapshot clone eagerly downloaded the HEAD blob")
	}

	// A server without partial-clone filtering still honors depth=1. Git falls
	// back to transferring the tip's blobs, but not the branch's full history.
	run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "false")
	fallbackDir := filepath.Join(tmp, "fallback.git")
	cfg.GitDir = fallbackDir
	requirement.RequiredCommit = advancedTip
	if _, err := New(nil).PrepareSource(ctx, cfg, requirement); err != nil {
		t.Fatalf("PrepareSource fallback: %v", err)
	}
	fallbackCount, err := runGit(ctx, fallbackDir, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if fallbackCount != "1" {
		t.Fatalf("fallback snapshot history count = %q, want 1", fallbackCount)
	}
	cmd = exec.CommandContext(ctx, "git", "cat-file", "-e", blobOID)
	cmd.Env = append(os.Environ(), "GIT_DIR="+fallbackDir, "GIT_NO_LAZY_FETCH=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("fallback snapshot did not download the HEAD blob: %v", err)
	}

	mismatchDir := filepath.Join(tmp, "mismatch.git")
	cfg.GitDir = mismatchDir
	requirement.RequiredCommit = strings.Repeat("0", 40)
	_, err = New(nil).PrepareSource(ctx, cfg, requirement)
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("mismatched source error = %v", err)
	}
	if _, statErr := os.Stat(mismatchDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("mismatched snapshot left git dir behind: %v", statErr)
	}
}

func TestPrepareSourceSupportsSHA256Remote(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "snapshot.git")

	run(t, "git", "init", "--bare", "--object-format=sha256", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("sha256\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "sha256")
	run(t, "git", "-C", work, "push", "origin", "main")

	tip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	if len(tip) != 64 {
		t.Fatalf("SHA-256 commit length = %d, want 64", len(tip))
	}
	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			ID:     "sha256",
			Name:   "sha256",
			GitDir: gitDir,
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "file://" + bare,
		},
	}
	requirement := model.SourceRequirement{
		Ref:            "refs/heads/main",
		RequiredCommit: tip,
		Depth:          1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prepared, err := New(nil).PrepareSource(ctx, cfg, requirement)
	if err != nil {
		t.Fatalf("PrepareSource: %v", err)
	}
	if prepared.Commit != tip || !prepared.Verified || !prepared.Acquired {
		t.Fatalf("prepared source = %+v", prepared)
	}
	format, err := runGit(ctx, gitDir, "rev-parse", "--show-object-format")
	if err != nil {
		t.Fatal(err)
	}
	if format != "sha256" {
		t.Fatalf("object format = %q, want sha256", format)
	}
	head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head != tip {
		t.Fatalf("HEAD = %q, want %q", head, tip)
	}
}

func TestPrepareSourceReacquiresMissingOrCorruptReceiptGitDir(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "verified")
	run(t, "git", "-C", work, "push", "origin", "main")

	tip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	requirement := model.SourceRequirement{
		Ref:            "refs/heads/main",
		RequiredCommit: tip,
		Depth:          1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, test := range []struct {
		name   string
		damage func(t *testing.T, gitDir string)
	}{
		{
			name: "missing",
			damage: func(t *testing.T, gitDir string) {
				t.Helper()
				if err := os.RemoveAll(gitDir); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt",
			damage: func(t *testing.T, gitDir string) {
				t.Helper()
				if err := os.RemoveAll(gitDir); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(gitDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(gitDir, "not-a-repository"), []byte("corrupt\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt_index",
			damage: func(t *testing.T, gitDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(gitDir, "index"), []byte("broken\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gitDir := filepath.Join(tmp, test.name+".git")
			cfg := model.RepoConfig{
				RepoPaths: model.RepoPaths{
					ID:     model.RepoID(test.name),
					Name:   test.name,
					GitDir: gitDir,
				},
				RepoSpec: model.RepoSpec{
					RemoteURL: "file://" + bare,
				},
			}
			store := New(nil)
			if _, err := store.PrepareSource(ctx, cfg, requirement); err != nil {
				t.Fatalf("initial PrepareSource: %v", err)
			}
			oldPool, err := store.getPool(gitDir)
			if err != nil {
				t.Fatal(err)
			}
			test.damage(t, gitDir)

			cfg.AcquiredRef = requirement.Ref
			cfg.AcquiredCommit = requirement.RequiredCommit
			prepared, err := store.PrepareSource(ctx, cfg, requirement)
			if err != nil {
				t.Fatalf("reacquire PrepareSource: %v", err)
			}
			if !prepared.Verified || !prepared.Acquired || prepared.Commit != tip {
				t.Fatalf("prepared source = %+v", prepared)
			}
			head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if head != tip {
				t.Fatalf("HEAD = %q, want %q", head, tip)
			}
			newPool, err := store.getPool(gitDir)
			if err != nil {
				t.Fatal(err)
			}
			if newPool == oldPool {
				t.Fatal("reacquisition reused the batch pool from the replaced Git directory")
			}
		})
	}
}

func TestPrepareSourceReceiptPreservesIndexAndRestoresHEAD(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "snapshot.git")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	for _, content := range []string{"base\n", "required\n"} {
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", work, "add", "README.md")
		run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", strings.TrimSpace(content))
	}
	run(t, "git", "-C", work, "push", "origin", "main")

	requiredCommit := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			ID:     "verified",
			Name:   "verified",
			GitDir: gitDir,
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "file://" + bare,
		},
	}
	requirement := model.SourceRequirement{
		Ref:            "refs/heads/main",
		RequiredCommit: requiredCommit,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(nil)
	if _, err := store.PrepareSource(ctx, cfg, requirement); err != nil {
		t.Fatalf("initial PrepareSource: %v", err)
	}

	stagedPath := filepath.Join(tmp, "staged-readme")
	if err := os.WriteFile(stagedPath, []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagedOID, err := runGit(ctx, gitDir, "hash-object", "-w", stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, gitDir, "update-index", "--add", "--cacheinfo", "100644", stagedOID, "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, gitDir, "update-ref", "--no-deref", "HEAD", requiredCommit+"^"); err != nil {
		t.Fatal(err)
	}

	cfg.AcquiredRef = requirement.Ref
	cfg.AcquiredCommit = requirement.RequiredCommit
	prepared, err := store.PrepareSource(ctx, cfg, requirement)
	if err != nil {
		t.Fatalf("receipt PrepareSource: %v", err)
	}
	if prepared.Acquired {
		t.Fatal("receipt reuse unexpectedly performed remote acquisition")
	}
	head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head != requiredCommit {
		t.Fatalf("HEAD = %q, want required commit %q", head, requiredCommit)
	}
	staged, err := runGit(ctx, gitDir, "diff", "--cached", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if staged != "README.md" {
		t.Fatalf("staged paths = %q, want README.md", staged)
	}
}

func TestCloneBloblessUsesExactCanonicalTag(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			tmp := t.TempDir()
			bare := filepath.Join(tmp, "origin.git")
			work := filepath.Join(tmp, "work")
			gitDir := filepath.Join(tmp, "repo.git")

			initArgs := []string{"init", "--bare"}
			if objectFormat == "sha256" {
				initArgs = append(initArgs, "--object-format=sha256")
			}
			initArgs = append(initArgs, bare)
			run(t, "git", initArgs...)
			run(t, "git", "clone", bare, work)
			run(t, "git", "-C", work, "checkout", "-b", "main")
			if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("tag\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", work, "add", "README.md")
			run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "tag target")
			tagCommit := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
			run(t, "git", "-C", work, "update-ref", "refs/tags/release", tagCommit)

			run(t, "git", "-C", work, "checkout", "-b", "release")
			if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("branch\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", work, "add", "README.md")
			run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "same-named branch")
			branchCommit := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
			if branchCommit == tagCommit {
				t.Fatal("branch and tag unexpectedly resolve to the same commit")
			}
			run(t, "git", "-C", work, "push", "origin",
				"refs/heads/main:refs/heads/main",
				"refs/heads/release:refs/heads/release",
				"refs/tags/release:refs/tags/release",
			)
			run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "true")

			cfg := model.RepoConfig{
				RepoPaths: model.RepoPaths{
					ID:     "tag",
					Name:   "tag",
					GitDir: gitDir,
				},
				RepoSpec: model.RepoSpec{
					RemoteURL:    "file://" + bare,
					Branch:       "refs/tags/release",
					HistoryDepth: 1,
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := New(nil).CloneBlobless(ctx, cfg); err != nil {
				t.Fatalf("CloneBlobless: %v", err)
			}
			head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if head != tagCommit {
				t.Fatalf("HEAD = %q, want tag commit %q; same-named branch is %q", head, tagCommit, branchCommit)
			}
			format, err := runGit(ctx, gitDir, "rev-parse", "--show-object-format")
			if err != nil {
				t.Fatal(err)
			}
			if format != objectFormat {
				t.Fatalf("object format = %q, want %q", format, objectFormat)
			}
			fetchRef, err := runGit(ctx, gitDir, "config", "--get-all", "remote.origin.fetch")
			if err != nil {
				t.Fatal(err)
			}
			wantFetchRef := "+refs/tags/release:" + fetchedFullRefRemoteTrackingRef
			if fetchRef != wantFetchRef {
				t.Fatalf("remote.origin.fetch = %q, want %q", fetchRef, wantFetchRef)
			}
		})
	}
}

func TestCloneAndFetchRefSkipTags(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "repo.git")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "master")
	run(t, "git", "-C", work, "push", "origin", "master")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tmp, "git.log")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GIT_COMMAND_LOG\"\nexec \"$REAL_GIT\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_COMMAND_LOG", logPath)
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + bare, Branch: "master", FetchRef: "master"}}
	oldPool, err := store.getPool(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CloneBlobless(ctx, cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	newPool, err := store.getPool(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if newPool == oldPool {
		t.Fatal("clone reused the batch pool from the previously missing Git directory")
	}
	if err := store.FetchRefAmbient(ctx, cfg, cfg.FetchRef); err != nil {
		t.Fatalf("FetchRefAmbient: %v", err)
	}
	if err := store.FetchNonInteractive(ctx, cfg); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "clone --filter=blob:none --no-checkout --single-branch --no-tags --branch master") {
		t.Fatalf("clone did not include --no-tags; git log:\n%s", logText)
	}
	if !strings.Contains(logText, "fetch --filter=blob:none --no-tags origin +refs/heads/master:refs/remotes/origin/master") {
		t.Fatalf("fetch did not include --no-tags; git log:\n%s", logText)
	}
	if !strings.Contains(logText, "fetch --no-tags origin") {
		t.Fatalf("refresh fetch did not include --no-tags; git log:\n%s", logText)
	}
}

func TestFetchStartsBeforeRemoteLookupCompletes(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fetchStarted := filepath.Join(tmp, "fetch-started")
	// The fetch marks its start only while the lookup still runs. A passing
	// Fetch kills the lookup on return; the sleep bound limits how long the
	// lookup outlives a timeout panic.
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  'remote get-url origin') sleep 60; : > \"$AFS_LOOKUP_FINISHED\";;\n" +
		"  'fetch --no-tags origin') [ -f \"$AFS_LOOKUP_FINISHED\" ] || : > \"$AFS_FETCH_STARTED\"; printf '%s\\n' 'HTTP 503 transport failure' >&2; exit 1;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_FETCH_STARTED", fetchStarted)
	t.Setenv("AFS_LOOKUP_FINISHED", filepath.Join(tmp, "lookup-finished"))

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = nil
	err := store.FetchNonInteractive(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: filepath.Join(tmp, "repo.git")}, RepoSpec: model.RepoSpec{RemoteURL: "/private/configured/repo.git"}})
	if err == nil {
		t.Fatal("Fetch error = nil, want transport failure")
	}
	if _, statErr := os.Stat(fetchStarted); statErr != nil {
		t.Fatalf("fetch did not start while remote lookup was blocked: %v", statErr)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || operationErr.Attempts != 1 {
		t.Fatalf("Fetch error = %#v, want one attempted fetch", err)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationAttemptFailed+`"`) {
		t.Fatalf("fetch failure terminal log missing: %s", logOutput)
	}
}

func TestRemoteLookupIncludesConfiguredAndActualOrigin(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, "repo.git")
	run(t, "git", "init", "--bare", gitDir)
	const configured = "https://example.com/old/repo.git"
	actual := filepath.Join(tmp, "private", "repo.git")
	run(t, "git", "--git-dir", gitDir, "remote", "add", "origin", actual)
	remotesForLogging, cancel := New(nil).startRemotesForLogging(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{GitDir: gitDir}, RepoSpec: model.RepoSpec{RemoteURL: configured}})
	defer cancel()
	for {
		remotes, complete := remotesForLogging()
		if complete {
			if !slices.Contains(remotes, configured) || !slices.Contains(remotes, actual) {
				t.Fatalf("remotesForLogging = %q, want configured and actual remotes", remotes)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFetchRefAmbientFullRefPreparesDetachedHEAD(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, "git", "-C", work, "push", "origin", "master")
	run(t, "git", "-C", work, "checkout", "-b", "pull-request")
	os.WriteFile(filepath.Join(work, "PR.md"), []byte("pull request\n"), 0o644)
	run(t, "git", "-C", work, "add", "PR.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "pr")
	run(t, "git", "-C", work, "push", "origin", "HEAD:refs/pull/10/head")

	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "file://"+bare)

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: preparedGitDir}, RepoSpec: model.RepoSpec{Branch: "master"}}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := store.FetchRefAmbient(ctx, cfg, "refs/pull/10/head"); err != nil {
		t.Fatalf("FetchRefAmbient: %v", err)
	}
	if _, err := runGit(ctx, preparedGitDir, "rev-parse", "--verify", fetchedFullRefRemoteTrackingRef+"^{commit}"); err != nil {
		t.Fatalf("expected fetched full ref at safe remote-tracking ref: %v", err)
	}
	if err := store.PrepareFetchedBranch(ctx, cfg, "refs/pull/10/head"); err != nil {
		t.Fatalf("PrepareFetchedBranch: %v", err)
	}
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if ref != "DETACHED" {
		t.Fatalf("ref = %q, want DETACHED", ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "PR.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("PR.md not found in prepared tree")
	}
}
