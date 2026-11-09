package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// recordingMount stands in for mounting and keeps each mount it makes, with
// the gate the daemon mounted it behind.
type recordingMount struct {
	mu     sync.Mutex
	mounts []*fakeMountedFS
	gates  []*fusefs.ReadyGate
}

func (m *recordingMount) mount(_, _ context.Context, _ model.RepoPaths, _ *fusefs.Engine, gate *fusefs.ReadyGate, _ *slog.Logger, _ func(string) string) (fusefs.MountedFS, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mfs := &fakeMountedFS{unmounted: make(chan struct{})}
	m.mounts = append(m.mounts, mfs)
	m.gates = append(m.gates, gate)
	return mfs, nil
}

// only returns the one mount m made, which must still be mounted, and its gate.
func (m *recordingMount) only(t *testing.T) (*fakeMountedFS, *fusefs.ReadyGate) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.mounts) != 1 {
		t.Fatalf("the daemon mounted %d times, want once", len(m.mounts))
	}
	select {
	case <-m.mounts[0].unmounted:
		t.Fatal("the daemon unmounted the repo")
	default:
	}
	return m.mounts[0], m.gates[0]
}

// requireGateOpen fails t unless gate lets FUSE ops through.
func requireGateOpen(t *testing.T, gate *fusefs.ReadyGate) {
	t.Helper()
	// Wait reports an open or failed gate before it checks the context, so a
	// canceled context reads the gate's state without waiting.
	probe, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.Wait(probe); err != nil {
		t.Fatalf("gate wait = %v, want the gate open", err)
	}
}

func runningRuntime(svc *Service, id model.RepoID) *repoRuntime {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.running[id]
}

