//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/meta"
)

// sharedRestoreTimeout bounds restoring the shared mount between tests. A
// restore that outlives it is stuck on the mount, which is then wedged.
const sharedRestoreTimeout = 2 * time.Minute

// TestE2ESharedMount runs every e2e test that needs only a mounted repo in its
// seeded state on one mount, because each mount counts toward the series after
// which macFUSE's FSKit extension can stop loading (see e2eMountsStarted). A
// test whose subject is mounting, restarting, or the state of a fresh mount
// keeps its own mount.
func TestE2ESharedMount(t *testing.T) {
	shared := newSharedE2ERepo(t)
	shared.run(t, "GitCleanState", testE2EGitCleanState)
	shared.run(t, "GitConcurrentReads", testE2EGitConcurrentReads)
	shared.run(t, "HEADReconcileRetriesAfterSnapshotLock", testE2EHEADReconcileRetriesAfterSnapshotLock)
	shared.run(t, "GitStatusDetectsSameSizeRewriteAfterMtimeRestore", testE2EGitStatusDetectsSameSizeRewriteAfterMtimeRestore)
	shared.run(t, "GitStatusPorcelain", testE2EGitStatusPorcelain)
	shared.run(t, "GitRenameTrackedDirectory", testE2EGitRenameTrackedDirectory)
	shared.run(t, "GitCheckoutBranchSwitch", testE2EGitCheckoutBranchSwitch)
	shared.run(t, "GitCheckoutConflictKeepsLocalChanges", testE2EGitCheckoutConflictKeepsLocalChanges)
	shared.run(t, "GitCheckoutRestorePath", testE2EGitCheckoutRestorePath)
	shared.run(t, "GitMergeAndRebase", testE2EGitMergeAndRebase)
	shared.run(t, "GitCommitModifyTrackedFile", testE2EGitCommitModifyTrackedFile)
	shared.run(t, "GitCommitPreservesUnstagedAfterStagedCommit", testE2EGitCommitPreservesUnstagedAfterStagedCommit)
	shared.run(t, "GitCommitTrackedRename", testE2EGitCommitTrackedRename)
	shared.run(t, "GitCommitTrackedDelete", testE2EGitCommitTrackedDelete)
	shared.run(t, "GitCommitExecutableBit", testE2EGitCommitExecutableBit)
	shared.run(t, "GitCommitSymlink", testE2EGitCommitSymlink)
	shared.run(t, "GitResetHardCleanAndStash", testE2EGitResetHardCleanAndStash)
	shared.run(t, "GitPullFastForward", testE2EGitPullFastForward)
	shared.run(t, "GitPush", testE2EGitPush)
	t.Run("FilesystemDirectoryMoveWorkflows", func(t *testing.T) { testE2EFilesystemDirectoryMoveWorkflows(t, shared) })
	shared.run(t, "FilesystemDirectoryRenameConcurrentAccess", testE2EFilesystemDirectoryRenameConcurrentAccess)
}

// sharedE2ERepo is a mounted repo that tests take turns on. After each test it
// restores the state the mount came up in, and verifies that state before the
// next test runs.
type sharedE2ERepo struct {
	repo   *mountedE2ERepo
	gitDir string
	// remoteGitDir is the local bare origin, or empty when AFS_E2E_REPO names
	// a remote that no test writes to.
	remoteGitDir string
	head         string
	// headRef is how daemon status names the branch HEAD is on.
	headRef    string
	refs       string
	remoteRefs string
	// emptyCommit has an empty tree. While HEAD is on it, the daemon
	// reconciles every overlay entry against a base without that path.
	emptyCommit string
	// broken names the test after which a restore failed. Later tests skip.
	broken string
}

type overlayRow struct{ path, kind string }

