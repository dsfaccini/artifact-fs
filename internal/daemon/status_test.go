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

// bootstrapFixture is a repo whose index matches its published snapshot, so
// bootstrapCleanStatusIndex marks tracked.txt FSMonitor-valid while the
// overlay is empty.
type bootstrapFixture struct {
	svc     *Service
	cfg     model.RepoConfig
	headOID string
	gen     int64
	snap    *snapshot.Store
	ov      *overlay.Store
}

func newBootstrapFixture(t *testing.T) *bootstrapFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "worktree")
	runCmd(t, "git", "init", "--initial-branch", "main", repoPath)
	if err := os.WriteFile(filepath.Join(repoPath, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", repoPath, "add", "tracked.txt")
	runCmd(t, "git", "-C", repoPath, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	headOID := strings.TrimSpace(runCmdOutput(t, "git", "-C", repoPath, "rev-parse", "HEAD"))
	runCmd(t, "git", "-C", repoPath, "read-tree", "HEAD")

	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	cfg := model.RepoConfig{ID: "repo", Name: "repo", GitDir: filepath.Join(repoPath, ".git"), MountPath: repoPath}
	svc.fillPaths(&cfg)
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	gen, err := snap.PublishGeneration(ctx, headOID, "refs/heads/main", []model.BaseNode{
		{RepoID: cfg.ID, Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: cfg.ID, Path: "tracked.txt", Type: "file", Mode: 0o644, SizeState: "known"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	return &bootstrapFixture{svc: svc, cfg: cfg, headOID: headOID, gen: gen, snap: snap, ov: ov}
}

func (f *bootstrapFixture) bootstrap() error {
	return f.svc.bootstrapCleanStatusIndex(context.Background(), f.cfg, f.headOID, f.gen, f.snap, f.ov)
}

// removeTracked records a delete of tracked.txt, which leaves the overlay
// with no dirty entry.
func (f *bootstrapFixture) removeTracked(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := f.ov.Remove(ctx, "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	if dirty, err := f.ov.DirtyCount(ctx); err != nil || dirty != 0 {
		t.Fatalf("delete-only dirty count = %d, %v; want zero", dirty, err)
	}
}

// indexEntries lists the index through a hook that reports no changes.
// ls-files -f prints an FSMonitor-valid entry, which Git does not stat, in
// lowercase.
func (f *bootstrapFixture) indexEntries(t *testing.T) string {
	t.Helper()
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'token\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(runCmdOutput(t, "git", "-C", f.cfg.MountPath, "-c", "core.fsmonitor="+hook, "ls-files", "-f"))
}

func TestBootstrapCleanStatusIndexSkipsDeleteOnlyOverlay(t *testing.T) {
	// The fixture bootstraps without an overlay entry, so only the delete
	// below can leave tracked.txt to a stat.
	clean := newBootstrapFixture(t)
	if err := clean.bootstrap(); err != nil {
		t.Fatal(err)
	}
	if got := clean.indexEntries(t); got != "h tracked.txt" {
		t.Fatalf("index without an overlay entry = %q, want FSMonitor-valid h tracked.txt", got)
	}

	f := newBootstrapFixture(t)
	f.removeTracked(t)
	if err := f.bootstrap(); err != nil {
		t.Fatal(err)
	}
	if got := f.indexEntries(t); got != "H tracked.txt" {
		t.Fatalf("index with a delete-only overlay = %q, want H tracked.txt", got)
	}
}

func TestBootstrapCleanStatusIndexSkipsOverlayEntryWrittenDuringBootstrap(t *testing.T) {
	f := newBootstrapFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	started, release := filepath.Join(bin, "started"), filepath.Join(bin, "release")
	// Hold the bootstrap at the update-index that marks entries valid, after
	// every check that runs before it. The hold ends after 60 seconds, so a
	// fake git left behind by a timeout panic exits on its own.
	script := `#!/bin/sh
case " $* " in
*" update-index --fsmonitor "*)
	: > "$AFS_BOOTSTRAP_STARTED"
	start=$(date +%s)
	while [ ! -e "$AFS_BOOTSTRAP_RELEASE" ] && [ $(($(date +%s) - start)) -lt 60 ]; do sleep 0.01; done ;;
esac
exec "$AFS_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("AFS_BOOTSTRAP_STARTED", started)
	t.Setenv("AFS_BOOTSTRAP_RELEASE", release)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })

	errCh := make(chan error, 1)
	go func() { errCh <- f.bootstrap() }()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("bootstrap returned before marking entries valid: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	f.removeTracked(t)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if got := f.indexEntries(t); got != "H tracked.txt" {
		t.Fatalf("index after an overlay entry appeared during bootstrap = %q, want H tracked.txt", got)
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

func TestFSMonitorHookReturnsPathsChangedSinceToken(t *testing.T) {
	ctx := t.Context()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	defer ov.Close()
	hook := func(since string) (string, []string) {
		t.Helper()
		var out bytes.Buffer
		if err := svc.FSMonitorHook(ctx, "repo", since, &out); err != nil {
			t.Fatal(err)
		}
		fields := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
		return fields[0], fields[1:]
	}
	if _, err := ov.CreateFile(ctx, "written.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.CreateFile(ctx, "moved.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ov.Remove(ctx, "deleted.txt"); err != nil {
		t.Fatal(err)
	}
	if _, paths := hook("artifact-fs:repo:v1:0"); !reflect.DeepEqual(paths, []string{"deleted.txt", "moved.txt", "written.txt"}) {
		t.Fatalf("paths since sequence 0 = %q, want every overlay path", paths)
	}
	token, paths := hook("other-hook:1")
	if !reflect.DeepEqual(paths, []string{"/"}) {
		t.Fatalf("paths for a foreign token = %q, want a rescan", paths)
	}
	if token, paths = hook(token); len(paths) != 0 {
		t.Fatalf("paths since the current token = %q, want none", paths)
	}

	if _, err := ov.WriteFile(ctx, "written.txt", 0, []byte("edit")); err != nil {
		t.Fatal(err)
	}
	// An untracked source leaves no delete marker, so only the change log
	// still names moved.txt.
	if err := ov.Rename(ctx, "moved.txt", "renamed.txt", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"moved.txt", "renamed.txt", "written.txt"}
	next, paths := hook(token)
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths since the earlier token = %q, want %q", paths, want)
	}
	// Git that has not stored the newer token asks from the earlier one again.
	if _, paths := hook(token); !reflect.DeepEqual(paths, want) {
		t.Fatalf("repeated query from the earlier token = %q, want %q", paths, want)
	}
	if _, paths := hook(next); len(paths) != 0 {
		t.Fatalf("paths since the newer token = %q, want none", paths)
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
		{name: "token past the overlay's sequence", since: "artifact-fs:repo:v1:1", live: true, want: "MM README.md"},
		{name: "unmounted worktree", since: "artifact-fs:repo:v1:0", want: "MM README.md"},
		{name: "current token and mount", since: "artifact-fs:repo:v1:0", live: true, want: "M  README.md"},
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