// commitFile commits content to name on repo's checked-out branch and returns
// the commit.
func commitFile(t *testing.T, repo, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", repo, "add", name)
	runCmd(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", name)
	return strings.TrimSpace(runCmdOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
}

// newBranchedRemote is a repository with a main branch and a feature branch
// one commit ahead of it, checked out on main. It returns the feature commit.
func newBranchedRemote(t *testing.T, dir string) string {
	t.Helper()
	runCmd(t, "git", "init", "--initial-branch", "main", dir)
	commitFile(t, dir, "README.md", "main\n")
	runCmd(t, "git", "-C", dir, "checkout", "-b", "feature")
	feature := commitFile(t, dir, "FEATURE.md", "feature\n")
	runCmd(t, "git", "-C", dir, "checkout", "main")
	return feature
}

func publishedHead(t *testing.T, cfg model.RepoConfig) (string, string) {
	t.Helper()
	snap, err := snapshot.New(context.Background(), cfg.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	oid, ref, _, err := snap.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return oid, ref
}

// A refresh change is not part of a repo's prepare identity, so one that lands
// while an async preparation activates its runtime must not leave the registry
// ready while the runtime's gate holds every FUSE op.
func TestSetRefreshDuringPrepareActivationOpensGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	gitDir := createPreparedGitDir(t, tmp)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	m := &recordingMount{}
	svc.mount = m.mount
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{Branch: "master", PreparedGitDir: true, FetchRef: "master"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Minute}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	svc.readyRecorded = func() {
		if err := svc.ctl().SetRefresh(ctx, cfg.Name, model.RefreshPolicy{RemoteRefreshDisabled: true}); err != nil {
			t.Errorf("set-refresh: %v", err)
		}
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.prepareWorkers.Wait()
	for range 2 {
		if err := svc.syncRepos(ctx); err != nil {
			t.Fatal(err)
		}
	}

	stored, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PrepareState != model.PrepareStateReady || !stored.RemoteRefreshDisabled {
		t.Fatalf("registry prepare state %q, refresh disabled %t; want ready, true", stored.PrepareState, stored.RemoteRefreshDisabled)
	}
	rec, _, err := svc.registry.GetRuntime(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != repoStateMounted {
		t.Fatalf("daemon state = %q, want %s", rec.State, repoStateMounted)
	}
	_, gate := m.only(t)
	requireGateOpen(t, gate)
}

// prepare on a ready verified source prepares it again behind the gate of the
// runtime that serves it, which stays mounted, also when the daemon mounted
// the repo ready after a restart.
func TestPrepareOnReadyVerifiedRepoKeepsItsMount(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		name := "same daemon"
		if restart {
			name = "after restart"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tmp := t.TempDir()
			remote := filepath.Join(tmp, "remote")
			runCmd(t, "git", "init", "--initial-branch", "main", remote)
			commit := commitFile(t, remote, "README.md", "verified\n")
			runCmd(t, "git", "-C", remote, "config", "uploadpack.allowFilter", "true")
			root := filepath.Join(tmp, "artifact-fs")

			svc, err := New(ctx, root, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = svc.Close() }()
			m := &recordingMount{}
			svc.mount = m.mount
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "verified"}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + remote, Branch: "refs/heads/main", RequiredCommit: commit, HistoryDepth: 1}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Hour}, Enabled: true}
			if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
				t.Fatal(err)
			}
			if err := svc.syncRepos(ctx); err != nil {
				t.Fatal(err)
			}
			svc.prepareWorkers.Wait()
			if restart {
				if err := svc.Close(); err != nil {
					t.Fatal(err)
				}
				svc, err = New(ctx, root, discardLogger())
				if err != nil {
					t.Fatal(err)
				}
				m = &recordingMount{}
				svc.mount = m.mount
				if err := svc.syncRepos(ctx); err != nil {
					t.Fatal(err)
				}
			}
			m.only(t)
			id := model.RepoID(cfg.Name)
			rt := runningRuntime(svc, id)
			if rt == nil || rt.reportedState() != repoStateMounted {
				t.Fatalf("runtime before prepare = %+v, want mounted", rt)
			}
			before, err := svc.registry.GetRepo(ctx, cfg.Name)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := svc.ctl().Prepare(ctx, cfg.Name); err != nil {
				t.Fatal(err)
			}
			if err := svc.syncRepos(ctx); err != nil {
				t.Fatal(err)
			}
			svc.prepareWorkers.Wait()

			_, gate := m.only(t)
			if runningRuntime(svc, id) != rt {
				t.Fatal("prepare replaced the runtime that served the repo")
			}
			requireGateOpen(t, gate)
			stored, err := svc.registry.GetRepo(ctx, cfg.Name)
			if err != nil {
				t.Fatal(err)
			}
			if stored.PrepareState != model.PrepareStateReady {
				t.Fatalf("prepare state = %q, want ready", stored.PrepareState)
			}
			svc.mu.Lock()
			served := rt.cfg.PrepareRequest
			svc.mu.Unlock()
			if stored.PrepareRequest == before.PrepareRequest || served != stored.PrepareRequest {
				t.Fatalf("prepare request before %q, stored %q, served %q; want a new request the runtime prepared", before.PrepareRequest, stored.PrepareRequest, served)
			}
			if rec, _, err := svc.registry.GetRuntime(ctx, id); err != nil || rec.State != repoStateMounted {
				t.Fatalf("daemon state = %q, %v; want %s", rec.State, err, repoStateMounted)
			}
		})
	}
}

// prepare on a sync repo the daemon serves runs in the CLI while the mount
// keeps serving, and the daemon then serves the commit it prepared.
func TestPrepareOnServedSyncRepoKeepsItsMount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	svc, ctl := newSyncedDaemonIn(t, tmp)
	m := &recordingMount{}
	svc.mount = m.mount
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	m.only(t)
	rt := runningRuntime(svc, "repo")
	if rt == nil {
		t.Fatal("the daemon does not serve the repo")
	}

	advanced := commitFile(t, filepath.Join(tmp, "remote"), "ADVANCED.md", "advanced\n")
	if _, err := ctl.Prepare(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, nil, func() bool {
		rec, _, err := svc.registry.GetRuntime(ctx, "repo")
		return err == nil && rec.ServedCommit == advanced
	})

	m.only(t)
	if runningRuntime(svc, "repo") != rt {
		t.Fatal("prepare replaced the runtime that served the repo")
	}
}

