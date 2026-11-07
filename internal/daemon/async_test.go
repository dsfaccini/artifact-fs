package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func TestAddRepoAsyncRegistersWithoutClone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
			ID:   "repo",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://github.com/example/repo.git",
			Branch:    "master",
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: time.Minute,
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("PrepareState = %q, want preparing", got.PrepareState)
	}
	if got.FetchRef != "refs/heads/master" {
		t.Fatalf("FetchRef = %q, want refs/heads/master", got.FetchRef)
	}
	if got.GitDir != filepath.Join(svc.root, "repos", "repo", "git") {
		t.Fatalf("GitDir = %q", got.GitDir)
	}
}

func TestAddRepoSyncReregistrationUpdatesExistingCloneBranch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	if err := os.WriteFile(filepath.Join(remote, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", remote, "add", "README.md")
	runCmd(t, "git", "-C", remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "main")
	runCmd(t, "git", "-C", remote, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(remote, "README.md"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", remote, "add", "README.md")
	runCmd(t, "git", "-C", remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "feature")
	wantOID := strings.TrimSpace(runCmdOutput(t, "git", "-C", remote, "rev-parse", "feature"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "feature"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	gotOID, _, err := svc.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if gotOID != wantOID {
		t.Fatalf("prepared HEAD = %s, want feature HEAD %s", gotOID, wantOID)
	}
}

func TestSyncRegistrationIsNotMountedWhilePreparing(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "git-started")
	release := filepath.Join(tmp, "git-release")
	gitScript := "#!/bin/sh\n: > \"$AFS_GIT_STARTED\"\nwhile [ ! -f \"$AFS_GIT_RELEASE\" ]; do sleep 0.01; done\nprintf '%s\\n' 'clone failed' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_GIT_STARTED", started)
	t.Setenv("AFS_GIT_RELEASE", release)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main"}, Enabled: true})
	}()
	waitFor(t, errCh, func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrepareState != model.PrepareStateSyncPreparing {
		t.Fatalf("PrepareState = %q, want sync-preparing", cfg.PrepareState)
	}
	status, err := svc.ctl().Status(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != string(model.PrepareStatePreparing) {
		t.Fatalf("status state = %q, want preparing", status.State)
	}
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	_, running := svc.running[cfg.ID]
	svc.mu.Unlock()
	if running {
		t.Fatal("sync-preparing repository was mounted")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil {
		t.Fatal("expected preparation failure")
	}
}

func TestSupersededPrepareDoesNotMutateExistingClone(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	runCmd(t, "git", "init", "--initial-branch", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", repo, "add", "README.md")
	runCmd(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "main")
	runCmd(t, "git", "-C", repo, "remote", "add", "origin", "https://original.example/repo.git")

	root := filepath.Join(tmp, "artifact-fs")
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			ID:     "repo",
			Name:   "repo",
			GitDir: filepath.Join(repo, ".git"),
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://stale.example/repo.git",
			Branch:    "refs/heads/main",
			FetchRef:  "main",
		},
		Enabled:       true,
		ConfigVersion: "version-1",
	}
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- svc.withRepoPrepareLock(ctx, cfg.Name, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
	}()
	<-lockHeld

	prepareErr := make(chan error, 1)
	go func() {
		prepareErr <- svc.ctl().Prepare(ctx, cfg.Name)
	}()
	waitFor(t, prepareErr, func() bool {
		current, err := svc.registry.GetRepo(ctx, cfg.Name)
		return err == nil && current.PrepareState == model.PrepareStateSyncPreparing && current.PrepareRequest != cfg.PrepareRequest
	})
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:   cfg.Name,
			GitDir: cfg.GitDir,
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://new.example/repo.git",
			Branch:    "main",
		},
		Enabled: true,
	}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	close(releaseLock)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-prepareErr; !errors.Is(err, registry.ErrRepoChanged) {
		t.Fatalf("Prepare error = %v, want ErrRepoChanged", err)
	}
	remote := strings.TrimSpace(runCmdOutput(t, "git", "-C", repo, "remote", "get-url", "origin"))
	if remote != "https://original.example/repo.git" {
		t.Fatalf("origin = %q, want original remote", remote)
	}
}

func TestRepoPrepareLockSerializesServices(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, err := NewController(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewController(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	// The lock retries on a timer, so in a synctest bubble the second service
	// is durably blocked once it has found the lock held.
	synctest.Test(t, func(t *testing.T) {
		firstEntered := make(chan struct{})
		releaseFirst := make(chan struct{})
		firstDone := make(chan error, 1)
		go func() {
			firstDone <- first.withRepoPrepareLock(ctx, "repo", func() error {
				close(firstEntered)
				<-releaseFirst
				return nil
			})
		}()
		<-firstEntered

		secondEntered := make(chan struct{})
		secondDone := make(chan error, 1)
		go func() {
			secondDone <- second.withRepoPrepareLock(ctx, "repo", func() error {
				close(secondEntered)
				return nil
			})
		}()
		synctest.Wait()
		select {
		case <-secondEntered:
			t.Fatal("second service entered preparation while the first held the repo lock")
		default:
		}
		close(releaseFirst)
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
		if err := <-secondDone; err != nil {
			t.Fatal(err)
		}
		select {
		case <-secondEntered:
		default:
			t.Fatal("second service did not enter preparation after the lock was released")
		}
	})
}

func TestAddRepoAsyncRejectsInlineCredentials(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		svc.mu.Lock()
		delete(svc.preparing, model.RepoID("repo"))
		svc.mu.Unlock()
		_ = svc.Close()
	}()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
			ID:   "repo",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://token@example.com/org/repo.git",
			Branch:    "master",
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err == nil {
		t.Fatal("expected inline credential error")
	}
}

func TestPrepareAsyncRejectsPersistedInlineCredentialsBeforeClone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
			ID:   "repo",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://token@example.com/org/repo.git",
			Branch:    "master",
			FetchRef:  "master",
		},
		PrepareState: model.PrepareStatePreparing,
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: time.Minute,
		},
		Enabled: true,
	}
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.prepareAsync(ctx, got, 1); err == nil {
		t.Fatal("expected prepareAsync failure")
	}
	if _, err := os.Stat(got.GitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git dir stat = %v, want not exist", err)
	}
	got, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStateFailed {
		t.Fatalf("PrepareState = %q, want failed", got.PrepareState)
	}
	if strings.Contains(got.PrepareError, "token") {
		t.Fatalf("PrepareError was not redacted: %q", got.PrepareError)
	}
}

