package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func TestVerifiedSourceStatusAndRefreshPolicy(t *testing.T) {
	const requiredCommit = "0123456789012345678901234567890123456789"
	cfg := model.RepoConfig{
		Branch:                "refs/heads/main",
		RequiredCommit:        requiredCommit,
		AcquiredRef:           "refs/heads/main",
		AcquiredCommit:        requiredCommit,
		AcquiredAt:            time.Now(),
		RemoteRefreshDisabled: true,
	}
	st := model.RepoRuntimeState{CurrentHEADOID: requiredCommit}
	applySourceStatus(&st, cfg)
	if st.SourceRef != cfg.Branch || st.RequiredCommit != requiredCommit || st.Acquisition != "verified" || !st.RemoteRefreshDisabled {
		t.Fatalf("verified source status = %+v", st)
	}

	// Acquisition is historical evidence, not a mutable comparison against HEAD.
	st.CurrentHEADOID = strings.Repeat("f", 40)
	applySourceStatus(&st, cfg)
	if st.Acquisition != "verified" {
		t.Fatalf("acquisition = %q, want verified", st.Acquisition)
	}

	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg.ID = "verified"
	cfg.Name = "verified"
	cfg.PrepareState = model.PrepareStateReady
	cfg.Enabled = true
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.FetchNow(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "refresh is disabled") {
		t.Fatalf("FetchNow error = %v, want refresh-disabled rejection", err)
	}
}