// add-repo stores every path absolute, so the daemon, which runs in another
// directory, finds the same files.
func TestAddRepoStoresAbsolutePaths(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Chdir(tmp)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := NewController(ctx, filepath.Join(tmp, "state"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", MountRoot: "mnt", BlobCacheDir: "cache", GitDir: "git"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/repo.git"}, Enabled: true}
	if err := ctl.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := ctl.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.MountPath != filepath.Join(cwd, "mnt", "repo") || got.BlobCacheDir != filepath.Join(cwd, "cache") || got.GitDir != filepath.Join(cwd, "git") {
		t.Fatalf("mount path %q, blob cache %q, git dir %q; want them under %s", got.MountPath, got.BlobCacheDir, got.GitDir, cwd)
	}
	for _, path := range []string{got.MountRoot, got.OverlayDir, got.OverlayDBPath, got.MetaDBPath} {
		if !filepath.IsAbs(path) {
			t.Fatalf("stored path %q is relative: %+v", path, got)
		}
	}
}

// A source ref named origin/<branch> names that branch on the remote, as a
// fetch ref does.
func TestAddRepoReadsOriginPrefixedRefAsBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	feature := newBranchedRemote(t, remote)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "origin/feature"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "refs/heads/feature" || got.FetchRef != "refs/heads/feature" {
		t.Fatalf("source ref %q, fetch ref %q; want refs/heads/feature for both", got.Branch, got.FetchRef)
	}
	if oid, _ := publishedHead(t, got); oid != feature {
		t.Fatalf("published commit %s, want feature %s", oid, feature)
	}
}

// A sync add-repo serves its fetch ref also when it clones, as an async one
// does.
func TestAddRepoSyncFreshCloneServesFetchRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	feature := newBranchedRemote(t, remote)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main", FetchRef: "feature"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if oid, ref := publishedHead(t, got); oid != feature || ref != "feature" {
		t.Fatalf("published %s at %q, want feature %s", oid, ref, feature)
	}
}

// A clone's refspec maps only --ref's branch, and Git sets an upstream only
// through a refspec that maps it. A preparation that moves the clone to
// another branch adds that branch to origin's refspec, so the branch tracks
// its remote branch: a plain git pull merges it rather than --ref's branch,
// git push pushes to it, and git status counts commits ahead of it.
func TestSwitchedBranchTracksItsRemoteBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	feature := newBranchedRemote(t, remote)
	bare := filepath.Join(tmp, "origin.git")
	runCmd(t, "git", "clone", "-q", "--bare", remote, bare)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + bare, Branch: "main"}, Enabled: true}
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mainTip := commitFile(t, remote, "MAIN.md", "main\n")
	runCmd(t, "git", "-C", remote, "push", "-q", bare, "main")
	cfg.FetchRef = "feature"
	if err := svc.ctl().AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	// Package tests never mount, so Git runs in a plain worktree of the git dir.
	worktree := filepath.Join(tmp, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) (string, error) {
		args = append([]string{"--git-dir", got.GitDir, "--work-tree", worktree, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)
		out, err := exec.Command("git", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := git("reset", "-q", "--hard"); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}
	// pull.rebase=false makes Git merge diverged branches instead of refusing.
	if out, err := git("-c", "pull.rebase=false", "pull", "-q"); err != nil {
		t.Errorf("git pull: %v\n%s", err, out)
	}
	if head, _ := git("rev-parse", "HEAD"); head != feature {
		t.Errorf("HEAD after git pull = %s, want feature %s; main is %s", head, feature, mainTip)
	}
	if err := os.WriteFile(filepath.Join(worktree, "LOCAL.md"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "LOCAL.md"}, {"commit", "-q", "-m", "local"}} {
		if out, err := git(args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if status, _ := git("status", "-sb"); status != "## feature...origin/feature [ahead 1]" {
		t.Errorf("git status -sb = %q, want ## feature...origin/feature [ahead 1]", status)
	}
	if out, err := git("push", "-q"); err != nil {
		t.Errorf("git push: %v\n%s", err, out)
	}
	local, _ := git("rev-parse", "HEAD")
	if pushed := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", bare, "rev-parse", "refs/heads/feature")); pushed != local {
		t.Errorf("remote feature = %s after git push, want %s", pushed, local)
	}
	if _, err := svc.ctl().Prepare(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	refspecs := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", got.GitDir, "config", "--get-all", "remote.origin.fetch"))
	if want := "+refs/heads/main:refs/remotes/origin/main\n+refs/heads/feature:refs/remotes/origin/feature"; refspecs != want {
		t.Fatalf("origin's refspecs after a second preparation:\n%s\nwant:\n%s", refspecs, want)
	}
}

// A refspec pattern that maps the branch counts, so preparation adds none.
func TestPreparationKeepsARefspecPatternThatMapsTheBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	gitDir := createPreparedGitDir(t, tmp)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{Branch: "master", PreparedGitDir: true, FetchRef: "master"}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, got, 1); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if upstream := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "rev-parse", "--abbrev-ref", "master@{upstream}")); upstream != "origin/master" {
		t.Fatalf("master's upstream = %q, want origin/master", upstream)
	}
	if refspecs := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "config", "--get-all", "remote.origin.fetch")); refspecs != "+refs/heads/*:refs/remotes/origin/*" {
		t.Fatalf("origin's refspecs = %q, want only +refs/heads/*:refs/remotes/origin/*", refspecs)
	}
}

