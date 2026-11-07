package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
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

func (m *recordingMount) mount(_, _ context.Context, _ model.RepoConfig, _ *fusefs.Engine, gate *fusefs.ReadyGate, _ *slog.Logger) (fusefs.MountedFS, error) {
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
	cfg := model.RepoConfig{Name: "repo", Branch: "master", RefreshInterval: time.Minute, GitDir: gitDir, PreparedGitDir: true, FetchRef: "master", Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	svc.readyRecorded = func() {
		if err := svc.ctl().SetRefresh(ctx, cfg.Name, 0, true); err != nil {
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
	for _, restart := range []bool{false, true} {
		name := "same daemon"
		if restart {
			name = "after restart"
		}
		t.Run(name, func(t *testing.T) {
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
			cfg := model.RepoConfig{Name: "verified", RemoteURL: "file://" + remote, Branch: "refs/heads/main", RequiredCommit: commit, HistoryDepth: 1, RefreshInterval: time.Hour, Enabled: true}
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

			if err := svc.ctl().Prepare(ctx, cfg.Name); err != nil {
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
			if rec, _, err := svc.registry.GetRuntime(ctx, id); err != nil || rec.State != repoStateMounted {
				t.Fatalf("daemon state = %q, %v; want %s", rec.State, err, repoStateMounted)
			}
		})
	}
}

// prepare on a sync repo the daemon serves runs in the CLI while the mount
// keeps serving, and the daemon then serves the commit it prepared.
func TestPrepareOnServedSyncRepoKeepsItsMount(t *testing.T) {
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
	if err := ctl.Prepare(ctx, "repo"); err != nil {
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
	cfg := model.RepoConfig{Name: "repo", RemoteURL: "https://example.com/repo.git", MountRoot: "mnt", BlobCacheDir: "cache", GitDir: "git", Enabled: true}
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
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	feature := newBranchedRemote(t, remote)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{Name: "repo", RemoteURL: remote, Branch: "origin/feature", Enabled: true}); err != nil {
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
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	feature := newBranchedRemote(t, remote)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{Name: "repo", RemoteURL: remote, Branch: "main", FetchRef: "feature", Enabled: true}); err != nil {
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
