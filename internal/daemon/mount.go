package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// mountFunc mounts a repo's filesystem. ctx bounds the wait for the mount,
// and opCtx is the parent of every op's context. A nil gate mounts it
// ungated. The fuse library logs its errors to the logger.
type mountFunc func(ctx, opCtx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate, logger *slog.Logger) (fusefs.MountedFS, error)

// defaultMount is what New installs as Service.mount, and defaultUnmountPath
// what it installs as Service.unmountPath. Package tests replace both so they
// cannot mount or unmount.
var (
	defaultMount       mountFunc = fusefs.MountRepoWithGate
	defaultUnmountPath           = fusefs.UnmountPath
)

// mountDue reports whether cfg's next mount may run: no mount of it failed,
// its backoff has passed, or a new mount request asks for one.
func (s *Service) mountDue(cfg model.RepoConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mountRetries[cfg.ID]
	return r == nil || r.request != cfg.MountRequest || r.due(time.Now())
}

// backoffMount backs off cfg's next mount after a failed one and returns
// when it runs.
func (s *Service) backoffMount(cfg model.RepoConfig) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mountRetries[cfg.ID]
	if r == nil || r.request != cfg.MountRequest {
		r = &mountRetry{retrier: newRetrier(phaseMount, mountRetryMin, mountRetryMax), request: cfg.MountRequest}
		s.mountRetries[cfg.ID] = r
	}
	return r.fail(time.Now())
}

// mountSucceeded ends the backoff of id's mounts.
func (s *Service) mountSucceeded(id model.RepoID) {
	s.mu.Lock()
	delete(s.mountRetries, id)
	s.mu.Unlock()
}

// mountRepo brings cfg's prepared repo's snapshot to its git dir's HEAD
// through the prepare pipeline, then starts the runtime that serves it, which
// mounts it. An async repo's runtime gets an open gate, which a later prepare
// request closes while the repo prepares again.
func (s *Service) mountRepo(ctx context.Context, cfg model.RepoConfig) error {
	if cfg.PrepareState == model.PrepareStateSyncPreparing {
		return fusefs.ErrRepoNotReady
	}
	if cfg.PrepareState == "" && cfg.PrepareError != "" {
		return fmt.Errorf("repo prepare failed: %s", cfg.PrepareError)
	}
	return s.prepare(ctx, cfg, preparation{
		ambient:    true,
		mountPoint: true,
		complete: func(prepareCtx context.Context, res *prepared) error {
			return s.startPreparedRuntime(ctx, prepareCtx, res)
		},
	})
}

// startPreparedRuntime starts the runtime that serves res, which mounts it.
// ctx is the runtime's parent, and prepareCtx bounds the steps before it
// starts.
func (s *Service) startPreparedRuntime(ctx, prepareCtx context.Context, res *prepared) error {
	cfg, snap, gen := res.cfg, res.snap, res.gen
	if err := s.withCurrentPrepare(prepareCtx, cfg, func() error {
		return s.pruneGenerations(prepareCtx, nil, snap, gen)
	}); err != nil {
		return withPreparationPhase(preparePhaseCleanupSnapshot, err)
	}
	// attachMount checks this before each mount. Checking it here too
	// keeps the index of a native worktree untouched.
	if cfg.AdoptGitDir {
		entries, err := os.ReadDir(cfg.MountPath)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("adopted mountpoint must be empty; native working files are not imported")
		}
	}
	if cfg.RequiredCommit == "" {
		if err := s.git.EnsureIndexInitialized(prepareCtx, cfg); err != nil {
			return err
		}
	}
	ov, err := overlay.New(prepareCtx, cfg)
	if err != nil {
		return err
	}
	baseLookup := func(path string) (model.BaseNode, bool, error) {
		return snap.LookupNode(prepareCtx, gen, path)
	}
	if err := ov.ReconcileChecked(prepareCtx, baseLookup, nil, nil); err != nil {
		ov.Close()
		return err
	}
	if err := s.bootstrapCleanStatusIndex(prepareCtx, cfg, res.source.Commit, gen, snap, ov); err != nil {
		ov.Close()
		return err
	}
	var gate *fusefs.ReadyGate
	if isAsyncRepo(cfg) {
		gate = fusefs.NewReadyGate(true)
	}
	res.kept = true
	rt := s.startRuntime(ctx, cfg, snap, ov, servedHead{oid: res.source.Commit, ref: res.source.Ref, gen: gen}, gate)
	s.startRepoBackground(rt)
	return nil
}

// runHook runs a test hook of Service when a test set it.
func runHook(hook func()) {
	if hook != nil {
		hook()
	}
}

// mountAsyncRepo starts the runtime of cfg's async repo, which mounts it
// behind a gate that holds FUSE ops until preparation ends.
func (s *Service) mountAsyncRepo(ctx context.Context, cfg model.RepoConfig) error {
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return err
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return err
	}
	headOID, headRef, gen, _ := snap.ReadState(ctx)
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		snap.Close()
		return err
	}
	gate := fusefs.NewReadyGate(false)
	if cfg.PrepareState == model.PrepareStateFailed {
		gate.MarkFailed(prepareGateError(cfg.PrepareError))
	}
	s.startRuntime(ctx, cfg, snap, ov, servedHead{oid: headOID, ref: headRef, gen: gen}, gate)
	return nil
}

// mountEntry is what the mount table lists for a mount point: the mount's
// source and its file system type.
type mountEntry struct {
	source string
	fstype string
}

// ownsLeftover reports whether entry, the mount at repo's mount point, is one
// an artifact-fs daemon made for repo on goos. The Linux mount table lists
// such a mount with the type fuse.artifact-fs and the source
// artifact-fs:<repo>. macFUSE's FSKit lists the type macfuse and the source
// macfuse://<UUID>, so on darwin any macFUSE mount counts.
func ownsLeftover(goos string, entry mountEntry, repo string) bool {
	if goos == "linux" {
		return entry.fstype == "fuse."+fusefs.FSSubtype && entry.source == fusefs.FSName(repo)
	}
	return strings.HasPrefix(entry.fstype, "macfuse")
}

// isMounted reports whether path is an active mount point, and false when it cannot tell.
func isMounted(path string) bool {
	_, mounted, _ := mountTableEntry(path)
	return mounted
}

// mountPointHeld reports whether path still holds a mount, and true when it
// cannot tell.
func mountPointHeld(path string) bool {
	_, held := mountPointEntry(path)
	return held
}

// mountPointEntry reports whether path still holds a mount, and true when it
// cannot tell, with what the mount table lists for that mount. The mount
// table lists absolute resolved paths, so it looks up path with its parent's
// symlinks resolved. Only when the table does not list it does it touch path
// itself, to resolve a symlinked final component; a dead mount point can fail
// with ENOTCONN.
func mountPointEntry(path string) (mountEntry, bool) {
	path, err := filepath.Abs(path)
	if err != nil {
		return mountEntry{}, true
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return mountEntry{}, true
	}
	path = filepath.Join(parent, filepath.Base(path))
	if entry, mounted, err := mountTableEntry(path); mounted || err != nil {
		return entry, true
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return mountEntry{}, false
	}
	if err != nil {
		return mountEntry{}, true
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return mountEntry{}, false
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return mountEntry{}, true
	}
	entry, mounted, err := mountTableEntry(target)
	return entry, mounted || err != nil
}