// --depth bounds the history every preparation transfers, also when it
// fetches a fetch ref other than the source ref.
func TestSyncPreparationFetchRefKeepsHistoryDepth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	newBranchedRemote(t, remote)
	runCmd(t, "git", "-C", remote, "checkout", "feature")
	commitFile(t, remote, "MORE.md", "more\n")
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	// Git ignores --depth for a clone from a local path.
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + remote, Branch: "main", FetchRef: "feature", HistoryDepth: 1}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	commits := func() string {
		return strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-list", "--count", "HEAD"))
	}
	if got := commits(); got != "1" {
		t.Fatalf("add-repo --depth 1 transferred %s commits of the fetch ref, want 1", got)
	}
	// A later fetch adds only the new commits to the history it has.
	commitFile(t, remote, "AGAIN.md", "again\n")
	commitFile(t, remote, "LAST.md", "last\n")
	if _, err := svc.ctl().Prepare(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	if got := commits(); got != "3" {
		t.Fatalf("prepare with --depth 1 left %s commits of the fetch ref, want the 2 new ones on the 1 it had", got)
	}
}

// newPreparedGitDir returns a work repository pushing to a bare remote, and a
// git dir that already holds the remote's master branch, as a baked image
// would.
func newPreparedGitDir(t *testing.T, tmp string) (work, gitDir string) {
	t.Helper()
	bare := filepath.Join(tmp, "origin.git")
	work = filepath.Join(tmp, "work")
	gitDir = filepath.Join(tmp, "prepared.git")
	runCmd(t, "git", "init", "--bare", bare)
	runCmd(t, "git", "clone", bare, work)
	runCmd(t, "git", "-C", work, "checkout", "-b", "master")
	commitFile(t, work, "README.md", "hello\n")
	runCmd(t, "git", "-C", work, "push", "origin", "master")
	runCmd(t, "git", "init", "--separate-git-dir", gitDir, "--initial-branch", "master", filepath.Join(tmp, "prepared"))
	runCmd(t, "git", "--git-dir", gitDir, "remote", "add", "origin", "file://"+bare)
	runCmd(t, "git", "--git-dir", gitDir, "fetch", "--update-head-ok", "origin", "+refs/heads/master:refs/heads/master")
	return work, gitDir
}

// preparePreparedGitDir registers gitDir with --prepared-gitdir --depth 1,
// prepares it as the daemon does, and fails t unless its master matches
// work's.
func preparePreparedGitDir(t *testing.T, svc *Service, work, gitDir string) {
	t.Helper()
	ctx := context.Background()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{Branch: "master", PreparedGitDir: true, FetchRef: "master", HistoryDepth: 1}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, got, 1); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := strings.TrimSpace(runCmdOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	if head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "rev-parse", "refs/heads/master")); head != want {
		t.Fatalf("master = %s, want %s", head, want)
	}
}

// A fetch with --depth marks its tip shallow even when the git dir holds the
// tip's parent. The check that a prepared git dir's branch only moves forward
// then cannot see the old tip below the new one.
func TestPreparedGitDirWithDepthMovesItsExistingBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	work, gitDir := newPreparedGitDir(t, tmp)
	commitFile(t, work, "MORE.md", "more\n")
	runCmd(t, "git", "-C", work, "push", "origin", "master")
	svc, err := New(context.Background(), filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	preparePreparedGitDir(t, svc, work, gitDir)
}