func newSharedE2ERepo(t *testing.T) *sharedE2ERepo {
	t.Helper()
	repo := newMountedE2ERepo(t)
	// The shared daemon can outlive the 5-minute refresh interval. A
	// background fetch would then move remote-tracking refs while a test or a
	// restore runs.
	if err := repo.svc.SetRefresh(context.Background(), repoName, 24*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	s := &sharedE2ERepo{repo: repo, gitDir: filepath.Join(repo.root, "repos", repoName, "git")}
	var err error
	if s.head, err = gitOutput(repo.mountPath, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	st, err := repo.svc.Status(context.Background(), repoName)
	if err != nil {
		t.Fatal(err)
	}
	s.headRef = st.CurrentHEADRef
	if s.refs, err = listRefs(s.gitDir); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AFS_E2E_REPO") == "" {
		remoteURL, err := gitOutput("", "--git-dir", s.gitDir, "remote", "get-url", "origin")
		if err != nil {
			t.Fatal(err)
		}
		s.remoteGitDir = strings.TrimPrefix(remoteURL, "file://")
		if s.remoteRefs, err = listRefs(s.remoteGitDir); err != nil {
			t.Fatal(err)
		}
	}
	emptyTree, err := gitOutput("", "--git-dir", s.gitDir, "mktree")
	if err != nil {
		t.Fatal(err)
	}
	if s.emptyCommit, err = gitOutput("", "--git-dir", s.gitDir,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit-tree", emptyTree, "-m", "empty tree for restoring the shared mount",
	); err != nil {
		t.Fatal(err)
	}
	// A fresh mount must already be in the state each test starts from,
	// including a clean Git status.
	if err := s.verify(); err != nil {
		t.Fatalf("fresh mount: %v", err)
	}
	repo.logs.Reset()
	return s
}

// run runs test as a subtest on the shared mount, then restores the mount.
func (s *sharedE2ERepo) run(t *testing.T, name string, test func(*testing.T, *mountedE2ERepo)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		skipIfMountWedged(t)
		if s.broken != "" {
			t.Skipf("skipping: the shared mount could not be restored after %s", s.broken)
		}
		t.Cleanup(func() { s.release(t) })
		test(t, s.repo)
	})
}

// release restores the shared mount after t. A restore that fails fails t,
// whose changes it could not undo, and skips the tests after it. A restore
// that stalls marks the mount wedged.
func (s *sharedE2ERepo) release(t *testing.T) {
	t.Helper()
	if wedgedMount.Load() != nil {
		return
	}
	restored := make(chan error, 1)
	go func() { restored <- s.restore() }()
	select {
	case err := <-restored:
		if err != nil {
			s.broken = t.Name()
			t.Errorf("restore the shared mount for the next test: %v", err)
		}
	case <-time.After(sharedRestoreTimeout):
		t.Errorf("restore the shared mount for the next test: %v", markMountWedged(s.repo.mountPath))
	}
}

// restore returns the mount to the state it came up in: HEAD on main at the
// seeded commit, the seeded refs here and in origin, no overlay entries, and
// an index that matches HEAD.
func (s *sharedE2ERepo) restore() error {
	mountPath := s.repo.mountPath
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(s.gitDir, dir)); err == nil {
			if _, err := gitOutput(mountPath, "rebase", "--quit"); err != nil {
				return err
			}
			break
		}
	}
	if s.remoteGitDir != "" {
		if err := restoreRefs(s.remoteGitDir, s.remoteRefs); err != nil {
			return fmt.Errorf("restore origin refs: %w", err)
		}
	}
	if _, err := gitOutput("", "--git-dir", s.gitDir, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	if err := restoreRefs(s.gitDir, s.refs); err != nil {
		return fmt.Errorf("restore refs: %w", err)
	}
	if err := s.waitForDaemonHEAD(s.head); err != nil {
		return err
	}
	if err := s.discardOverlay(); err != nil {
		return err
	}
	// A mixed reset sets the index to HEAD without writing the worktree, and
	// clears merge and cherry-pick state.
	if _, err := gitOutput(mountPath, "reset", "-q"); err != nil {
		return err
	}
	if err := s.verify(); err != nil {
		return err
	}
	s.repo.logs.Reset()
	return nil
}

// discardOverlay leaves the overlay without entries. Restoring files with Git
// writes them through the overlay, and the daemon keeps an entry that matches
// its base until HEAD moves. Removing each entry's path turns it into a delete
// marker, and the daemon drops every delete marker when HEAD moves to a tree
// without that path.
func (s *sharedE2ERepo) discardOverlay() error {
	entries, err := s.overlayEntries()
	if err != nil || len(entries) == 0 {
		return err
	}
	for _, e := range entries {
		if e.path == "." || e.path == ".git" || strings.HasPrefix(e.path, ".git/") || !filepath.IsLocal(e.path) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.repo.mountPath, e.path)); err != nil {
			return fmt.Errorf("remove overlay %s entry %s: %w", e.kind, e.path, err)
		}
	}
	for _, move := range []struct{ to, from string }{{s.emptyCommit, s.head}, {s.head, s.emptyCommit}} {
		if _, err := gitOutput("", "--git-dir", s.gitDir, "update-ref", "refs/heads/main", move.to, move.from); err != nil {
			return err
		}
		if err := s.waitForDaemonHEAD(move.to); err != nil {
			return err
		}
	}
	return nil
}