// A preparation of an existing clone rejects a remote with inline credentials
// before it points the clone's origin at the remote.
func TestPrepareAsyncExistingCloneRejectsCredentialedRemoteBeforeSetURL(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	gitDir := filepath.Join(tmp, "repo.git")
	worktree := filepath.Join(tmp, "worktree")
	runCmd(t, "git", "init", "--separate-git-dir", gitDir, "--initial-branch", "master", worktree)
	runCmd(t, "git", "-C", worktree, "remote", "add", "origin", "https://github.com/org/repo.git")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "set-url-invoked")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ncase \"$*\" in *\"remote set-url\"*) : > \"$GIT_SET_URL_MARKER\";; esac\nexec \"$AFS_REAL_GIT\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("GIT_SET_URL_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for i, remote := range []string{
		"ssh:/git:secret@github.com/org/repo.git",
		"ssh//git:secret@github.com/org/repo.git",
		"ssh:/git:pa/ss@github.com/org/repo.git",
		"alice:ghp_secret@github.com:org/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			name := fmt.Sprintf("repo%d", i)
			cfg := model.RepoConfig{
				RepoPaths: model.RepoPaths{
					Name:   name,
					ID:     model.RepoID(name),
					GitDir: gitDir,
				},
				RepoSpec: model.RepoSpec{
					RemoteURL: remote,
					Branch:    "master",
					FetchRef:  "master",
				},
				PrepareState: model.PrepareStatePreparing,
				RefreshPolicy: model.RefreshPolicy{
					RefreshInterval: time.Minute,
				},
				Enabled: true,
			}
			svc.fillPaths(&cfg)
			if err := svc.registry.AddRepo(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			got, err := svc.registry.GetRepo(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			err = svc.prepareAsync(ctx, got, 1)
			if err == nil {
				t.Fatal("expected credentialed remote rejection")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "pa/ss") {
				t.Fatalf("error leaked credential: %v", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("git remote set-url was invoked before rejecting credentialed remote")
			}
			got, err = svc.registry.GetRepo(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if got.PrepareState != model.PrepareStateFailed {
				t.Fatalf("PrepareState = %q, want failed", got.PrepareState)
			}
			if strings.Contains(got.PrepareError, "secret") || strings.Contains(got.PrepareError, "pa/ss") {
				t.Fatalf("PrepareError leaked credential: %q", got.PrepareError)
			}
		})
	}
}

func TestAddRepoPreparedGitDirValidation(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
			ID:   "repo",
		},
		RepoSpec: model.RepoSpec{
			Branch:         "master",
			PreparedGitDir: true,
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{}); err == nil {
		t.Fatal("expected --prepared-gitdir requires --async error")
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err == nil {
		t.Fatal("expected --git-dir required error")
	}
}

func TestSyncReposSkipsResetWhilePrepareWorkerInFlight(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		svc.mu.Lock()
		delete(svc.preparing, model.RepoID("repo"))
		svc.mu.Unlock()
		_ = svc.Close()
	}()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
			ID:   "repo",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://github.com/example/repo.git",
			Branch:    "master",
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}

	gate := fusefs.NewReadyGate(true)
	rt := &repoRuntime{
		cfg:    cfg,
		gate:   gate,
		active: true,
		mount:  mountServing,
	}
	svc.mu.Lock()
	svc.running[cfg.ID] = rt
	svc.preparing[cfg.ID] = prepareWorker{token: 1}
	svc.mu.Unlock()

	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rt.reportedState(); got != repoStateMounted {
		t.Fatalf("runtime state = %q, want mounted", got)
	}
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("gate was reset while prepare worker was in flight: %v", err)
	}
}

