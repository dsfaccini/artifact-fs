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

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// mountFunc mounts a repo's filesystem. A nil gate mounts it ungated. The
// fuse library logs its errors to the logger.
type mountFunc func(model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error)

// defaultMount is what New installs as Service.mount. Package tests replace it so they cannot mount.
var defaultMount mountFunc = fusefs.MountRepoWithGate

// errConnectionEndedMounted is the mount error of a connection that ended
// with no error while the mount point still holds its mount.
var errConnectionEndedMounted = errors.New("fuse connection ended while the mount point still holds the mount")

type mountFailure struct {
	lastAttempt time.Time
	backoff     time.Duration
	// request is the mount request the failed attempt carried out.
	request string
}

// mountRepo opens all stores, starts the FUSE server, watcher, and refresh
// loop. Called by the daemon's Start for each registered repo.
func (s *Service) mountRepo(ctx context.Context, cfg model.RepoConfig) error {
	var snap *snapshot.Store
	var ov *overlay.Store
	var headOID, headRef string
	var gen int64
	prepareCtx, cancel := s.withPrepareTimeout(ctx)
	defer cancel()
	err := s.withRepoPrepareLock(prepareCtx, cfg.Name, func() error {
		latest, err := s.registry.GetRepo(prepareCtx, cfg.Name)
		if err != nil {
			return err
		}
		if latest.ConfigVersion != cfg.ConfigVersion {
			return registry.ErrRepoChanged
		}
		cfg = latest
		if cfg.PrepareState == model.PrepareStateSyncPreparing {
			return fusefs.ErrRepoNotReady
		}
		if cfg.PrepareState == "" && cfg.PrepareError != "" {
			return fmt.Errorf("repo prepare failed: %s", cfg.PrepareError)
		}
		snap, headOID, headRef, gen, err = s.ensurePreparedRepo(prepareCtx, cfg, true)
		if err != nil {
			return err
		}
		if cfg.AdoptGitDir {
			entries, readErr := os.ReadDir(cfg.MountPath)
			if readErr != nil || len(entries) != 0 {
				snap.Close()
				snap = nil
				if readErr != nil {
					return readErr
				}
				return errors.New("adopted mountpoint must be empty; native working files are not imported")
			}
		}
		if cfg.RequiredCommit == "" {
			if err := s.git.EnsureIndexInitialized(prepareCtx, cfg); err != nil {
				snap.Close()
				snap = nil
				return err
			}
		}
		ov, err = overlay.New(prepareCtx, cfg)
		if err != nil {
			snap.Close()
			snap = nil
			return err
		}
		baseLookup := func(path string) (model.BaseNode, bool, error) {
			return snap.LookupNode(prepareCtx, gen, path)
		}
		if err := ov.ReconcileChecked(prepareCtx, baseLookup, nil, nil); err != nil {
			ov.Close()
			ov = nil
			snap.Close()
			snap = nil
			return err
		}
		if err := s.bootstrapCleanStatusIndex(prepareCtx, cfg, headOID, gen, snap, ov); err != nil {
			ov.Close()
			ov = nil
			snap.Close()
			snap = nil
			return err
		}
		return s.startMountedRepo(ctx, cfg, snap, ov, headOID, headRef, gen)
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) startMountedRepo(ctx context.Context, cfg model.RepoConfig, snap *snapshot.Store, ov *overlay.Store, headOID string, headRef string, gen int64) error {
	h := hydrator.New(s.git)

	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	s.refreshCommitTime(ctx, cfg, headOID, resolver, "commit timestamp unavailable, mtime will use generation fallback")
	runtimeCtx, cancel := context.WithCancel(ctx)
	sizes := newSizeUpdateBatcher(snap, s.logger, cfg.Name)
	sizes.Start(runtimeCtx)

	h.SetOnHydrated(func(_ model.RepoID, objectOID string, size int64) {
		sizes.Add(resolver.Generation(), objectOID, size)
	})
	h.Start(s.hydrationWorkers(), cfg)
	engine := &fusefs.Engine{
		Resolver: resolver,
		Repo:     cfg,
		Overlay:  ov,
		Hydrator: h,
	}

	mfs, mountErr := s.mountFS(ctx, cfg, engine, nil)
	if mountErr == nil {
		s.configureStatusOptimization(ctx, cfg)
	}
	rt := &repoRuntime{
		cfg:      cfg,
		ctx:      runtimeCtx,
		cancel:   cancel,
		snapshot: snap,
		overlay:  ov,
		hydrator: h,
		sizes:    sizes,
		resolver: resolver,
		engine:   engine,
		mfs:      mfs,
		refresh:  make(chan time.Duration, 1),
		state:    newRuntimeState(cfg.ID, headOID, headRef, gen),
	}
	if mfs == nil {
		rt.state.State = repoStateDegraded
	}
	s.startRuntime(rt)
	s.recordStartedRuntime(rt, mountErr)
	s.startRepoBackground(rt)

	return nil
}

// recordStartedRuntime records a runtime that started, with the error of its
// first mount, which the next registry sync retries.
func (s *Service) recordStartedRuntime(rt *repoRuntime, mountErr error) {
	if mountErr != nil {
		s.mu.Lock()
		cfg := rt.cfg
		s.mu.Unlock()
		s.recordFailure(cfg, phaseMount, mountErr, time.Now().Add(registrySyncInterval))
		s.recordRuntimeState(rt)
		return
	}
	// A mount that serves again ends an earlier connection failure.
	s.recordRuntimeState(rt, phaseMount, phaseServe)
}

// mountFS mounts cfg through s.mount and logs the outcome. Its caller
// records a failure with the time of the retry.
func (s *Service) mountFS(ctx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate) (fusefs.MountedFS, error) {
	mfs, err := s.mount(cfg, engine, gate, s.logger)
	if err != nil {
		s.logger.Error("fuse mount failed, runtime will retry", "repo", cfg.Name, "error", auth.RedactLogString(err.Error(), cfg.RemoteURL))
		return nil, err
	}
	s.logger.Info("fuse mounted", "repo", cfg.Name, "backend", mfs.Backend())
	return mfs, nil
}

func (s *Service) mountAsyncRepo(ctx context.Context, cfg model.RepoConfig) error {
	s.fillPaths(&cfg)
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
	h := hydrator.New(s.git)
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	if headOID != "" {
		s.refreshCommitTime(ctx, cfg, headOID, resolver, "commit timestamp unavailable, mtime will use generation fallback")
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	sizes := newSizeUpdateBatcher(snap, s.logger, cfg.Name)
	sizes.Start(runtimeCtx)
	h.SetOnHydrated(func(_ model.RepoID, objectOID string, size int64) {
		sizes.Add(resolver.Generation(), objectOID, size)
	})
	h.Start(s.hydrationWorkers(), cfg)

	gate := fusefs.NewReadyGate(false)
	if cfg.PrepareState == model.PrepareStateFailed {
		gate.MarkFailed(prepareGateError(cfg.PrepareError))
	}
	engine := &fusefs.Engine{
		Resolver: resolver,
		Repo:     cfg,
		Overlay:  ov,
		Hydrator: h,
	}

	mfs, mountErr := s.mountFS(ctx, cfg, engine, gate)
	state := cfg.PrepareState
	if strings.TrimSpace(state) == "" {
		state = model.PrepareStatePreparing
	}
	rt := &repoRuntime{
		cfg:      cfg,
		ctx:      runtimeCtx,
		cancel:   cancel,
		snapshot: snap,
		overlay:  ov,
		hydrator: h,
		sizes:    sizes,
		resolver: resolver,
		engine:   engine,
		mfs:      mfs,
		gate:     gate,
		refresh:  make(chan time.Duration, 1),
		state: model.RepoRuntimeState{
			RepoID:             cfg.ID,
			CurrentHEADOID:     headOID,
			CurrentHEADRef:     headRef,
			SnapshotGeneration: gen,
			LastFetchResult:    "never",
			State:              state,
			PrepareError:       cfg.PrepareError,
		},
	}
	s.startRuntime(rt)
	s.recordStartedRuntime(rt, mountErr)
	return nil
}

func (s *Service) joinRuntimeMount(rt *repoRuntime) {
	s.mu.Lock()
	mfs := rt.mfs
	if mfs == nil || rt.joinDone != nil {
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	rt.joinDone = done
	// Preparation replaces rt.cfg under s.mu.
	mountPath := rt.cfg.MountPath
	s.mu.Unlock()
	go func() {
		// Join's error is fatal to the connection, and the dead mount can
		// still hold the mount point. Join also returns nil after an abort,
		// which leaves the dead mount in place.
		connErr := mfs.Join(context.Background())
		if connErr == nil && mountPointHeld(mountPath) {
			connErr = errConnectionEndedMounted
		}
		s.mu.Lock()
		failed := connErr != nil && !rt.stopping && !rt.detached && rt.mfs == mfs
		if failed {
			rt.failedMount = mfs
			rt.mounts.Add(1)
		}
		cfg := rt.cfg
		s.mu.Unlock()
		// The next registry sync mounts the runtime again.
		nextRetry := time.Now().Add(registrySyncInterval)
		if failed {
			defer rt.mounts.Done()
			s.logger.Error("fuse connection failed, unmounting", "repo", cfg.Name, "error", auth.RedactLogString(connErr.Error(), cfg.RemoteURL))
			s.recordFailure(cfg, phaseServe, connErr, nextRetry)
			if err := s.unmountFailedMount(rt); err != nil {
				s.logger.Error("fuse unmount failed, runtime will retry", "repo", cfg.Name, "error", auth.RedactLogString(err.Error(), cfg.RemoteURL))
			}
		}
		close(done)
		s.mu.Lock()
		exited := failed || !rt.stopping && !rt.detached && rt.mfs == mfs
		if exited {
			rt.mfs = nil
			rt.joinDone = nil
			rt.state.State = repoStateDegraded
		}
		s.mu.Unlock()
		if exited {
			if !failed {
				s.recordFailure(cfg, phaseServe, errFUSEServerExited, nextRetry)
			}
			s.recordRuntimeState(rt)
		}
	}()
}

// unmountFailedMount unmounts the mount whose connection failed, so that no
// mount lands on top of it. A failed unmount leaves it for the next call
// unless the mount point no longer holds a mount. The caller holds rt.mounts.
func (s *Service) unmountFailedMount(rt *repoRuntime) error {
	s.mu.Lock()
	mfs := rt.failedMount
	name, mountPath := rt.cfg.Name, rt.cfg.MountPath
	s.mu.Unlock()
	if mfs == nil {
		return nil
	}
	if err := mfs.Unmount(); err != nil && mountPointHeld(mountPath) {
		return fmt.Errorf("unmount %s after its connection failed: %w", name, err)
	}
	s.mu.Lock()
	rt.failedMount = nil
	s.mu.Unlock()
	return nil
}

// isMounted reports whether path is an active mount point, and false when it cannot tell.
func isMounted(path string) bool {
	mounted, _ := mountTableHas(path)
	return mounted
}

// mountPointHeld reports whether path still holds a mount, and true when it
// cannot tell. The mount table lists absolute resolved paths, so it looks up
// path with its parent's symlinks resolved. Only when the table does not list
// it does it touch path itself, to resolve a symlinked final component; a dead
// mount point can fail with ENOTCONN.
func mountPointHeld(path string) bool {
	path, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return true
	}
	path = filepath.Join(parent, filepath.Base(path))
	if mounted, err := mountTableHas(path); mounted || err != nil {
		return true
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return true
	}
	mounted, err := mountTableHas(target)
	return mounted || err != nil
}

func (s *Service) retryRuntimeMount(rt *repoRuntime) {
	s.mu.Lock()
	if rt == nil || rt.mfs != nil || rt.engine == nil || rt.stopping {
		s.mu.Unlock()
		return
	}
	mf := s.mountFailures[rt.cfg.ID]
	if mf != nil && time.Since(mf.lastAttempt) < mf.backoff {
		s.mu.Unlock()
		return
	}
	cfg, engine, gate := rt.cfg, rt.engine, rt.gate
	rt.mounts.Add(1)
	s.mu.Unlock()
	defer rt.mounts.Done()

	err := s.unmountFailedMount(rt)
	var mfs fusefs.MountedFS
	if err != nil {
		s.logger.Error("fuse unmount failed, runtime will retry", "repo", cfg.Name, "error", auth.RedactLogString(err.Error(), cfg.RemoteURL))
	} else {
		mfs, err = s.mountFS(rt.ctx, cfg, engine, gate)
	}
	s.mu.Lock()
	if err != nil {
		mf = s.mountFailures[cfg.ID]
		if mf == nil {
			mf = &mountFailure{}
			s.mountFailures[cfg.ID] = mf
		}
		mf.lastAttempt = time.Now()
		mf.request = cfg.MountRequest
		if mf.backoff == 0 {
			mf.backoff = 30 * time.Second
		} else {
			mf.backoff = min(mf.backoff*2, 5*time.Minute)
		}
		nextRetry := mf.lastAttempt.Add(mf.backoff)
		s.mu.Unlock()
		s.recordFailure(cfg, phaseMount, err, nextRetry)
		return
	}
	if s.running[cfg.ID] != rt || rt.mfs != nil {
		s.mu.Unlock()
		_ = mfs.Unmount()
		_ = mfs.Join(context.Background())
		return
	}
	rt.mfs = mfs
	delete(s.mountFailures, cfg.ID)
	stopping := rt.stopping || rt.ctx.Err() != nil
	if !stopping && rt.state.State == repoStateDegraded {
		rt.state.State = repoStateMounted
	}
	s.mu.Unlock()
	if !stopping {
		s.configureStatusOptimization(rt.ctx, cfg)
		s.recordRuntimeState(rt, phaseMount, phaseServe)
	}
	s.joinRuntimeMount(rt)
}