// verify reports how the mount differs from the state it came up in.
func (s *sharedE2ERepo) verify() error {
	if entries, err := s.overlayEntries(); err != nil {
		return err
	} else if len(entries) != 0 {
		return fmt.Errorf("overlay entries = %v, want none", entries)
	}
	st, err := s.repo.svc.Status(context.Background(), repoName)
	if err != nil {
		return err
	}
	if st.State != "mounted" || st.MountError != "" || st.CurrentHEADOID != s.head || st.CurrentHEADRef != s.headRef || st.DirtyOverlay {
		return fmt.Errorf("daemon status = %+v, want mounted on %s at %s with a clean overlay", st, s.headRef, s.head)
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"symbolic-ref", "HEAD"}, "refs/heads/main"},
		{[]string{"rev-parse", "HEAD"}, s.head},
		{[]string{"status", "--short", "--untracked-files=all"}, ""},
		{[]string{"diff", "--quiet"}, ""},
		{[]string{"diff", "--cached", "--quiet"}, ""},
	} {
		got, err := gitOutput(s.repo.mountPath, check.args...)
		if err != nil {
			return err
		}
		if got != check.want {
			return fmt.Errorf("git %s = %q, want %q", strings.Join(check.args, " "), got, check.want)
		}
	}
	for _, refs := range []struct{ gitDir, want string }{{s.gitDir, s.refs}, {s.remoteGitDir, s.remoteRefs}} {
		if refs.gitDir == "" {
			continue
		}
		got, err := listRefs(refs.gitDir)
		if err != nil {
			return err
		}
		if got != refs.want {
			return fmt.Errorf("refs in %s:\n%s\nwant:\n%s", refs.gitDir, got, refs.want)
		}
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(s.gitDir, name)); err == nil {
			return fmt.Errorf("%s remains in %s", name, s.gitDir)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *sharedE2ERepo) waitForDaemonHEAD(oid string) error {
	return pollCondition(15*time.Second, "daemon HEAD on "+s.headRef+" at "+oid, func() (bool, string) {
		st, err := s.repo.svc.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		return st.CurrentHEADOID == oid && st.CurrentHEADRef == s.headRef, fmt.Sprintf("head=%s ref=%s", st.CurrentHEADOID, st.CurrentHEADRef)
	})
}

func (s *sharedE2ERepo) overlayEntries() ([]overlayRow, error) {
	db, err := meta.OpenDB(filepath.Join(s.repo.root, "overlays", repoName, "meta.sqlite"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT path, kind FROM overlay_entries ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []overlayRow
	for rows.Next() {
		var e overlayRow
		if err := rows.Scan(&e.path, &e.kind); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// listRefs lists every ref in gitDir with its object and, for a symbolic ref, its target.
func listRefs(gitDir string) (string, error) {
	return gitOutput("", "--git-dir", gitDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
}

// restoreRefs makes the refs in gitDir match want, a listRefs listing. It
// leaves symbolic refs to resolve through the refs they name.
func restoreRefs(gitDir, want string) error {
	got, err := listRefs(gitDir)
	if err != nil {
		return err
	}
	wantRefs, gotRefs := parseRefs(want), parseRefs(got)
	for _, ref := range gotRefs {
		if _, ok := refByName(wantRefs, ref.name); !ok {
			if _, err := gitOutput("", "--git-dir", gitDir, "update-ref", "--no-deref", "-d", ref.name); err != nil {
				return err
			}
		}
	}
	for _, ref := range wantRefs {
		current, ok := refByName(gotRefs, ref.name)
		if ref.symref != "" || (ok && current.oid == ref.oid) {
			continue
		}
		if _, err := gitOutput("", "--git-dir", gitDir, "update-ref", "--no-deref", ref.name, ref.oid); err != nil {
			return err
		}
	}
	return nil
}

type gitRef struct{ name, oid, symref string }

func parseRefs(listing string) []gitRef {
	var refs []gitRef
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ref := gitRef{name: fields[0], oid: fields[1]}
		if len(fields) > 2 {
			ref.symref = fields[2]
		}
		refs = append(refs, ref)
	}
	return refs
}

func refByName(refs []gitRef, name string) (gitRef, bool) {
	for _, ref := range refs {
		if ref.name == name {
			return ref, true
		}
	}
	return gitRef{}, false
}

// gitOutput runs git in dir and returns its trimmed output.
func gitOutput(dir string, args ...string) (string, error) {
	stdout, stderr, err := gitCmdResult(dir, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}
