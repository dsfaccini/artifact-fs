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
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func TestVerifiedSourceStatusAndRefreshPolicy(t *testing.T) {
	t.Parallel()
	const requiredCommit = "0123456789012345678901234567890123456789"
	cfg := model.RepoConfig{
		RepoSpec: model.RepoSpec{
			Branch:         "refs/heads/main",
			RequiredCommit: requiredCommit,
		},
		AcquiredRef:    "refs/heads/main",
		AcquiredCommit: requiredCommit,
		AcquiredAt:     time.Now(),
		RefreshPolicy: model.RefreshPolicy{
			RemoteRefreshDisabled: true,
		},
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
	if err := svc.ctl().FetchNow(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "refresh is disabled") {
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
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", GitDir: filepath.Join(repoPath, ".git"), MountPath: repoPath}}
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
	ov, err := overlay.New(ctx, cfg.RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	return &bootstrapFixture{svc: svc, cfg: cfg, headOID: headOID, gen: gen, snap: snap, ov: ov}
}

func (f *bootstrapFixture) bootstrap() error {
	return f.svc.bootstrapCleanStatusIndex(context.Background(), f.cfg, f.headOID, f.gen, f.snap, f.ov)
}

// removeTracked records a delete of tracked.txt.
func (f *bootstrapFixture) removeTracked(t *testing.T) {
	t.Helper()
	if err := f.ov.Remove(context.Background(), "tracked.txt"); err != nil {
		t.Fatal(err)
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
	t.Parallel()
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
	t.Parallel()
	const requiredCommit = "0123456789012345678901234567890123456789"
	runtime := &repoRuntime{
		cfg:  model.RepoConfig{RepoPaths: model.RepoPaths{ID: "verified", GitDir: filepath.Join(t.TempDir(), "missing")}, RepoSpec: model.RepoSpec{RequiredCommit: requiredCommit}},
		head: servedHead{oid: requiredCommit, gen: 1},
	}
	service := &Service{running: map[model.RepoID]*repoRuntime{"verified": runtime}}
	if err := service.reconcileHEAD(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.head.oid != requiredCommit || runtime.head.gen != 1 {
		t.Fatalf("fixed base changed: %+v", runtime.head)
	}
}

// countingGitStore counts the remote fetches and HEAD resolves of a runtime's
// workers. Each call ends the worker that made it, so the count is all the
// call leaves behind. Any other call panics through the nil GitStore it embeds.
type countingGitStore struct {
	model.GitStore
	fetches, headResolves atomic.Int32
}

func (g *countingGitStore) FetchNonInteractive(context.Context, model.RepoConfig) error {
	g.fetches.Add(1)
	runtime.Goexit()
	return nil
}

func (g *countingGitStore) ResolveHEAD(context.Context, model.RepoConfig) (string, string, error) {
	g.headResolves.Add(1)
	runtime.Goexit()
	return "", "", nil
}

// A runtime on a required commit with remote refresh disabled keeps serving
// that commit: its workers never fetch, and never follow a HEAD move.
func TestFixedBaseRuntimeNeitherFetchesNorFollowsHEAD(t *testing.T) {
	const requiredCommit = "0123456789012345678901234567890123456789"
	gitDir := t.TempDir()
	branch := filepath.Join(gitDir, "refs", "heads", "main")
	if err := os.MkdirAll(filepath.Dir(branch), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{filepath.Join(gitDir, "HEAD"): "ref: refs/heads/main\n", branch: requiredCommit + "\n"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		git := &countingGitStore{}
		rt := &repoRuntime{
			ctx: ctx,
			cfg: model.RepoConfig{
				RepoPaths:     model.RepoPaths{ID: "verified", Name: "verified", GitDir: gitDir},
				RepoSpec:      model.RepoSpec{RequiredCommit: requiredCommit},
				RefreshPolicy: model.RefreshPolicy{RefreshInterval: 100 * time.Millisecond, RemoteRefreshDisabled: true},
			},
			head: servedHead{oid: requiredCommit, gen: 1},
		}
		svc := &Service{core: &core{git: git, logger: slog.New(slog.DiscardHandler)}, running: map[model.RepoID]*repoRuntime{"verified": rt}}
		svc.startRepoBackground(rt)
		synctest.Wait()
		if err := os.WriteFile(branch, []byte(strings.Repeat("f", 40)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if fetches, resolves := git.fetches.Load(), git.headResolves.Load(); fetches != 0 || resolves != 0 {
			t.Fatalf("fixed-base runtime fetched %d times and resolved HEAD %d times, want neither", fetches, resolves)
		}
		cancel()
		rt.workers.Wait()
	})
}

func TestPersistedVerifiedStatusDoesNotCountAcquisitionAsRefresh(t *testing.T) {
	t.Parallel()
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
		RepoPaths: model.RepoPaths{
			ID:     "verified",
			GitDir: gitDir,
		},
		RepoSpec: model.RepoSpec{
			Branch:         "refs/heads/main",
			RequiredCommit: strings.Repeat("a", 40),
		},
		AcquiredRef:    "refs/heads/main",
		AcquiredCommit: strings.Repeat("a", 40),
		AcquiredAt:     acquiredAt,
	}
	st := persistedStatus(context.Background(), cfg, model.RepoRuntimeRecord{}, false)
	if !st.LastFetchAt.IsZero() || st.LastFetchResult != "never" {
		t.Fatalf("acquisition reported as remote refresh: %+v", st)
	}

	refreshAt := acquiredAt.Add(time.Minute)
	if err := os.Chtimes(fetchHead, refreshAt, refreshAt); err != nil {
		t.Fatal(err)
	}
	st = persistedStatus(context.Background(), cfg, model.RepoRuntimeRecord{}, false)
	if !st.LastFetchAt.Equal(refreshAt) || st.LastFetchResult != "ok" {
		t.Fatalf("later remote refresh status = %+v, want %v and ok", st, refreshAt)
	}
}

func TestReadPersistedStatusIncludesHydrationStats(t *testing.T) {
	t.Parallel()
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

	st := persistedStatus(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", BlobCacheDir: cacheDir, GitDir: gitDir}}, model.RepoRuntimeRecord{}, false)

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
	t.Parallel()
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
	mountErr := errors.New("fskit: file system extension requires approval")
	svc.mount = func(context.Context, context.Context, model.RepoPaths, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger, func(string) string) (fusefs.MountedFS, error) {
		return nil, mountErr
	}
	if err := svc.ctl().AddRepo(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := svc.ctl().Status(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != repoStateDegraded || st.MountError != mountErr.Error() {
		t.Fatalf("daemon status after failed mount: state=%q mount_error=%q", st.State, st.MountError)
	}
	// The status CLI runs in its own process and reads what the daemon recorded.
	cliSvc, err := NewController(ctx, root, logger)
	if err != nil {
		t.Fatal(err)
	}
	st, err = cliSvc.Status(ctx, "repo")
	if closeErr := cliSvc.Close(); err != nil || closeErr != nil {
		t.Fatalf("CLI status: %v, close: %v", err, closeErr)
	}
	if st.State != repoStateDegraded || st.MountError != mountErr.Error() || st.LastErrorPhase != phaseMount || st.NextRetryAt.IsZero() {
		t.Fatalf("CLI status after failed mount: %+v", st)
	}

	svc.mount = fakeMount
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = svc.ctl().Status(ctx, "repo")
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
	t.Parallel()
	if err := writeHookField(shortWriter{}, "token"); err != io.ErrShortWrite {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
}

type blockingMountedFS struct {
	started chan context.Context
	release chan struct{}
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

func (m *blockingMountedFS) Backend() string                     { return "fake" }
func (m *blockingMountedFS) InvalidateChanged() error            { return nil }
func (m *blockingMountedFS) OpenFileGenerations() []int64        { return nil }
func (m *blockingMountedFS) OpenFilePIDs([]int64) map[uint32]int { return nil }

func (m *blockingMountedFS) Unmount(context.Context) error { return nil }

func TestJoinRuntimeMountIsNotCanceledWithWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		runtimeCtx, cancel := context.WithCancel(context.Background())
		mfs := &blockingMountedFS{started: make(chan context.Context, 1), release: make(chan struct{})}
		rt := &repoRuntime{ctx: runtimeCtx, mfs: mfs, mount: mountServing, stopping: true}
		svc := &Service{mountPointEntry: func(string) (mountEntry, bool) { return mountEntry{}, false }}
		svc.joinMount(rt, mfs)
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

// newHookService registers repo at mountPath. "/" is always in the mount
// table, so it stands in for a live mount.
func newHookService(t *testing.T, mountPath string) (*Service, model.RepoConfig) {
	t.Helper()
	ctx := t.Context()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", ID: "repo", MountPath: mountPath}, RepoSpec: model.RepoSpec{Branch: "main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Minute}, Enabled: true, ConfigVersion: "v1"}
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return svc, cfg
}

// hookAnswer returns the hook's raw answer for the repo newHookService
// registers, its token and its paths.
func hookAnswer(t *testing.T, svc *Service, since string) ([]byte, string, []string) {
	t.Helper()
	return hookAnswerFor(t, svc, "repo", since)
}

// hookAnswerOf returns the hook's raw answer for cfg, as FSMonitorHook gives it
// for cfg's registration, its token and its paths.
func hookAnswerOf(t *testing.T, cfg model.RepoConfig, since string) ([]byte, string, []string) {
	t.Helper()
	var out bytes.Buffer
	if err := answerFSMonitorHook(t.Context(), cfg, since, &out); err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
	return out.Bytes(), fields[0], fields[1:]
}

// hookAnswerFor returns the hook's raw answer for the repo named name, its
// token and its paths.
func hookAnswerFor(t *testing.T, svc *Service, name, since string) ([]byte, string, []string) {
	t.Helper()
	var out bytes.Buffer
	if err := FSMonitorHook(t.Context(), svc.root, name, since, &out); err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
	return out.Bytes(), fields[0], fields[1:]
}

// FSMonitorHook reads the state root without a Controller. It must answer
// what the hook answers from a Controller's registry, which New opened and
// migrated, for every token Git can hold.
func TestFSMonitorHookAnswersWhatAControllerReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		mounted, overlay bool
	}{
		{name: "mounted", mounted: true, overlay: true},
		{name: "mounted without an overlay", mounted: true},
		{name: "unmounted", overlay: true},
		{name: "unmounted without an overlay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			mountPath := filepath.Join(t.TempDir(), "mnt")
			if tc.mounted {
				mountPath = "/"
			}
			svc, cfg := newHookService(t, mountPath)
			instance := "none"
			if tc.overlay {
				ov, err := overlay.New(ctx, cfg.RepoPaths)
				if err != nil {
					t.Fatal(err)
				}
				defer ov.Close()
				if _, err := ov.CreateFile(ctx, "written.txt", 0o644); err != nil {
					t.Fatal(err)
				}
				if err := ov.Remove(ctx, "deleted.txt"); err != nil {
					t.Fatal(err)
				}
				changes, ok, err := overlay.ReadChangesSince(ctx, cfg.RepoPaths, 0)
				if err != nil || !ok {
					t.Fatalf("read the change log: ok=%v, %v", ok, err)
				}
				instance = changes.Instance
			}
			ctl, err := NewController(ctx, svc.root, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer ctl.Close()
			ctlCfg, err := ctl.registry.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			prefix := "artifact-fs:repo:v1:"
			for _, since := range []string{"", "other-hook:1", prefix + "rescan", prefix + instance + ":0", prefix + instance + ":1", prefix + instance + ":2", prefix + instance + ":-1", prefix + "other:0", "artifact-fs:repo:v0:" + instance + ":0"} {
				got, _, _ := hookAnswer(t, svc, since)
				var want bytes.Buffer
				if err := answerFSMonitorHook(ctx, ctlCfg, since, &want); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want.Bytes()) {
					t.Errorf("answer for token %q = %q, want %q", since, got, want.Bytes())
				}
			}
			_, wantErr := ctl.registry.GetRepo(ctx, "missing")
			if err := FSMonitorHook(ctx, svc.root, "missing", "", io.Discard); wantErr == nil || err == nil || err.Error() != wantErr.Error() {
				t.Fatalf("hook for a missing repo: %v, want %v", err, wantErr)
			}
		})
	}
}

// gitStatusWithAnswer runs git status in repo through a hook that replays
// answer, so Git consumes it without re-executing this test binary. It
// returns the token Git queried with and the status.
func gitStatusWithAnswer(t *testing.T, repo string, answer []byte) (string, string) {
	t.Helper()
	return gitStatusWithAnswerIn(t, []string{"-C", repo}, answer)
}

// gitStatusWithAnswerIn is gitStatusWithAnswer for the repository that the
// git options in location name.
func gitStatusWithAnswerIn(t *testing.T, location []string, answer []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	answerPath, tokenPath, hook := filepath.Join(dir, "answer"), filepath.Join(dir, "token"), filepath.Join(dir, "hook")
	if err := os.WriteFile(answerPath, answer, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s' \"$2\" > '"+tokenPath+"'\ncat '"+answerPath+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, err := exec.Command("git", append(location, "-c", "core.fsmonitor="+hook, "status", "--porcelain")...).Output()
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(token), string(status)
}

func TestFSMonitorHookReturnsPathsChangedSinceToken(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	svc, cfg := newHookService(t, "/")
	ov, err := overlay.New(ctx, cfg.RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	defer ov.Close()
	if _, err := ov.CreateFile(ctx, "written.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.CreateFile(ctx, "moved.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ov.Remove(ctx, "deleted.txt"); err != nil {
		t.Fatal(err)
	}
	_, token, paths := hookAnswer(t, svc, "other-hook:1")
	if !reflect.DeepEqual(paths, []string{"/"}) {
		t.Fatalf("paths for a foreign token = %q, want a rescan", paths)
	}
	first := token[:strings.LastIndex(token, ":")+1] + "0"
	if _, _, paths := hookAnswer(t, svc, first); !reflect.DeepEqual(paths, []string{"deleted.txt", "moved.txt", "written.txt"}) {
		t.Fatalf("paths since sequence 0 = %q, want every overlay path", paths)
	}
	if _, token, paths = hookAnswer(t, svc, token); len(paths) != 0 {
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
	_, next, paths := hookAnswer(t, svc, token)
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths since the earlier token = %q, want %q", paths, want)
	}
	// Git that has not stored the newer token asks from the earlier one again.
	if _, _, paths := hookAnswer(t, svc, token); !reflect.DeepEqual(paths, want) {
		t.Fatalf("repeated query from the earlier token = %q, want %q", paths, want)
	}
	if _, _, paths := hookAnswer(t, svc, next); len(paths) != 0 {
		t.Fatalf("paths since the newer token = %q, want none", paths)
	}
}

func TestFSMonitorHookRescansForTokenFromRecreatedOverlay(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	svc, cfg := newHookService(t, "/")
	ov, err := overlay.New(ctx, cfg.RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"old1", "old2", "old3"} {
		if _, err := ov.CreateFile(ctx, path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, token, _ := hookAnswer(t, svc, "other-hook:1")
	if err := ov.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(cfg.OverlayDBPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	ov, err = overlay.New(ctx, cfg.RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	defer ov.Close()
	// The new database's sequence passes the token's, so only the database id
	// tells the token apart.
	for _, path := range []string{"new1", "new2", "new3", "new4", "new5"} {
		if _, err := ov.CreateFile(ctx, path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, paths := hookAnswer(t, svc, token); !reflect.DeepEqual(paths, []string{"/"}) {
		t.Fatalf("paths for a token from the deleted database = %q, want a rescan", paths)
	}
}

func TestFSMonitorHookLeavesOverlaySchemaToTheDaemon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	svc, cfg := newHookService(t, "/")
	// An overlay database from before the change log, with a row whose ctime
	// New would backfill.
	db, err := meta.OpenDB(cfg.OverlayDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE overlay_entries (path TEXT PRIMARY KEY, kind TEXT NOT NULL, backing_path TEXT, mode INTEGER NOT NULL, size_bytes INTEGER NOT NULL DEFAULT 0, mtime_unix_ns INTEGER NOT NULL, ctime_unix_ns INTEGER NOT NULL, source_oid TEXT, source_mode INTEGER NOT NULL DEFAULT 0, target_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, mode, mtime_unix_ns, ctime_unix_ns) VALUES ('created.txt', 'create', 420, 1, 0)`); err != nil {
		t.Fatal(err)
	}
	_, token, paths := hookAnswer(t, svc, "other-hook:1")
	if !reflect.DeepEqual(paths, []string{"/"}) {
		t.Fatalf("paths without a change log = %q, want a rescan", paths)
	}
	if _, _, paths := hookAnswer(t, svc, token); !reflect.DeepEqual(paths, []string{"/"}) {
		t.Fatalf("paths for the token issued without a change log = %q, want a rescan", paths)
	}
	var tables, ctime int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'overlay_changes'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT ctime_unix_ns FROM overlay_entries`).Scan(&ctime); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || ctime != 0 {
		t.Fatalf("after the hook: change log tables = %d, ctime = %d; want the schema and rows untouched", tables, ctime)
	}
}

// Git trusts its cached directory listings after a delta answer, so the hook
// must name a path inside every directory whose listing a rename changes.
func TestFSMonitorHookKeepsGitUntrackedCacheAcrossDirectoryRenames(t *testing.T) {
	t.Parallel()
	// rename mirrors an overlay rename in the repository.
	rename := func(t *testing.T, ov *overlay.Store, repo, from, to string) {
		t.Helper()
		if err := ov.Rename(t.Context(), from, to, false); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(repo, from), filepath.Join(repo, to)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// emptyDir exists next to src, which holds a.txt.
		emptyDir string
		change   func(t *testing.T, ov *overlay.Store, repo string)
		want     string
	}{
		{name: "rename onto a cached empty directory", emptyDir: "dst", change: func(t *testing.T, ov *overlay.Store, repo string) {
			if err := os.Remove(filepath.Join(repo, "dst")); err != nil {
				t.Fatal(err)
			}
			rename(t, ov, repo, "src", "dst")
		}, want: "?? dst/\n"},
		{name: "directory recreated at the source", change: func(t *testing.T, ov *overlay.Store, repo string) {
			rename(t, ov, repo, "src", "tmp")
			if err := ov.Mkdir(t.Context(), "src", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(repo, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, want: "?? tmp/\n"},
		{name: "empty directory renamed onto the source", emptyDir: "e", change: func(t *testing.T, ov *overlay.Store, repo string) {
			rename(t, ov, repo, "src", "tmp")
			rename(t, ov, repo, "e", "src")
		}, want: "?? tmp/\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			svc, cfg := newHookService(t, "/")
			ov, err := overlay.New(ctx, cfg.RepoPaths)
			if err != nil {
				t.Fatal(err)
			}
			defer ov.Close()
			repo := filepath.Join(t.TempDir(), "repo")
			runCmd(t, "git", "init", "--initial-branch=main", repo)
			if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "git", "-C", repo, "add", "tracked.txt")
			runCmd(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
			runCmd(t, "git", "-C", repo, "config", "core.untrackedCache", "true")
			// The repository mirrors the mount.
			for _, dir := range []string{tc.emptyDir, "src"} {
				if dir == "" {
					continue
				}
				if err := ov.Mkdir(ctx, dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ov.CreateFile(ctx, "src/a.txt", 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "src", "a.txt"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			answer, token, _ := hookAnswer(t, svc, "other-hook:1")
			if _, status := gitStatusWithAnswer(t, repo, answer); status != "?? src/\n" {
				t.Fatalf("status after a rescan = %q", status)
			}
			answer, next, _ := hookAnswer(t, svc, token)
			if queried, status := gitStatusWithAnswer(t, repo, answer); queried != token || status != "?? src/\n" {
				t.Fatalf("status after an empty delta = %q from token %q, want ?? src/ from %q", status, queried, token)
			}

			tc.change(t, ov, repo)
			answer, _, paths := hookAnswer(t, svc, next)
			if queried, status := gitStatusWithAnswer(t, repo, answer); queried != next || status != tc.want {
				t.Fatalf("status = %q from token %q with paths %q, want %q from %q", status, queried, paths, tc.want, next)
			}
		})
	}
}

func TestFSMonitorHookRescansUnlessTokenAndMountAreCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// since turns the token the hook issued into the one Git sends; nil
		// sends it unchanged.
		since           func(issued string) string
		issuedUnmounted bool
		bootstrap       bool
		live            bool
		want            string
	}{
		{name: "foreign token", since: func(string) string { return "other-hook:1" }, live: true, want: "MM README.md"},
		{name: "earlier registration token", since: func(issued string) string { return strings.Replace(issued, ":v1:", ":v0:", 1) }, live: true, want: "MM README.md"},
		{name: "token past the overlay's sequence", since: func(issued string) string { return strings.TrimSuffix(issued, ":0") + ":1" }, live: true, want: "MM README.md"},
		{name: "unmounted worktree", want: "MM README.md"},
		{name: "token issued while unmounted", issuedUnmounted: true, live: true, want: "MM README.md"},
		{name: "token the bootstrap issued while unmounted", issuedUnmounted: true, bootstrap: true, live: true, want: "M  README.md"},
		{name: "current token and mount", live: true, want: "M  README.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The bootstrap case sets the environment, so it runs alone,
			// before the parallel cases resume.
			if !tc.bootstrap {
				t.Parallel()
			}
			ctx := t.Context()
			dir := t.TempDir()
			unmounted := filepath.Join(dir, "unmounted")
			mountPath := func(live bool) string {
				if live {
					return "/"
				}
				return unmounted
			}
			svc, cfg := newHookService(t, mountPath(!tc.issuedUnmounted))
			ov, err := overlay.New(ctx, cfg.RepoPaths)
			if err != nil {
				t.Fatal(err)
			}
			if err := ov.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.bootstrap {
				t.Setenv(gitstore.FSMonitorBootstrapEnv, "1")
			}
			_, since, _ := hookAnswer(t, svc, "other-hook:1")
			if tc.bootstrap {
				if err := os.Unsetenv(gitstore.FSMonitorBootstrapEnv); err != nil {
					t.Fatal(err)
				}
			}
			if tc.since != nil {
				since = tc.since(since)
			}
			cfg.MountPath = mountPath(tc.live)
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
			if err := os.WriteFile(earlierHook, []byte("#!/bin/sh\nprintf '%s\\000' '"+since+"'\n"), 0o755); err != nil {
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
			answer, _, _ := hookAnswer(t, svc, since)
			queried, status := gitStatusWithAnswer(t, repo, answer)
			if queried != since {
				t.Fatalf("Git queried token %q, want %q", queried, since)
			}
			if got := strings.TrimSuffix(status, "\n"); got != tc.want {
				t.Fatalf("git status = %q, want %q", got, tc.want)
			}
		})
	}
}

// A HEAD move that writes no work tree file, as git update-ref makes, changes
// what the mount serves. Git trusts each index entry the hook does not name, so
// the hook must name every path whose served content the switch changed.
func TestFSMonitorHookNamesThePathsAGenerationSwitchChanged(t *testing.T) {
	t.Parallel()
	f := newHeadReconcileFixture(t)
	ctx := f.ctx
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(f.workDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(path string) {
		t.Helper()
		if err := os.Remove(filepath.Join(f.workDir, path)); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture's index and work tree hold its last commit.
	runCmd(t, "git", "-C", f.workDir, "reset", "-q", "--hard", f.oldOID)
	write("gone.txt", "gone\n")
	write("src/b.txt", "b\n")
	base := f.commitToMain("base")
	write("src/b.txt", "b2\n")
	remove("gone.txt")
	write("src/new.txt", "new\n")
	next := f.commitToMain("next")
	// The work tree mirrors the mount, which serves base.
	runCmd(t, "git", "-C", f.workDir, "reset", "-q", "--hard", base)
	f.moveHEAD(base, f.oldGen+1)

	cfg := f.rt.cfg
	cfg.MountPath = "/" // always in the mount table, so it stands in for a live mount
	cfg.ConfigVersion = "v1"
	if err := f.svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	answer, token, _ := hookAnswerFor(t, f.svc, cfg.Name, "other-hook:1")
	if _, status := gitStatusWithAnswer(t, f.workDir, answer); status != "" {
		t.Fatalf("status after a rescan = %q, want clean", status)
	}
	answer, since, _ := hookAnswerFor(t, f.svc, cfg.Name, token)
	if queried, status := gitStatusWithAnswer(t, f.workDir, answer); queried != token || status != "" {
		t.Fatalf("status after an empty delta = %q from token %q, want clean from %q", status, queried, token)
	}

	f.moveHEAD(next, f.oldGen+2)
	// The mount serves next, and the index still holds base.
	write("src/b.txt", "b2\n")
	remove("gone.txt")
	write("src/new.txt", "new\n")
	answer, _, paths := hookAnswerFor(t, f.svc, cfg.Name, since)
	queried, status := gitStatusWithAnswer(t, f.workDir, answer)
	truth := runCmdOutput(t, "git", "-C", f.workDir, "-c", "core.fsmonitor=false", "status", "--porcelain")
	if want := "AD gone.txt\nMM src/b.txt\nD  src/new.txt\n?? src/new.txt\n"; truth != want {
		t.Fatalf("status with fsmonitor off = %q, want %q", truth, want)
	}
	if queried != since || status != truth {
		t.Errorf("status = %q from token %q, want %q from %q, as with fsmonitor off", status, queried, truth, since)
	}
	if want := []string{"gone.txt", "src/b.txt", "src/new.txt"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("paths after the switch = %q, want %q", paths, want)
	}
}

// acrossARestart serves the repo of a daemon in tmp, made with opts, and
// takes a token the hook trusts for a work tree that stands in for the mount.
// It stops the daemon, calls whileDown, and serves the repo from a new daemon
// on the same state root once its preparations end. It returns the repo's
// config, git's location arguments for the work tree, and the token.
func acrossARestart(t *testing.T, tmp string, opts AddRepoOptions, whileDown func(cfg model.RepoConfig, work string)) (model.RepoConfig, []string, string) {
	t.Helper()
	ctx := t.Context()
	svc, _ := newDaemonWithRepo(t, tmp, opts)
	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	svc.prepareWorkers.Wait()
	root := filepath.Join(tmp, "artifact-fs")
	cfg, err := registry.ReadRepo(ctx, registryPath(root), "repo")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MountPath = "/" // always in the mount table, so it stands in for a live mount
	work := t.TempDir()
	writeFile(t, filepath.Join(work, "README.md"), "main\n")
	location := []string{"--git-dir", cfg.GitDir, "--work-tree", work}
	answer, token, _ := hookAnswerOf(t, cfg, "other-hook:1")
	if _, status := gitStatusWithAnswerIn(t, location, answer); status != "" {
		t.Fatalf("status after a rescan = %q, want clean", status)
	}
	answer, since, _ := hookAnswerOf(t, cfg, token)
	if queried, status := gitStatusWithAnswerIn(t, location, answer); queried != token || status != "" {
		t.Fatalf("status after an empty delta = %q from token %q, want clean from %q", status, queried, token)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	whileDown(cfg, work)
	restarted, err := New(ctx, root, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.mount = fakeMount
	if err := restarted.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.prepareWorkers.Wait()
	if st, err := restarted.ctl().Status(ctx, "repo"); err != nil || st.State != repoStateMounted || st.ServedHEADOID != strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD")) {
		t.Fatalf("status after the restart = %+v, %v; want HEAD mounted", st, err)
	}
	return cfg, location, since
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// moveHEADWhileDown commits a new README.md to the repo's remote, points the
// repo's main at it, and writes it to work, as a HEAD move with no daemon
// leaves the mount to serve. It returns the new commit.
func moveHEADWhileDown(t *testing.T, tmp string, cfg model.RepoConfig, work string) string {
	t.Helper()
	next := commitFile(t, filepath.Join(tmp, "remote"), "README.md", "next\n")
	runCmd(t, "git", "--git-dir", cfg.GitDir, "fetch", "--no-tags", "origin", "main")
	runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "refs/heads/main", next)
	writeFile(t, filepath.Join(work, "README.md"), "next\n")
	return next
}

// publishWhileDown publishes the tree of commit as the current generation of
// cfg's snapshot store, as a preparation with no daemon does, and prunes
// every generation below it when prune is set.
func publishWhileDown(t *testing.T, cfg model.RepoConfig, commit string, prune bool) {
	t.Helper()
	ctx := t.Context()
	git := gitstore.New(discardLogger())
	defer git.Close()
	nodes, err := git.BuildTreeIndex(ctx, cfg, commit)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	gen, err := snap.PublishGeneration(ctx, commit, "refs/heads/main", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if prune {
		if err := snap.PruneGenerations(ctx, gen); err != nil {
			t.Fatal(err)
		}
	}
}

// requireStatusWithFSMonitorOff checks that git status through the hook's
// answer for since matches the status with fsmonitor off, want, and returns
// the paths the hook answered.
func requireStatusWithFSMonitorOff(t *testing.T, cfg model.RepoConfig, location []string, since, want string) []string {
	t.Helper()
	answer, _, paths := hookAnswerOf(t, cfg, since)
	_, status := gitStatusWithAnswerIn(t, location, answer)
	truth := runCmdOutput(t, "git", append(location, "-c", "core.fsmonitor=false", "status", "--porcelain")...)
	if truth != want {
		t.Fatalf("status with fsmonitor off = %q, want %q", truth, want)
	}
	if status != truth {
		t.Errorf("status = %q, want %q, as with fsmonitor off", status, truth)
	}
	return paths
}

// A HEAD move while the daemon is down changes what the next mount serves,
// and no runtime logged it. Git's index keeps a token the hook trusts, even
// when the overlay is empty: the clean-index bootstrap skips an index that
// differs from HEAD. The next runtime logs what changed since the generation
// the overlay last served, so the hook names README.md and Git's index stays
// trusted for every other path.
func TestFSMonitorHookAfterAHEADMoveWhileTheDaemonWasDown(t *testing.T) {
	t.Parallel()
	tmp := shortTempDir(t)
	cfg, location, since := acrossARestart(t, tmp, AddRepoOptions{}, func(cfg model.RepoConfig, work string) {
		moveHEADWhileDown(t, tmp, cfg, work)
	})
	if paths := requireStatusWithFSMonitorOff(t, cfg, location, since, "MM README.md\n"); !slices.Equal(paths, []string{"README.md"}) {
		t.Errorf("hook answered %q, want README.md", paths)
	}
}

// A restart with no HEAD move keeps Git's token trusted. A rescan would make
// Git open every file whose stat data it cannot trust, and a mount numbers
// its inodes anew. The overlay holds an entry, so the clean-index bootstrap
// does not rewrite the index.
func TestFSMonitorHookKeepsItsTokenAcrossARestart(t *testing.T) {
	t.Parallel()
	tmp := shortTempDir(t)
	cfg, location, since := acrossARestart(t, tmp, AddRepoOptions{}, func(cfg model.RepoConfig, work string) {
		ov, err := overlay.New(t.Context(), cfg.RepoPaths)
		if err != nil {
			t.Fatal(err)
		}
		defer ov.Close()
		if _, err := ov.CreateFile(t.Context(), "untracked.txt", 0o644); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(work, "untracked.txt"), "")
	})
	if paths := requireStatusWithFSMonitorOff(t, cfg, location, since, "?? untracked.txt\n"); !slices.Equal(paths, []string{"untracked.txt"}) {
		t.Errorf("hook answered %q, want untracked.txt", paths)
	}
}

// A restart whose overlay last served a generation the snapshot store no
// longer holds cannot tell what changed, so Git rescans. Here a preparation
// while the daemon was down published HEAD's new tree and pruned the rest.
func TestFSMonitorHookRescansAfterARestartFromAPrunedGeneration(t *testing.T) {
	t.Parallel()
	tmp := shortTempDir(t)
	cfg, location, since := acrossARestart(t, tmp, AddRepoOptions{}, func(cfg model.RepoConfig, work string) {
		publishWhileDown(t, cfg, moveHEADWhileDown(t, tmp, cfg, work), true)
	})
	if paths := requireStatusWithFSMonitorOff(t, cfg, location, since, "MM README.md\n"); !slices.Equal(paths, []string{"/"}) {
		t.Errorf("hook answered %q, want a rescan", paths)
	}
}

// The runtime of an async repo with a preparation pending starts behind its
// gate on the snapshot store's current generation, with no reconcile. Here a
// preparation while the daemon was down published HEAD's new tree and a new
// one was requested, so the async preparation after the restart switches
// between equal trees. The runtime start logs what changed since the
// generation the overlay last served.
func TestAsyncRuntimeStartLogsWhatChangedSinceTheServedGeneration(t *testing.T) {
	t.Parallel()
	tmp := shortTempDir(t)
	cfg, location, since := acrossARestart(t, tmp, AddRepoOptions{Async: true}, func(cfg model.RepoConfig, work string) {
		publishWhileDown(t, cfg, moveHEADWhileDown(t, tmp, cfg, work), false)
		reg, err := registry.New(t.Context(), registryPath(filepath.Join(tmp, "artifact-fs")))
		if err != nil {
			t.Fatal(err)
		}
		defer reg.Close()
		stored, err := reg.GetRepo(t.Context(), cfg.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.BeginPrepare(t.Context(), stored, model.PrepareStatePreparing, "while-down"); err != nil {
			t.Fatal(err)
		}
	})
	if paths := requireStatusWithFSMonitorOff(t, cfg, location, since, "MM README.md\n"); !slices.Equal(paths, []string{"README.md"}) {
		t.Errorf("hook answered %q, want README.md", paths)
	}
}

// seqMount records the change log's latest sequence each time the daemon
// asks the mount to invalidate the kernel's caches.
type seqMount struct {
	*fakeMountedFS
	backend string
	repo    model.RepoPaths
	mu      sync.Mutex
	seqs    []int64
}

func (m *seqMount) Backend() string { return m.backend }

func (m *seqMount) InvalidateChanged() error {
	changes, _, err := overlay.ReadChangesSince(context.Background(), m.repo, 0)
	m.mu.Lock()
	m.seqs = append(m.seqs, changes.Seq)
	m.mu.Unlock()
	return err
}

// serveThrough makes mount serve f's runtime.
func (f *headReconcileFixture) serveThrough(mount fusefs.MountedFS) {
	f.svc.mu.Lock()
	f.rt.mfs, f.rt.mount = mount, mountServing
	f.svc.mu.Unlock()
}

// switchToNext publishes the tree of f.nextOID, which changes README.md, and
// switches f's runtime to it.
func (f *headReconcileFixture) switchToNext() {
	f.t.Helper()
	nodes, err := f.svc.git.BuildTreeIndex(f.ctx, f.rt.cfg, f.nextOID)
	if err != nil {
		f.t.Fatal(err)
	}
	gen, err := f.rt.snapshot.PublishGeneration(f.ctx, f.nextOID, "main", nodes)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.svc.switchGeneration(f.ctx, f.rt, gen, 0); err != nil {
		f.t.Fatal(err)
	}
}

// A git status between a switch's commit and the kernel invalidations can
// read the hook's paths and get attributes the kernel cached from the old
// generation. The daemon logs the paths again after the invalidations, so the
// next query from that status's token names them again.
func TestSwitchLogsItsPathsAgainAfterTheKernelInvalidations(t *testing.T) {
	t.Parallel()
	f := newIdleHeadReconcileFixture(t, time.Millisecond)
	mount := &seqMount{fakeMountedFS: &fakeMountedFS{unmounted: make(chan struct{})}, backend: fusefs.BackendKernel, repo: f.rt.cfg.RepoPaths}
	f.serveThrough(mount)
	f.switchToNext()
	if len(mount.seqs) != 1 {
		t.Fatalf("invalidations = %d, want 1", len(mount.seqs))
	}
	changes, _, err := overlay.ReadChangesSince(f.ctx, f.rt.cfg.RepoPaths, mount.seqs[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"README.md"}; !reflect.DeepEqual(changes.Paths, want) {
		t.Errorf("paths logged after the invalidations = %q, want %q", changes.Paths, want)
	}
}

// FSKit sends the kernel no invalidations, so the kernel can answer with
// attributes it cached from the old generation for fusefs.CacheTTL after the
// switch. The daemon logs the paths once more when they have expired.
func TestSwitchOnFSKitLogsItsPathsAgainAfterTheKernelCacheExpires(t *testing.T) {
	t.Parallel()
	f := newIdleHeadReconcileFixture(t, time.Millisecond)
	f.serveThrough(&seqMount{fakeMountedFS: &fakeMountedFS{unmounted: make(chan struct{})}, backend: fusefs.BackendFSKit, repo: f.rt.cfg.RepoPaths})
	start := time.Now()
	f.switchToNext()
	switched, _, err := overlay.ReadChangesSince(f.ctx, f.rt.cfg.RepoPaths, 0)
	if err != nil {
		t.Fatal(err)
	}
	for {
		changes, _, err := overlay.ReadChangesSince(f.ctx, f.rt.cfg.RepoPaths, switched.Seq)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes.Paths) > 0 {
			if want := []string{"README.md"}; !reflect.DeepEqual(changes.Paths, want) {
				t.Errorf("paths logged after the switch = %q, want %q", changes.Paths, want)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed < fusefs.CacheTTL {
		t.Errorf("logged again %v after the switch began, want no sooner than %v", elapsed, fusefs.CacheTTL)
	}
}

// pausingListing pauses the first listing of generation gen until resume
// closes.
type pausingListing struct {
	model.SnapshotStore
	gen    int64
	once   sync.Once
	paused chan struct{}
	resume chan struct{}
}

func (s *pausingListing) ListChildren(gen int64, dir string) ([]model.BaseNode, error) {
	if gen == s.gen {
		s.once.Do(func() {
			close(s.paused)
			<-s.resume
		})
	}
	return s.SnapshotStore.ListChildren(gen, dir)
}

// Each of two concurrent switches retires the generation it replaced, so a
// prune keeps both for the retention window. A switch that read the
// generation before it waited for the other would retire the first
// generation twice and never the second.
func TestConcurrentSwitchesRetireTheGenerationsTheyReplaced(t *testing.T) {
	t.Parallel()
	f := newIdleHeadReconcileFixture(t, time.Millisecond)
	var gens []int64
	for _, oid := range []string{f.nextOID, f.lastOID} {
		nodes, err := f.svc.git.BuildTreeIndex(f.ctx, f.rt.cfg, oid)
		if err != nil {
			t.Fatal(err)
		}
		gen, err := f.rt.snapshot.PublishGeneration(f.ctx, oid, "main", nodes)
		if err != nil {
			t.Fatal(err)
		}
		gens = append(gens, gen)
	}
	pause := &pausingListing{SnapshotStore: f.rt.resolver.Snapshot, gen: gens[0], paused: make(chan struct{}), resume: make(chan struct{})}
	f.rt.resolver.Snapshot = pause
	first := make(chan error, 1)
	go func() { first <- f.svc.switchGeneration(f.ctx, f.rt, gens[0], 0) }()
	select {
	case <-pause.paused:
	case err := <-first:
		t.Fatalf("switch to %d returned before it listed the generation: %v", gens[0], err)
	}
	second := make(chan error, 1)
	go func() { second <- f.svc.switchGeneration(f.ctx, f.rt, gens[1], 0) }()
	waitForSwitchBehindAnother(t.Name())
	close(pause.resume)
	for _, done := range []chan error{first, second} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	f.rt.retiredMu.Lock()
	var retired []int64
	for _, r := range f.rt.retired {
		retired = append(retired, r.gen)
	}
	f.rt.retiredMu.Unlock()
	if want := []int64{f.oldGen, gens[0]}; !reflect.DeepEqual(retired, want) {
		t.Errorf("retired generations = %v, want %v", retired, want)
	}
}

// waitForSwitchBehindAnother returns once a goroutine of the test named test
// waits on a lock inside Resolver.SwitchGeneration. go test -timeout bounds
// the wait.
func waitForSwitchBehindAnother(test string) {
	// A goroutine's stack names its test's function, with each "/" of a
	// subtest's name as "_".
	fn := strings.ReplaceAll(test, "/", "_")
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			header, stack, _ := strings.Cut(g, "\n")
			if strings.Contains(header, "Mutex") && strings.Contains(stack, "(*Resolver).SwitchGeneration") && strings.Contains(stack, fn) {
				return
			}
		}
		runtime.Gosched()
	}
}