// The fetch command and the refresh fetch only the fetch ref, not every
// branch the prepared git dir's refspec names, which a --depth git dir would
// receive with its whole history.
func TestFetchAndRefreshFetchOnlyTheFetchRef(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	work, gitDir := newPreparedGitDir(t, tmp)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.mount = fakeMount
	preparePreparedGitDir(t, svc, work, gitDir)
	// push pushes a new branch, then a commit to master.
	push := func(branch string) string {
		t.Helper()
		runCmd(t, "git", "-C", work, "checkout", "-q", "-b", branch, "master")
		commitFile(t, work, branch+".md", branch+"\n")
		runCmd(t, "git", "-C", work, "push", "-q", "origin", branch)
		runCmd(t, "git", "-C", work, "checkout", "-q", "master")
		master := commitFile(t, work, "after-"+branch+".md", branch+"\n")
		runCmd(t, "git", "-C", work, "push", "-q", "origin", "master")
		return master
	}
	requireFetched := func(master, branch string) {
		t.Helper()
		if got := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "rev-parse", "refs/remotes/origin/master")); got != master {
			t.Fatalf("origin/master = %s, want %s", got, master)
		}
		if err := exec.Command("git", "--git-dir", gitDir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branch).Run(); err == nil {
			t.Fatalf("fetched branch %s, which is not the fetch ref", branch)
		}
	}

	master := push("before-fetch")
	if err := svc.ctl().FetchNow(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	requireFetched(master, "before-fetch")

	master = push("before-refresh")
	if err := svc.ctl().SetRefresh(ctx, "repo", model.RefreshPolicy{RefreshInterval: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	beforeRefresh := time.Now()
	done := startService(t, svc)
	var st model.RepoRuntimeState
	waitFor(t, done, func() bool {
		st, err = svc.ctl().Status(ctx, "repo")
		return err == nil && st.BehindCount == 2
	})
	requireFetched(master, "before-refresh")
	if st.AheadCount != 0 || !st.LastFetchAt.After(beforeRefresh) || st.LastFetchResult != "ok" {
		t.Fatalf("status after the refresh = ahead %d, last fetch %s %q; want ahead 0 and a fetch after %s", st.AheadCount, st.LastFetchAt, st.LastFetchResult, beforeRefresh)
	}
}

// requireRefreshedBehind starts svc with a short refresh interval and
// checks the counts status reports after its first refresh: behind, and
// neither ahead nor diverged.
func requireRefreshedBehind(t *testing.T, svc *Service, behind int) {
	t.Helper()
	ctx := context.Background()
	if err := svc.ctl().SetRefresh(ctx, "repo", model.RefreshPolicy{RefreshInterval: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	done := startService(t, svc)
	// The record holds a refresh's counts once it holds its result.
	waitFor(t, done, func() bool {
		rec, _, err := svc.registry.GetRuntime(ctx, cfg.ID)
		return err == nil && rec.LastFetchResult != "" && rec.LastFetchResult != string(fetchNever)
	})
	st, err := svc.ctl().Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastFetchResult != string(fetchOK) {
		t.Fatalf("refresh result = %q: %s", st.LastFetchResult, st.LastError)
	}
	if st.AheadCount != 0 || st.BehindCount != behind || st.Diverged {
		t.Fatalf("status after a refresh = ahead %d, behind %d, diverged %t; want 0, %d, false", st.AheadCount, st.BehindCount, st.Diverged, behind)
	}
}

// A --prepared-gitdir registered with only --fetch-ref keeps the default
// --ref refs/heads/main, which its remote may lack. The fetch command still
// fetches the fetch ref, and ahead/behind compares HEAD with it.
func TestFetchNowOfAPreparedGitDirWithOnlyAFetchRef(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	work, gitDir := newPreparedGitDir(t, tmp)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.mount = fakeMount
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{PreparedGitDir: true, FetchRef: "master"}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, got, 1); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	moved := commitFile(t, work, "MORE.md", "more\n")
	runCmd(t, "git", "-C", work, "push", "-q", "origin", "master")
	if err := svc.ctl().FetchNow(ctx, "repo"); err != nil {
		t.Fatalf("FetchNow: %v", err)
	}
	if head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "rev-parse", "refs/remotes/origin/master")); head != moved {
		t.Fatalf("origin/master = %s, want %s", head, moved)
	}
	requireRefreshedBehind(t, svc, 1)
}

// A clone's refspec names --ref, and preparation adds another fetch ref's
// branch. The fetch command fetches only the fetch ref, which the clone
// serves, and leaves --ref's branch alone; ahead/behind compares HEAD with the
// fetch ref.
func TestFetchNowOfACloneWithAnotherFetchRef(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	newBranchedRemote(t, remote)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.mount = fakeMount
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main", FetchRef: "feature"}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	oldMain := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", got.GitDir, "rev-parse", "refs/remotes/origin/main"))
	runCmd(t, "git", "-C", remote, "checkout", "-q", "feature")
	feature := commitFile(t, remote, "NEXT.md", "next\n")
	runCmd(t, "git", "-C", remote, "checkout", "-q", "main")
	commitFile(t, remote, "MAIN.md", "main\n")
	if err := svc.ctl().FetchNow(ctx, "repo"); err != nil {
		t.Fatalf("FetchNow: %v", err)
	}
	for ref, want := range map[string]string{"refs/remotes/origin/feature": feature, "refs/remotes/origin/main": oldMain} {
		if head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", got.GitDir, "rev-parse", ref)); head != want {
			t.Fatalf("%s = %s, want %s", ref, head, want)
		}
	}
	requireRefreshedBehind(t, svc, 1)
}