func TestDrivePrepareSkipsStalePreparingSnapshot(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		svc.mu.Lock()
		delete(svc.running, model.RepoID("repo"))
		delete(svc.preparing, model.RepoID("repo"))
		svc.mu.Unlock()
		_ = svc.Close()
	}()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
			ID:   "repo",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://github.com/example/repo.git",
			Branch:    "master",
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	latest, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.UpdatePrepareStateForRequest(ctx, latest, model.PrepareStateReady, ""); err != nil {
		t.Fatal(err)
	}
	stale := latest
	stale.PrepareState = model.PrepareStatePreparing

	gate := fusefs.NewReadyGate(true)
	rt := &repoRuntime{
		cfg:    latest,
		gate:   gate,
		active: true,
		mount:  mountServing,
	}
	svc.mu.Lock()
	svc.running[latest.ID] = rt
	svc.mu.Unlock()

	svc.drivePrepare(ctx, stale, rt, false)
	if got := rt.reportedState(); got != repoStateMounted {
		t.Fatalf("runtime state = %q, want mounted", got)
	}
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("gate was reset from stale registry snapshot: %v", err)
	}
	svc.mu.Lock()
	_, preparing := svc.preparing[latest.ID]
	svc.mu.Unlock()
	if preparing {
		t.Fatal("started prepare worker from stale registry snapshot")
	}
}

func TestPrepareAsyncFailurePersistsRedactedError(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:   "repo",
			ID:     "repo",
			GitDir: filepath.Join(t.TempDir(), "missing.git"),
		},
		RepoSpec: model.RepoSpec{
			Branch:         "master",
			PreparedGitDir: true,
			FetchRef:       "master",
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, got, 1); err == nil {
		t.Fatal("expected prepareAsync failure")
	}
	got, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStateFailed {
		t.Fatalf("PrepareState = %q, want failed", got.PrepareState)
	}
	if got.PrepareError == "" {
		t.Fatal("PrepareError is empty, want persisted failure")
	}

	if err := svc.setPrepareState(ctx, got, model.PrepareStateFailed, errors.New("clone https://token@example.com/org/repo.git failed")); err != nil {
		t.Fatal(err)
	}
	got, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.PrepareError, "token") {
		t.Fatalf("PrepareError was not redacted: %q", got.PrepareError)
	}
}

func TestStartPrepareWorkerTimesOutAndPersistsFailed(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cloneStarted := filepath.Join(t.TempDir(), "clone-started")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(markCloneStartScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_STARTED", cloneStarted)

	var logs bytes.Buffer
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	expirePrepareOnFile(svc, cloneStarted)
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
			ID:   "repo",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://github.com/example/repo.git",
			Branch:    "master",
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	svc.startPrepareWorker(ctx, got)

	got = waitForPrepareState(t, svc, "repo", model.PrepareStateFailed)
	if got.PrepareError != "prepare timed out" {
		t.Fatalf("PrepareError = %q, want timeout", got.PrepareError)
	}
	waitFor(t, nil, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		_, preparing := svc.preparing[got.ID]
		return !preparing
	})
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logRepoPreparationStarted+`"`) ||
		!strings.Contains(logOutput, `"msg":"`+logRepoPreparationFailed+`"`) ||
		!strings.Contains(logOutput, `"mode":"`+prepareModeAsync+`"`) ||
		!strings.Contains(logOutput, `"phase":"`+preparePhaseClone+`"`) ||
		!strings.Contains(logOutput, `"state":"`+prepareLogStateFailed+`"`) ||
		!strings.Contains(logOutput, `"deadline_set":true`) ||
		!strings.Contains(logOutput, `"timed_out":true`) {
		t.Fatalf("async timeout lifecycle logs incomplete: %s", logOutput)
	}
}

