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
	"github.com/cloudflare/artifact-fs/internal/model"
)

// sharedRestoreTimeout bounds restoring the shared mount between tests. A
// restore that outlives it is stuck on the mount, which is then wedged.
const sharedRestoreTimeout = 2 * time.Minute

// kernelCacheExpiry is longer than the 1-second entry and attribute expiry that
// internal/fusefs gives the kernel. Nothing invalidates those caches when the
// daemon switches snapshots, so a stat within that second can see the old one.
const kernelCacheExpiry = 1500 * time.Millisecond

// TestE2ESharedMount runs every e2e test that needs only a mounted repo in its
// seeded state on one mount, because each mount counts toward the series after
// which macFUSE's FSKit extension can stop loading (see e2eMountsStarted). A
// test whose subject is mounting, restarting, or the state of a fresh mount
// keeps its own mount. On FSKit only the runReadOnly tests run.
func TestE2ESharedMount(t *testing.T) {
	shared := newSharedE2ERepo(t)
	shared.runReadOnly(t, "GitCleanState", testE2EGitCleanState)
	shared.runReadOnly(t, "GitConcurrentReads", testE2EGitConcurrentReads)
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
	headRef string
	// generation is the snapshot generation the daemon served when the
	// mount was last handed to a test.
	generation int64
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
	if err := repo.ctl.SetRefresh(context.Background(), repoName, model.RefreshPolicy{RefreshInterval: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	s := &sharedE2ERepo{repo: repo, gitDir: filepath.Join(repo.root, "repos", repoName, "git")}
	var err error
	if s.head, err = gitOutput(repo.mountPath, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	st, err := repo.ctl.Status(context.Background(), repoName)
	if err != nil {
		t.Fatal(err)
	}
	s.headRef = st.CurrentHEADRef
	s.generation = st.SnapshotGeneration
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
// Undoing the changes of test moves HEAD, so run skips test on FSKit.
func (s *sharedE2ERepo) run(t *testing.T, name string, test func(*testing.T, *mountedE2ERepo)) {
	t.Helper()
	s.runSubtest(t, name, test, true)
}

// runReadOnly runs a test that leaves the mount in the state it came up in.
// Restoring after that test moves no HEAD, so runReadOnly also runs it on FSKit.
func (s *sharedE2ERepo) runReadOnly(t *testing.T, name string, test func(*testing.T, *mountedE2ERepo)) {
	t.Helper()
	s.runSubtest(t, name, test, false)
}

func (s *sharedE2ERepo) runSubtest(t *testing.T, name string, test func(*testing.T, *mountedE2ERepo), needsRestore bool) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		skipIfMountWedged(t)
		if s.broken != "" {
			t.Skipf("skipping: the shared mount could not be restored after %s", s.broken)
		}
		if needsRestore {
			skipIfFSKitHEADMove(t, "restoring the shared mount after this test")
		}
		t.Cleanup(func() { s.release(t, needsRestore) })
		test(t, s.repo)
	})
}

// release restores the shared mount after t. A restore that fails fails t,
// whose changes it could not undo, and skips the tests after it. A restore
// that stalls marks the mount wedged. When t needs no restore, release first
// fails t if it changed the mount, because on FSKit restoring that change
// would move HEAD.
func (s *sharedE2ERepo) release(t *testing.T, needsRestore bool) {
	t.Helper()
	if wedgedMount.Load() != nil {
		return
	}
	restored := make(chan error, 1)
	go func() {
		if !needsRestore {
			if err := s.verifyUnchanged(); err != nil {
				restored <- fmt.Errorf("the test is registered with runReadOnly but changed the mount: %w", err)
				return
			}
		}
		restored <- s.restore()
	}()
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
	// A test that moved HEAD may leave a reconcile in flight that daemon
	// status does not show yet.
	head, headErr := gitOutput("", "--git-dir", s.gitDir, "rev-parse", "HEAD")
	ref, refErr := gitOutput("", "--git-dir", s.gitDir, "symbolic-ref", "-q", "HEAD")
	headMoved := headErr != nil || refErr != nil || head != s.head || ref != "refs/heads/main"
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
	st, err := s.repo.ctl.Status(context.Background(), repoName)
	if err != nil {
		return err
	}
	if headMoved || st.SnapshotGeneration != s.generation {
		if st, err = s.settle(); err != nil {
			return err
		}
	}
	// A mixed reset sets the index to HEAD without writing the worktree, and
	// clears merge and cherry-pick state.
	if _, err := gitOutput(mountPath, "reset", "-q"); err != nil {
		return err
	}
	if err := s.verify(); err != nil {
		return err
	}
	s.generation = st.SnapshotGeneration
	s.repo.logs.Reset()
	return nil
}

// settle waits until the daemon serves s.head and keeps one snapshot for
// longer than kernelCacheExpiry, so the kernel holds no entry or attribute
// from an earlier snapshot. It returns the daemon status at that point.
func (s *sharedE2ERepo) settle() (model.RepoRuntimeState, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := s.waitForDaemonHEAD(s.head); err != nil {
			return model.RepoRuntimeState{}, err
		}
		before, err := s.repo.ctl.Status(context.Background(), repoName)
		if err != nil {
			return model.RepoRuntimeState{}, err
		}
		time.Sleep(kernelCacheExpiry)
		after, err := s.repo.ctl.Status(context.Background(), repoName)
		if err != nil {
			return model.RepoRuntimeState{}, err
		}
		if after.CurrentHEADOID == s.head && after.CurrentHEADRef == s.headRef && after.SnapshotGeneration == before.SnapshotGeneration {
			return after, nil
		}
		if time.Now().After(deadline) {
			return model.RepoRuntimeState{}, fmt.Errorf("daemon did not settle on %s at %s: generation %d became %d, head %s on %s",
				s.headRef, s.head, before.SnapshotGeneration, after.SnapshotGeneration, after.CurrentHEADOID, after.CurrentHEADRef)
		}
	}
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

// verifyUnchanged reports how a test changed the mount, including a HEAD move
// that the test undid: the daemon publishes a snapshot generation for each HEAD
// it moves to.
func (s *sharedE2ERepo) verifyUnchanged() error {
	st, err := s.repo.ctl.Status(context.Background(), repoName)
	if err != nil {
		return err
	}
	if st.SnapshotGeneration != s.generation {
		return fmt.Errorf("daemon snapshot generation = %d, want %d", st.SnapshotGeneration, s.generation)
	}
	return s.verify()
}

// verify reports how the mount differs from the state it came up in.
func (s *sharedE2ERepo) verify() error {
	if entries, err := s.overlayEntries(); err != nil {
		return err
	} else if len(entries) != 0 {
		return fmt.Errorf("overlay entries = %v, want none", entries)
	}
	st, err := s.repo.ctl.Status(context.Background(), repoName)
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
		st, err := s.repo.ctl.Status(context.Background(), repoName)
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