// With --depth, the fetch command does not fetch --ref's branch either, which
// forks from the fetch ref below the shallow boundary and would arrive with
// its whole history.
func TestFetchNowWithDepthFetchesNoOtherBranch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "prepared.git")
	runCmd(t, "git", "init", "-q", "--bare", bare)
	runCmd(t, "git", "clone", "-q", bare, work)
	runCmd(t, "git", "-C", work, "checkout", "-q", "-b", "master")
	for i := range 8 {
		commitFile(t, work, fmt.Sprintf("c%d.md", i), "c\n")
	}
	runCmd(t, "git", "-C", work, "push", "-q", "origin", "master")
	runCmd(t, "git", "-C", work, "checkout", "-q", "-b", "main", "master~6")
	for i := range 3 {
		commitFile(t, work, fmt.Sprintf("m%d.md", i), "m\n")
	}
	runCmd(t, "git", "-C", work, "push", "-q", "origin", "main")
	runCmd(t, "git", "init", "-q", "--separate-git-dir", gitDir, "--initial-branch", "master", filepath.Join(tmp, "prepared"))
	runCmd(t, "git", "--git-dir", gitDir, "remote", "add", "origin", "file://"+bare)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{Branch: "main", PreparedGitDir: true, FetchRef: "master", HistoryDepth: 1}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, got, 1); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	before := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "rev-list", "--all", "--count"))
	if err := svc.ctl().FetchNow(ctx, "repo"); err != nil {
		t.Fatalf("FetchNow: %v", err)
	}
	if after := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "rev-list", "--all", "--count")); after != before {
		t.Fatalf("the git dir holds %s commits after the fetch, want the %s it held before", after, before)
	}
	if err := exec.Command("git", "--git-dir", gitDir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/main").Run(); err == nil {
		t.Fatal("fetched --ref's branch main, which is not the fetch ref")
	}
}

// A fetch command or refresh fetches the remote-tracking branch a later
// preparation moves the prepared branch to, so it must not mark it shallow
// either.
func TestPreparedGitDirWithDepthPreparesAgainAfterAFetch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	work, gitDir := newPreparedGitDir(t, tmp)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	preparePreparedGitDir(t, svc, work, gitDir)
	commitFile(t, work, "MORE.md", "more\n")
	runCmd(t, "git", "-C", work, "push", "origin", "master")
	if err := svc.ctl().FetchNow(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	// Registering the repo again prepares it again.
	preparePreparedGitDir(t, svc, work, gitDir)
}

// add-repo without --async clones or fetches in the CLI, where git runs in a
// background process group. A prompt there stops git until the prepare
// timeout, so a remote that asks for credentials must fail add-repo instead.
// Git falls back to SSH_ASKPASS, which IsolatePrompts records, for the prompt
// it would show on the terminal.
func TestSyncAddRepoDoesNotPrompt(t *testing.T) {
	for _, tt := range []struct {
		name string
		svc  func(t *testing.T) *Service
	}{
		{"fresh clone", func(t *testing.T) *Service {
			svc, err := New(context.Background(), filepath.Join(t.TempDir(), "artifact-fs"), discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.Close() })
			return svc
		}},
		{"existing clone", func(t *testing.T) *Service {
			svc, _ := newSyncedDaemon(t)
			return svc
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			askpassRuns := gittest.IsolatePrompts(t)
			svc := tt.svc(t)
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: gittest.UnauthorizedRemote(t), Branch: "main"}, Enabled: true}
			if err := svc.ctl().AddRepo(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
				t.Fatalf("add-repo from a remote that refuses every request = %v, want the non-interactive failure", err)
			}
			gittest.RequireNoPrompt(t, askpassRuns)
		})
	}
}