func TestMountRepoPreparationTimesOutAndReleasesLock(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitStarted := filepath.Join(tmp, "git-started")
	gitFinished := filepath.Join(tmp, "git-finished")
	// The git call finishes only if the prepare deadline fails to kill it.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n: > \"$AFS_GIT_STARTED\"\nsleep 60\n: > \"$AFS_GIT_FINISHED\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_GIT_STARTED", gitStarted)
	t.Setenv("AFS_GIT_FINISHED", gitFinished)

	var logs bytes.Buffer
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	expirePrepareOnFile(svc, gitStarted)
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			ID:   "repo",
			Name: "repo",
		},
		RepoSpec: model.RepoSpec{
			Branch: "main",
		},
		Enabled:       true,
		ConfigVersion: "v1",
	}
	svc.fillPaths(&cfg)
	if err := os.MkdirAll(cfg.GitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if err := svc.syncRepos(ctx); err != nil {
		t.Fatalf("syncRepos: %v", err)
	}
	if _, err := os.Stat(gitFinished); err == nil {
		t.Fatal("git ran to completion; the prepare deadline did not interrupt it")
	}
	failure := findJSONLogRecord(t, logs.Bytes(), "repo mount failed")
	if got := failure["repo"]; got != "repo" {
		t.Fatalf("mount failure repo = %v, want repo", got)
	}
	if got, _ := failure["error"].(string); !strings.Contains(got, context.DeadlineExceeded.Error()) {
		t.Fatalf("mount failure error = %q, want deadline exceeded", got)
	}
	if err := svc.withRepoPrepareLock(ctx, cfg.Name, func() error { return nil }); err != nil {
		t.Fatalf("prepare lock remained held after timeout: %v", err)
	}
}

func TestAddRepoSyncFailureIsLoggedAndPersisted(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitPath := filepath.Join(bin, "git")
	gitScript := "#!/bin/sh\nprintf '%s\\n' 'fatal: Authentication failed for https://credential-user:super-secret@example.com/org/repo.git; HTTP 401' >&2\nexit 1\n"
	if err := os.WriteFile(gitPath, []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	err = svc.ctl().AddRepo(ctx, model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "repo",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL: "https://credential-user:super-secret@example.com/org/repo.git",
			Branch:    "main",
		},
		Enabled: true,
	})
	if err == nil {
		t.Fatal("expected synchronous preparation failure")
	}
	got, getErr := svc.registry.GetRepo(ctx, "repo")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.PrepareState != "" {
		t.Fatalf("PrepareState = %q, want synchronous state", got.PrepareState)
	}
	if shouldMountAsync(got) {
		t.Fatal("synchronous failure was reclassified as an async repository")
	}
	if strings.Contains(got.PrepareError, "credential-user") || strings.Contains(got.PrepareError, "super-secret") || strings.Contains(got.PrepareError, "example.com") || strings.Contains(got.PrepareError, "org/repo.git") {
		t.Fatalf("PrepareError leaked remote details: %q", got.PrepareError)
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, `"msg":"`+logRepoPreparationStarted+`"`) ||
		!strings.Contains(logOutput, `"msg":"`+logRepoPreparationFailed+`"`) ||
		!strings.Contains(logOutput, `"mode":"`+prepareModeSync+`"`) ||
		!strings.Contains(logOutput, `"phase":"`+preparePhaseClone+`"`) ||
		!strings.Contains(logOutput, `"state":"`+prepareLogStateFailed+`"`) ||
		!strings.Contains(logOutput, `"deadline_set":true`) ||
		strings.Contains(logOutput, "credential-user") || strings.Contains(logOutput, "super-secret") || strings.Contains(logOutput, "example.com") || strings.Contains(logOutput, "org/repo.git") {
		t.Fatalf("sync preparation logs incomplete or unsafe: %s", logOutput)
	}
	status, statusErr := svc.ctl().Status(ctx, "repo")
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if status.PrepareError == "" {
		t.Fatal("Status PrepareError is empty after synchronous failure")
	}
}

func TestAddRepoSyncTimeoutIsLoggedAndPersisted(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cloneStarted := filepath.Join(tmp, "clone-started")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(markCloneStartScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_STARTED", cloneStarted)

	var logs bytes.Buffer
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	expirePrepareOnFile(svc, cloneStarted)
	defer svc.Close()

	err = svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main"}, Enabled: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AddRepo error = %v, want deadline exceeded", err)
	}
	got, getErr := svc.registry.GetRepo(ctx, "repo")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.PrepareState != "" || got.PrepareError != "prepare timed out" {
		t.Fatalf("prepare state/error = %q/%q, want synchronous/prepare timed out", got.PrepareState, got.PrepareError)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logRepoPreparationFailed+`"`) ||
		!strings.Contains(logOutput, `"mode":"`+prepareModeSync+`"`) ||
		!strings.Contains(logOutput, `"phase":"`+preparePhaseClone+`"`) ||
		!strings.Contains(logOutput, `"timed_out":true`) {
		t.Fatalf("sync timeout lifecycle logs incomplete: %s", logOutput)
	}
}