func TestBootstrapCleanStatusIndexSkipsDeleteOnlyOverlay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	runGit("init")
	runGit("config", "user.name", "test")
	runGit("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoPath, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	runGit("commit", "-m", "base")
	headOID := strings.TrimSpace(runGit("rev-parse", "HEAD"))
	runGit("read-tree", "HEAD")
	indexPath := filepath.Join(repoPath, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{ID: "repo", Name: "repo", GitDir: filepath.Join(repoPath, ".git"), MountPath: repoPath}
	svc.fillPaths(&cfg)
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := snap.PublishGeneration(ctx, headOID, "refs/heads/master", []model.BaseNode{
		{RepoID: cfg.ID, Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: cfg.ID, Path: "tracked.txt", Type: "file", Mode: 0o644, SizeState: "known"},
	})
	if err != nil {
		snap.Close()
		t.Fatal(err)
	}
	defer snap.Close()
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ov.Close()
	if err := ov.Remove(ctx, "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	if dirty, err := ov.DirtyCount(ctx); err != nil || dirty != 0 {
		t.Fatalf("delete-only dirty count = %d, %v; want zero", dirty, err)
	}
	entries, err := ov.ListAll(ctx)
	if err != nil || len(entries) != 1 || entries[0].Kind != model.OverlayKindDelete {
		t.Fatalf("delete-only overlay entries = %+v, %v", entries, err)
	}
	if err := svc.bootstrapCleanStatusIndex(ctx, cfg, headOID, gen, snap, ov); err != nil {
		t.Fatal(err)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("delete-only overlay allowed clean index bootstrap")
	}
}

func TestRequiredCommitIgnoresHEADWatcher(t *testing.T) {
	const requiredCommit = "0123456789012345678901234567890123456789"
	runtime := &repoRuntime{
		cfg:   model.RepoConfig{ID: "verified", RequiredCommit: requiredCommit, GitDir: filepath.Join(t.TempDir(), "missing")},
		state: model.RepoRuntimeState{CurrentHEADOID: requiredCommit, SnapshotGeneration: 1},
	}
	service := &Service{running: map[model.RepoID]*repoRuntime{"verified": runtime}}
	if err := service.reconcileHEAD(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.state.CurrentHEADOID != requiredCommit || runtime.state.SnapshotGeneration != 1 {
		t.Fatalf("fixed base changed: %+v", runtime.state)
	}
}

func TestPersistedVerifiedStatusDoesNotCountAcquisitionAsRefresh(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fetchHead := filepath.Join(gitDir, "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte("acquisition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acquiredAt := time.Now()
	acquisitionFetchAt := acquiredAt.Add(-time.Minute)
	if err := os.Chtimes(fetchHead, acquisitionFetchAt, acquisitionFetchAt); err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{
		ID:             "verified",
		GitDir:         gitDir,
		Branch:         "refs/heads/main",
		RequiredCommit: strings.Repeat("a", 40),
		AcquiredRef:    "refs/heads/main",
		AcquiredCommit: strings.Repeat("a", 40),
		AcquiredAt:     acquiredAt,
	}
	svc := &Service{}
	st := svc.readPersistedStatus(context.Background(), cfg)
	if !st.LastFetchAt.IsZero() || st.LastFetchResult != "never" {
		t.Fatalf("acquisition reported as remote refresh: %+v", st)
	}

	refreshAt := acquiredAt.Add(time.Minute)
	if err := os.Chtimes(fetchHead, refreshAt, refreshAt); err != nil {
		t.Fatal(err)
	}
	st = svc.readPersistedStatus(context.Background(), cfg)
	if !st.LastFetchAt.Equal(refreshAt) || st.LastFetchResult != "ok" {
		t.Fatalf("later remote refresh status = %+v, want %v and ok", st, refreshAt)
	}
}

func TestReadPersistedStatusIncludesHydrationStats(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	gitDir := filepath.Join(root, "git")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, strings.Repeat("a", 40)), []byte("abc"), 0o644); err != nil {
		t.Fatalf("write blob-a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, strings.Repeat("b", 64)), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write blob-b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, ".artifact-fs-blob-partial"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &Service{}
	st := svc.readPersistedStatus(context.Background(), model.RepoConfig{ID: "repo", BlobCacheDir: cacheDir, GitDir: gitDir})

	if st.LastFetchResult != "never" {
		t.Fatalf("LastFetchResult = %q, want never", st.LastFetchResult)
	}
	if !st.LastFetchAt.IsZero() {
		t.Fatalf("LastFetchAt = %v, want zero", st.LastFetchAt)
	}
	if st.HydratedBlobCount != 2 {
		t.Fatalf("HydratedBlobCount = %d, want 2", st.HydratedBlobCount)
	}
	if st.HydratedBlobBytes != 8 {
		t.Fatalf("HydratedBlobBytes = %d, want 8", st.HydratedBlobBytes)
	}
}

func TestStatusReportsMountErrorUntilRetryMounts(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote")
	runCmd(t, "git", "init", "--initial-branch", "main", remote)
	if err := os.WriteFile(filepath.Join(remote, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", remote, "add", "README.md")
	runCmd(t, "git", "-C", remote, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "main")

	root := filepath.Join(tmp, "artifact-fs")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := New(ctx, root, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.SetMountRoot(filepath.Join(tmp, "mnt"))
	mountErr := errors.New("fskit: file system extension requires approval")
	svc.mount = func(model.RepoConfig, *fusefs.Resolver, *fusefs.Engine, *fusefs.ReadyGate) (fusefs.MountedFS, error) {
		return nil, mountErr
	}
	if err := svc.AddRepo(ctx, model.RepoConfig{Name: "repo", RemoteURL: remote, Branch: "main", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := svc.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateDegraded || st.MountError != mountErr.Error() {
		t.Fatalf("daemon status after failed mount: state=%q mount_error=%q", st.State, st.MountError)
	}
	// The status CLI runs in its own process and reads only persisted state.
	cliSvc, err := New(ctx, root, logger)
	if err != nil {
		t.Fatal(err)
	}
	st, err = cliSvc.Status(ctx, "repo")
	if closeErr := cliSvc.Close(); err != nil || closeErr != nil {
		t.Fatalf("CLI status: %v, close: %v", err, closeErr)
	}
	if st.State != repoStateUnmounted || st.MountError != mountErr.Error() {
		t.Fatalf("CLI status after failed mount: state=%q mount_error=%q", st.State, st.MountError)
	}

	svc.mount = fakeMount
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = svc.Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateMounted || st.MountError != "" {
		t.Fatalf("status after retry mounted: state=%q mount_error=%q", st.State, st.MountError)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestWriteHookFieldRejectsShortWrite(t *testing.T) {
	if err := writeHookField(shortWriter{}, "token"); err != io.ErrShortWrite {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
}

type blockingMountedFS struct {
	started    chan context.Context
	release    chan struct{}
	unmountErr error
}

func (m *blockingMountedFS) Join(ctx context.Context) error {
	m.started <- ctx
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.release:
		return nil
	}
}

func (m *blockingMountedFS) Backend() string { return "fake" }

func (m *blockingMountedFS) Unmount() error { return m.unmountErr }

func TestJoinRuntimeMountIsNotCanceledWithWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtimeCtx, cancel := context.WithCancel(context.Background())
		mfs := &blockingMountedFS{started: make(chan context.Context, 1), release: make(chan struct{})}
		rt := &repoRuntime{ctx: runtimeCtx, mfs: mfs, stopping: true}
		svc := &Service{}
		svc.joinRuntimeMount(rt)
		joinCtx := <-mfs.started
		cancel()
		synctest.Wait()
		if err := joinCtx.Err(); err != nil {
			t.Fatalf("Join context was canceled with runtime workers: %v", err)
		}
		close(mfs.release)
		<-rt.joinDone
	})
}

func TestStopRuntimeKeepsWorkersAliveWhenUnmountFails(t *testing.T) {
	runtimeCtx, cancel := context.WithCancel(context.Background())
	rt := &repoRuntime{
		ctx:    runtimeCtx,
		cancel: cancel,
		mfs:    &blockingMountedFS{unmountErr: errors.New("busy")},
		cfg:    model.RepoConfig{Name: "repo"},
	}
	if err := (&Service{}).stopRuntime(rt); err == nil {
		t.Fatal("expected unmount failure")
	}
	if err := runtimeCtx.Err(); err != nil {
		t.Fatalf("runtime context canceled after failed unmount: %v", err)
	}
}

func TestFSMonitorDirtyPathsIncludesChangedPathsAndRenameEndpoints(t *testing.T) {
	paths := fsMonitorDirtyPaths([]model.OverlayEntry{
		{Path: "src/pkg/file.go", Kind: model.OverlayKindModify},
		{Path: "new/name.go", Kind: model.OverlayKindRename, TargetPath: "old/name.go"},
		{Path: "readme-link", Kind: model.OverlayKindSymlink, TargetPath: "../README.md"},
		{Path: ".", Kind: model.OverlayKindMkdir},
	})
	want := []string{
		"new/name.go",
		"old/name.go",
		"readme-link",
		"src/pkg/file.go",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("dirty paths = %#v, want %#v", paths, want)
	}
}

func TestFSMonitorHookOutputsDirtyOverlayPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{
		Name:            "repo",
		ID:              "repo",
		Branch:          "main",
		RefreshInterval: time.Minute,
		Enabled:         true,
		ConfigVersion:   "v1",
		// "/" is always in the mount table, so it stands in for a live mount.
		MountPath: "/",
	}
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ov.CreateFile(ctx, "new/file.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ov.Remove(ctx, "deleted.txt"); err != nil {
		t.Fatal(err)
	}
	if err := ov.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := svc.FSMonitorHook(ctx, "repo", "artifact-fs:repo:v1:1", &out); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(out.String(), "\x00")
	if !strings.HasPrefix(parts[0], "artifact-fs:repo:v1:") {
		t.Fatalf("token = %q", parts[0])
	}
	got := map[string]bool{}
	for _, p := range parts[1:] {
		if p != "" {
			got[p] = true
		}
	}
	for _, want := range []string{"deleted.txt", "new/file.txt"} {
		if !got[want] {
			t.Fatalf("fsmonitor paths missing %q: %v", want, got)
		}
	}
}

func TestFSMonitorHookRescansUnlessTokenAndMountAreCurrent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		since string
		live  bool
		want  string
	}{
		{name: "foreign token", since: "other-hook:1", live: true, want: "MM README.md"},
		{name: "earlier registration token", since: "artifact-fs:repo:v0:1", live: true, want: "MM README.md"},
		{name: "unmounted worktree", since: "artifact-fs:repo:v1:1", want: "MM README.md"},
		{name: "current token and mount", since: "artifact-fs:repo:v1:1", live: true, want: "M  README.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			svc, err := New(ctx, filepath.Join(dir, "state"), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			cfg := model.RepoConfig{Name: "repo", ID: "repo", Branch: "main", RefreshInterval: time.Minute, Enabled: true, ConfigVersion: "v1", MountPath: filepath.Join(dir, "unmounted")}
			if tc.live {
				// "/" is always in the mount table, so it stands in for a live mount.
				cfg.MountPath = "/"
			}
			svc.fillPaths(&cfg)
			if err := svc.registry.AddRepo(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(dir, "repo")
			readme := filepath.Join(repo, "README.md")
			runCmd(t, "git", "init", "--initial-branch=main", repo)
			if err := os.WriteFile(readme, []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "git", "-C", repo, "add", "README.md")
			runCmd(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
			if err := os.WriteFile(readme, []byte("staged\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "git", "-C", repo, "add", "README.md")
			// An earlier hook leaves its token and fsmonitor-valid bits in the index.
			earlierHook := filepath.Join(dir, "earlier-hook")
			if err := os.WriteFile(earlierHook, []byte("#!/bin/sh\nprintf '%s\\000' '"+tc.since+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "git", "-C", repo, "config", "core.fsmonitor", earlierHook)
			if got := runCmdOutput(t, "git", "-C", repo, "status", "--porcelain"); got != "M  README.md\n" {
				t.Fatalf("staged status = %q", got)
			}
			// The worktree shows HEAD content again, as a mount without the edit does.
			if err := os.WriteFile(readme, []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var answer bytes.Buffer
			if err := svc.FSMonitorHook(ctx, cfg.Name, tc.since, &answer); err != nil {
				t.Fatal(err)
			}
			answerPath, tokenPath := filepath.Join(dir, "answer"), filepath.Join(dir, "token")
			if err := os.WriteFile(answerPath, answer.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			// Replay the computed answer so Git consumes it without re-executing this test binary.
			replayHook := filepath.Join(dir, "replay-hook")
			if err := os.WriteFile(replayHook, []byte("#!/bin/sh\nprintf '%s' \"$2\" > '"+tokenPath+"'\ncat '"+answerPath+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "git", "-C", repo, "config", "core.fsmonitor", replayHook)
			status, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
			if err != nil {
				t.Fatal(err)
			}
			if token, err := os.ReadFile(tokenPath); err != nil || string(token) != tc.since {
				t.Fatalf("Git queried token %q, want %q: %v", token, tc.since, err)
			}
			if got := strings.TrimSuffix(string(status), "\n"); got != tc.want {
				t.Fatalf("git status = %q, want %q", got, tc.want)
			}
		})
	}
}