// prepare cancels the async preparation it supersedes, so the next
// preparation does not wait for a hung clone to end.
func TestPrepareCancelsTheAsyncPreparationItSupersedes(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	markers := filepath.Join(tmp, "markers")
	for _, dir := range []string{bin, markers} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A clone marks its start, hangs, then marks its end. A canceled clone
	// never marks its end.
	script := "#!/bin/sh\nif [ \"$1\" = clone ]; then\n  : > \"$AFS_MARKERS/start-$$\"\n  sleep 60\n  : > \"$AFS_MARKERS/end-$$\"\n  exit 1\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("AFS_MARKERS", markers)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	clones := func(prefix string) int {
		matches, _ := filepath.Glob(filepath.Join(markers, prefix+"-*"))
		return len(matches)
	}

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	m := &recordingMount{}
	svc.mount = m.mount
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.invalid/repo.git", Branch: "main"}, Enabled: true}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, nil, func() bool { return clones("start") == 1 })
	if _, err := svc.ctl().Prepare(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, nil, func() bool { return clones("start") == 2 })
	if clones("end") != 0 {
		t.Fatal("the next preparation cloned only after the superseded clone ended")
	}
}

// A failed prepare of a repo that was prepared before keeps the repo served,
// also after the daemon restarts, and status shows the prepare error with the
// commit the daemon serves.
func TestFailedSyncPrepareKeepsServingAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	svc, ctl := newSyncedDaemonIn(t, tmp)
	m := &recordingMount{}
	svc.mount = m.mount
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(tmp, "remote")
	served := strings.TrimSpace(runCmdOutput(t, "git", "-C", remote, "rev-parse", "HEAD"))
	if err := os.Rename(remote, filepath.Join(tmp, "remote-gone")); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Prepare(ctx, "repo"); err == nil {
		t.Fatal("prepare from a missing remote succeeded")
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	m.only(t)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	m = &recordingMount{}
	restarted.mount = m.mount
	if err := restarted.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	m.only(t)
	st, err := ctl.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateMounted || st.PrepareError == "" || st.ServedHEADOID != served {
		t.Fatalf("status after a restart = state %q, prepare error %q, served %q; want mounted, the prepare error, %s", st.State, st.PrepareError, st.ServedHEADOID, served)
	}
}

// A sync repo whose first preparation failed after its clone stays unmounted:
// no preparation ever published a snapshot of it.
func TestMountRefusesASyncRepoWhoseFirstPrepareFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	commitFile(t, remote, "README.md", "main\n")
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	m := &recordingMount{}
	svc.mount = m.mount
	// The clone succeeds, then the fetch of a ref the remote lacks fails.
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main", FetchRef: "missing"}, Enabled: true}); err == nil {
		t.Fatal("add-repo with a missing fetch ref succeeded")
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	mounts := len(m.mounts)
	m.mu.Unlock()
	if mounts != 0 {
		t.Fatalf("the daemon mounted a never-prepared repo %d times", mounts)
	}
	st, err := svc.ctl().Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastErrorPhase != phaseMount || !strings.Contains(st.LastError, "repo prepare failed") {
		t.Fatalf("status = %+v, want the refused mount", st)
	}
}