func TestAddRepoSyncCancellationIsLoggedAndPersisted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "git-started")
	gitScript := "#!/bin/sh\n: > \"$AFS_GIT_STARTED\"\nexec sleep 10\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_GIT_STARTED", started)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	svc, err := New(context.Background(), filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main"}, Enabled: true})
	}()
	waitFor(t, errCh, func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("AddRepo error = %v, want canceled", err)
	}
	got, err := svc.registry.GetRepo(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareError != "prepare canceled" || got.PrepareState != "" {
		t.Fatalf("canceled prepare state/error = %q/%q, want synchronous/prepare canceled", got.PrepareState, got.PrepareError)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logRepoPreparationCanceled+`"`) ||
		!strings.Contains(logOutput, `"state":"`+prepareLogStateCanceled+`"`) ||
		!strings.Contains(logOutput, `"canceled":true`) {
		t.Fatalf("sync cancellation lifecycle log incomplete: %s", logOutput)
	}
}

func TestAddRepoSyncBuildTreeCancellationIsPersisted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	runCmd(t, "git", "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", repo, "add", "README.md")
	runCmd(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "ls-tree-started")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = 'ls-tree' ]; then : > \"$AFS_GIT_STARTED\"; exec sleep 10; fi\n" +
		"exec \"$AFS_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("AFS_GIT_STARTED", started)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	svc, err := New(context.Background(), filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: filepath.Join(repo, ".git")}, RepoSpec: model.RepoSpec{Branch: "master"}, Enabled: true})
	}()
	waitFor(t, errCh, func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("AddRepo error = %v, want canceled", err)
	}
	got, err := svc.registry.GetRepo(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareError != "prepare canceled" {
		t.Fatalf("PrepareError = %q, want prepare canceled", got.PrepareError)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"phase":"`+preparePhaseBuildTree+`"`) ||
		!strings.Contains(logOutput, `"state":"`+prepareLogStateCanceled+`"`) {
		t.Fatalf("build-tree cancellation log incomplete: %s", logOutput)
	}
}

func TestSyncPrepareRetryPersistsCurrentFailure(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' 'new retry failure' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", ID: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main"}, PrepareError: "old failure", Enabled: true}
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if err := svc.ctl().Prepare(ctx, "repo"); err == nil {
		t.Fatal("expected prepare retry failure")
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.PrepareError, "new retry failure") || strings.Contains(got.PrepareError, "old failure") {
		t.Fatalf("PrepareError = %q, want current retry failure", got.PrepareError)
	}
}

func TestPrepareAsyncReportsRemoteConfigurationPhase(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' 'remote setup failed' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", ID: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main", FetchRef: "main"}, PrepareState: model.PrepareStatePreparing, Enabled: true}
	svc.fillPaths(&cfg)
	if err := os.MkdirAll(cfg.GitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.prepareAsync(ctx, got, 1); err == nil {
		t.Fatal("expected remote setup failure")
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"phase":"`+preparePhaseConfigureRemote+`"`) ||
		strings.Contains(logOutput, `"phase":"`+preparePhaseUpdateBranch+`"`) {
		t.Fatalf("remote setup phase was misclassified: %s", logOutput)
	}
}