// A preparation canceled after it moved the index restores the index to
// HEAD, so the git dir stays consistent for the next preparation.
func TestCanceledPreparationRestoresTheIndex(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	base := commitFile(t, remote, "README.md", "main\n")
	clone := filepath.Join(tmp, "clone")
	runCmd(t, "git", "clone", remote, clone)
	commitFile(t, remote, "ADVANCED.md", "advanced\n")
	gitDir := filepath.Join(clone, ".git")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The branch update after the index moved hangs until the preparation is
	// canceled.
	blocked := filepath.Join(tmp, "update-ref-blocked")
	script := "#!/bin/sh\nif [ \"$1\" = update-ref ] && [ ! -e \"$AFS_BLOCKED\" ]; then\n  : > \"$AFS_BLOCKED\"\n  sleep 60\n  exit 1\nfi\nexec \"$AFS_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("AFS_BLOCKED", blocked)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = svc.Close()
		}
	}()
	svc.mount = (&recordingMount{}).mount
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + remote, Branch: "main"}, Enabled: true}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, preparationsEnded(svc, "repo"), func() bool {
		_, err := os.Stat(blocked)
		return err == nil
	})
	// Stopping the daemon cancels the preparation.
	closed = true
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(gitDir, "index.lock")); err == nil {
		t.Fatal("the canceled preparation left index.lock")
	}
	if head := strings.TrimSpace(runCmdOutput(t, realGit, "--git-dir", gitDir, "rev-parse", "HEAD")); head != base {
		t.Fatalf("HEAD = %s, want %s", head, base)
	}
	if staged := runCmdOutput(t, realGit, "--git-dir", gitDir, "diff-index", "--cached", "--name-only", "HEAD"); staged != "" {
		t.Fatalf("the index differs from HEAD in %q", staged)
	}
}

// preparationsEnded reports name's prepare state once svc's async
// preparations end, so a wait for a step inside one fails when it ends first.
// Call it after the preparation started.
func preparationsEnded(svc *Service, name string) <-chan error {
	ended := make(chan error, 1)
	go func() {
		svc.prepareWorkers.Wait()
		cfg, err := svc.registry.GetRepo(context.Background(), name)
		if err == nil {
			err = fmt.Errorf("the preparation ended %q with %q", cfg.PrepareState, cfg.PrepareError)
		}
		ended <- err
	}()
	return ended
}

// A stop's drain outlasts the rollback of the preparation it cancels.
func TestDrainOutlastsTheRollbackOfACanceledPreparation(t *testing.T) {
	t.Parallel()
	if gitstore.RollbackBound >= drainTimeout {
		t.Fatalf("gitstore.RollbackBound = %v, want below drainTimeout %v", gitstore.RollbackBound, drainTimeout)
	}
}

// A preparation canceled while git read-tree holds index.lock leaves no lock
// behind, so the next preparation succeeds.
func TestCanceledReadTreeLeavesNoIndexLock(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	commitFile(t, remote, "README.md", "main\n")
	clone := filepath.Join(tmp, "clone")
	runCmd(t, "git", "clone", remote, clone)
	advanced := commitFile(t, remote, "ADVANCED.md", "advanced\n")
	gitDir := filepath.Join(clone, ".git")

	// git read-tree runs the fsmonitor hook while it holds index.lock. The
	// hook hangs there once, until the preparation is canceled.
	blocked := filepath.Join(tmp, "read-tree-blocked")
	hook := filepath.Join(tmp, "fsmonitor-hook")
	script := "#!/bin/sh\nif [ -e \"$AFS_INDEX_LOCK\" ] && [ ! -e \"$AFS_BLOCKED\" ]; then\n  : > \"$AFS_BLOCKED\"\n  sleep 60\nfi\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", clone, "config", "core.fsmonitor", hook)
	t.Setenv("AFS_INDEX_LOCK", filepath.Join(gitDir, "index.lock"))
	t.Setenv("AFS_BLOCKED", blocked)

	root := filepath.Join(tmp, "artifact-fs")
	svc, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = svc.Close()
		}
	}()
	svc.mount = (&recordingMount{}).mount
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir}, RepoSpec: model.RepoSpec{RemoteURL: "file://" + remote, Branch: "main"}, Enabled: true}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, preparationsEnded(svc, "repo"), func() bool {
		_, err := os.Stat(blocked)
		return err == nil
	})
	// Stopping the daemon cancels the preparation.
	closed = true
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.mount = (&recordingMount{}).mount
	if err := restarted.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.prepareWorkers.Wait()
	stored, err := restarted.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PrepareState != model.PrepareStateReady {
		t.Fatalf("the next preparation ended %q with %q, want ready", stored.PrepareState, stored.PrepareError)
	}
	if head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "rev-parse", "HEAD")); head != advanced {
		t.Fatalf("HEAD = %s, want %s", head, advanced)
	}
}