func TestAddRepoSyncFailureDoesNotOverwriteNewerConfig(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "git-started")
	release := filepath.Join(tmp, "git-release")
	gitScript := "#!/bin/sh\n: > \"$AFS_GIT_STARTED\"\nwhile [ ! -f \"$AFS_GIT_RELEASE\" ]; do sleep 0.01; done\nprintf '%s\\n' 'fatal: Authentication failed; HTTP 401' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_GIT_STARTED", started)
	t.Setenv("AFS_GIT_RELEASE", release)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main"}, Enabled: true})
	}()
	waitFor(t, errCh, func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	if err := svc.ctl().AddRepoWithOptions(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/org/repo.git", Branch: "main"}, Enabled: true}, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil {
		t.Fatal("expected original synchronous preparation to fail")
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "refs/heads/main" || got.PrepareState != model.PrepareStatePreparing || got.PrepareError != "" {
		t.Fatalf("newer config was overwritten: branch/state/error = %q/%q/%q", got.Branch, got.PrepareState, got.PrepareError)
	}
}

func TestPrepareAsyncPreparedGitDirPublishesSnapshotAndMarksReady(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	runCmd(t, "git", "init", "--bare", bare)
	runCmd(t, "git", "clone", bare, work)
	runCmd(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", "README.md")
	runCmd(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	runCmd(t, "git", "-C", work, "push", "origin", "master")

	runCmd(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	runCmd(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "file://"+bare)

	root := filepath.Join(tmp, "artifact-fs")
	var logs bytes.Buffer
	svc, err := New(ctx, root, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:   "repo",
			ID:     "repo",
			GitDir: preparedGitDir,
		},
		RepoSpec: model.RepoSpec{
			Branch:         "master",
			PreparedGitDir: true,
			FetchRef:       "master",
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: time.Minute,
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, got, 1); err != nil {
		t.Fatalf("prepareAsync: %v", err)
	}

	got, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStateReady {
		t.Fatalf("PrepareState = %q, want ready", got.PrepareState)
	}
	if got.PrepareError != "" {
		t.Fatalf("PrepareError = %q, want empty", got.PrepareError)
	}
	snap, err := snapshot.New(ctx, got.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	_, ref, gen, err := snap.ReadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "master" {
		t.Fatalf("snapshot ref = %q, want master", ref)
	}
	if gen == 0 {
		t.Fatal("snapshot generation = 0, want published generation")
	}
	if _, ok := lookupNode(t, snap, gen, "README.md"); !ok {
		t.Fatal("README.md not found in snapshot")
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logRepoPreparationCompleted+`"`) ||
		!strings.Contains(logOutput, `"mode":"`+prepareModeAsync+`"`) ||
		!strings.Contains(logOutput, `"source":"`+prepareSourcePreparedGitDir+`"`) ||
		!strings.Contains(logOutput, `"phase":"`+preparePhaseComplete+`"`) ||
		!strings.Contains(logOutput, `"state":"`+prepareLogStateCompleted+`"`) ||
		!strings.Contains(logOutput, `"deadline_set":true`) ||
		!strings.Contains(logOutput, `"snapshot_generation":`) {
		t.Fatalf("async completion lifecycle log incomplete: %s", logOutput)
	}
}

func TestSyncPrepareRetainsPreviousGenerationForDaemonHandoff(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	preparedGitDir := createPreparedGitDir(t, tmp)
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", ID: "repo", GitDir: preparedGitDir}, RepoSpec: model.RepoSpec{Branch: "master", PreparedGitDir: true, FetchRef: "master"}, Enabled: true}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, got, 1); err != nil {
		t.Fatal(err)
	}
	got, err = svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.New(ctx, got.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	headOID, headRef, oldGen, err := snap.ReadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snap.PublishGeneration(ctx, headOID, headRef, []model.BaseNode{{RepoID: got.ID, Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}}); err != nil {
		t.Fatal(err)
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}

	if err := svc.prepareSync(ctx, got); err != nil {
		t.Fatal(err)
	}
	snap, err = snapshot.New(ctx, got.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if _, ok := lookupNode(t, snap, oldGen, "."); !ok {
		t.Fatal("sync preparation pruned the generation that a daemon may still be serving")
	}
}

func TestSizeUpdateBatcherFlushesOnStop(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	snap, err := snapshot.New(ctx, filepath.Join(tmp, "snap.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	gen, err := snap.PublishGeneration(ctx, "head", "master", []model.BaseNode{
		{RepoID: "repo", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "repo", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "a", SizeState: "unknown"},
		{RepoID: "repo", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "b", SizeState: "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	batcher := newSizeUpdateBatcher(snap, slog.New(slog.NewTextHandler(io.Discard, nil)), "repo")
	batcher.Start(runCtx)
	batcher.Add(gen, "a", 10)
	batcher.Add(gen, "b", 20)
	cancel()
	batcher.Stop()

	n, ok := lookupNode(t, snap, gen, "a.txt")
	if !ok {
		t.Fatal("a.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 10 {
		t.Fatalf("a.txt size = %s/%d, want known/10", n.SizeState, n.SizeBytes)
	}
	n, ok = lookupNode(t, snap, gen, "b.txt")
	if !ok {
		t.Fatal("b.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 20 {
		t.Fatalf("b.txt size = %s/%d, want known/20", n.SizeState, n.SizeBytes)
	}
}

func TestPrepareAsyncFreshCloneSkipsSecondFetchForBranchFetchRef(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

	runCmd(t, "git", "init", "--bare", bare)
	runCmd(t, "git", "clone", bare, work)
	runCmd(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", "README.md")
	runCmd(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	runCmd(t, "git", "-C", work, "push", "origin", "master")

	for _, fetchRef := range []string{"master", "refs/heads/master", "origin/master", "refs/remotes/origin/master"} {
		t.Run(fetchRef, func(t *testing.T) {
			svc, err := New(ctx, filepath.Join(tmp, "artifact-fs", strings.ReplaceAll(fetchRef, "/", "-")), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()

			cfg := model.RepoConfig{
				RepoPaths: model.RepoPaths{
					Name: "repo",
					ID:   "repo",
				},
				RepoSpec: model.RepoSpec{
					RemoteURL: "file://" + bare,
					Branch:    "master",
					FetchRef:  fetchRef,
				},
				Enabled: true,
			}
			if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
				t.Fatal(err)
			}
			got, err := svc.registry.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}

			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(t.TempDir(), "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(t.TempDir(), "git.log")
			fakeGit := filepath.Join(bin, "git")
			if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GIT_COMMAND_LOG\"\nexec \"$AFS_REAL_GIT\" \"$@\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AFS_REAL_GIT", realGit)
			t.Setenv("GIT_COMMAND_LOG", logPath)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			if err := svc.prepareAsync(ctx, got, 1); err != nil {
				t.Fatalf("prepareAsync: %v", err)
			}
			logData, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for line := range strings.SplitSeq(string(logData), "\n") {
				if strings.HasPrefix(line, "fetch ") {
					t.Fatalf("fresh branch clone ran redundant fetch; git log:\n%s", logData)
				}
			}

			got, err = svc.registry.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			if got.PrepareState != model.PrepareStateReady {
				t.Fatalf("PrepareState = %q, want ready", got.PrepareState)
			}
		})
	}
}

func TestPrepareAsyncDoesNotOpenGateWhenReadyPersistenceFails(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	preparedGitDir := createPreparedGitDir(t, tmp)

	root := filepath.Join(tmp, "artifact-fs")
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.mount = fakeMount

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:   "repo",
			ID:     "repo",
			GitDir: preparedGitDir,
		},
		RepoSpec: model.RepoSpec{
			Branch:         "master",
			PreparedGitDir: true,
			FetchRef:       "master",
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: time.Minute,
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.mountAsyncRepo(ctx, got); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	gate := svc.running[got.ID].gate
	svc.mu.Unlock()
	if gate == nil {
		t.Fatal("runtime gate is nil")
	}
	// Reject only the ready write, so preparation runs every step before it.
	const writeErr = "ready state write failed"
	db, err := meta.OpenDB(filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_ready BEFORE UPDATE OF prepare_state ON repos
		WHEN NEW.prepare_state = '`+string(model.PrepareStateReady)+`' BEGIN SELECT RAISE(ABORT, '`+writeErr+`'); END`); err != nil {
		t.Fatal(err)
	}

	if err := svc.prepareAsync(ctx, got, 1); err == nil || !strings.Contains(err.Error(), writeErr) {
		t.Fatalf("prepareAsync = %v, want the ready state write failure", err)
	}
	// Wait reports an open or failed gate before it checks the context, so a
	// canceled context reads the gate's state without waiting.
	probeCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := gate.Wait(probeCtx); err == nil || !strings.Contains(err.Error(), writeErr) {
		t.Fatalf("gate wait = %v, want the gate failed with the ready state write failure", err)
	}
	if stored, err := svc.registry.GetRepo(ctx, "repo"); err != nil || stored.PrepareState != model.PrepareStateFailed {
		t.Fatalf("persisted prepare state = %q, %v; want failed", stored.PrepareState, err)
	}
}

func TestPrepareAsyncDoesNotMarkSupersededConfigReady(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	firstGitDir := createPreparedGitDir(t, filepath.Join(tmp, "first"))
	secondGitDir := createPreparedGitDir(t, filepath.Join(tmp, "second"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:   "repo",
			ID:     "repo",
			GitDir: firstGitDir,
		},
		RepoSpec: model.RepoSpec{
			Branch:         "master",
			PreparedGitDir: true,
			FetchRef:       "master",
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: time.Minute,
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	cfg.GitDir = secondGitDir
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}

	err = svc.prepareAsync(ctx, first, 1)
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("prepareAsync error = %v, want superseded", err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("PrepareState = %q, want preparing", got.PrepareState)
	}
	if got.GitDir != secondGitDir {
		t.Fatalf("GitDir = %q, want newer git dir %q", got.GitDir, secondGitDir)
	}
}

func TestPrepareAsyncDoesNotMarkSupersededConfigFailed(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name:   "repo",
			ID:     "repo",
			GitDir: filepath.Join(tmp, "missing.git"),
		},
		RepoSpec: model.RepoSpec{
			Branch:         "master",
			PreparedGitDir: true,
			FetchRef:       "master",
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: time.Minute,
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	cfg.GitDir = createPreparedGitDir(t, filepath.Join(tmp, "second"))
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}

	if err := svc.prepareAsync(ctx, first, 1); err == nil {
		t.Fatal("expected stale prepare failure")
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("PrepareState = %q, want preparing", got.PrepareState)
	}
	if got.PrepareError != "" {
		t.Fatalf("PrepareError = %q, want empty", got.PrepareError)
	}
}

func TestPrepareRequeuesReadyVerifiedSourceForRecovery(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

	runCmd(t, "git", "init", "--bare", bare)
	runCmd(t, "git", "clone", bare, work)
	runCmd(t, "git", "-C", work, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", "README.md")
	runCmd(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "verified")
	runCmd(t, "git", "-C", work, "push", "origin", "main")
	runCmd(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "true")
	requiredCommit := strings.TrimSpace(runCmdOutput(t, "git", "-C", work, "rev-parse", "HEAD"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			Name: "verified",
			ID:   "verified",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL:      "file://" + bare,
			Branch:         "refs/heads/main",
			RequiredCommit: requiredCommit,
			HistoryDepth:   1,
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval: time.Minute,
		},
		Enabled: true,
	}
	if err := svc.ctl().AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err = svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.prepareAsync(ctx, cfg, 1); err != nil {
		t.Fatalf("initial prepareAsync: %v", err)
	}
	cfg, err = svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrepareState != model.PrepareStateReady || cfg.AcquiredCommit != requiredCommit {
		t.Fatalf("initial prepared config = %+v", cfg)
	}
	initialAcquiredAt := cfg.AcquiredAt
	if err := os.RemoveAll(cfg.GitDir); err != nil {
		t.Fatal(err)
	}

	if err := svc.ctl().Prepare(ctx, cfg.Name); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cfg, err = svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("PrepareState = %q, want preparing", cfg.PrepareState)
	}
	if err := svc.prepareAsync(ctx, cfg, 1); err != nil {
		t.Fatalf("recovery prepareAsync: %v", err)
	}
	recovered, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.AcquiredAt.After(initialAcquiredAt) {
		t.Fatalf("recovery acquisition time = %v, want after %v", recovered.AcquiredAt, initialAcquiredAt)
	}
	st := persistedStatus(ctx, recovered, model.RepoRuntimeRecord{}, false)
	if !st.LastFetchAt.IsZero() || st.LastFetchResult != "never" {
		t.Fatalf("recovery acquisition reported as remote refresh: %+v", st)
	}
	head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD"))
	if head != requiredCommit {
		t.Fatalf("recovered HEAD = %q, want %q", head, requiredCommit)
	}
}

func createPreparedGitDir(t *testing.T, tmp string) string {
	t.Helper()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	runCmd(t, "git", "init", "--bare", bare)
	runCmd(t, "git", "clone", bare, work)
	runCmd(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", "README.md")
	runCmd(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	runCmd(t, "git", "-C", work, "push", "origin", "master")

	runCmd(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	runCmd(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "file://"+bare)
	return preparedGitDir
}

// markCloneStartScript is a fake git that marks AFS_CLONE_STARTED when a clone
// starts, and never finishes.
const markCloneStartScript = "#!/bin/sh\ncase \" $* \" in *' clone '*) : > \"$AFS_CLONE_STARTED\";; esac\nexec sleep 10\n"

// expirePrepareOnFile makes svc's preparation deadline pass once the fake git
// creates path, so the timeout interrupts the step the test names.
func expirePrepareOnFile(svc *Service, path string) {
	svc.prepareContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return gittest.ExpireOnFile(ctx, path)
	}
}

func waitForPrepareState(t *testing.T, svc *Service, name string, state model.PrepareState) model.RepoConfig {
	t.Helper()
	var got model.RepoConfig
	waitFor(t, nil, func() bool {
		var err error
		got, err = svc.registry.GetRepo(context.Background(), name)
		return err == nil && got.PrepareState == state
	})
	return got
}

// waitFor polls ready until it holds, and fails with the operation's error if
// done delivers first. Only go test -timeout bounds the wait: a fixed budget
// fails a loaded machine, not a broken code path.
func waitFor(t *testing.T, done <-chan error, ready func() bool) {
	t.Helper()
	for !ready() {
		select {
		case err := <-done:
			t.Fatalf("operation returned before the awaited step: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func findJSONLogRecord(t *testing.T, data []byte, message string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode log record: %v\n%s", err, data)
		}
		if record["msg"] == message {
			return record
		}
	}
	t.Fatalf("log message %q not found:\n%s", message, data)
	return nil
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	_ = runCmdOutput(t, name, args...)
}

func runCmdOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
	return string(out)
}

// lookupNode looks path up in snap's generation and fails t on a database
// error.
func lookupNode(t *testing.T, snap *snapshot.Store, generation int64, path string) (model.BaseNode, bool) {
	t.Helper()
	n, ok, err := snap.LookupNode(context.Background(), generation, path)
	if err != nil {
		t.Fatal(err)
	}
	return n, ok
}
