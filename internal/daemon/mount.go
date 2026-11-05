package daemon

import (
	"context"
	"errors"
	"fmt"
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

	mfs, err := s.mountFS(ctx, cfg, engine, nil)
	if err != nil {
		s.logger.Error("fuse mount failed, runtime will retry", "repo", cfg.Name, "error", err)
		mfs = nil
	} else {
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
	s.startRepoBackground(rt)

	return nil
}

// mountFS mounts cfg through s.mount and logs which backend serves the mount.
// It persists the mount error that status reports, and clears it on success.
func (s *Service) mountFS(ctx context.Context, cfg model.RepoConfig, engine *fusefs.Engine, gate *fusefs.ReadyGate) (fusefs.MountedFS, error) {
	mfs, err := s.mount(cfg, engine, gate, s.logger)
	s.recordMountError(ctx, cfg, err)
	if err != nil {
		return nil, err
	}
	s.logger.Info("fuse mounted", "repo", cfg.Name, "backend", mfs.Backend())
	return mfs, nil
}

// recordMountError persists err, redacted, as the repo's mount error. A nil err clears it.
func (s *Service) recordMountError(ctx context.Context, cfg model.RepoConfig, err error) {
	mountErr := ""
	if err != nil {
		mountErr = auth.RedactLogString(err.Error(), cfg.RemoteURL)
	}
	if writeErr := s.registry.UpdateMountError(ctx, cfg.ID, mountErr); writeErr != nil {
		s.logger.Error("repo mount error write failed", "repo", cfg.Name, "error", writeErr)
	}
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

	mfs, err := s.mountFS(ctx, cfg, engine, gate)
	if err != nil {
		s.logger.Error("fuse mount failed, runtime will retry", "repo", cfg.Name, "error", err)
		mfs = nil
	}
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
	s.mu.Unlock()
	go func() {
		// Join's error is fatal to the connection, and the dead mount can
		// still hold the mount point. Join also returns nil after an abort,
		// which leaves the dead mount in place.
		connErr := mfs.Join(context.Background())
		if connErr == nil && mountPointHeld(rt.cfg.MountPath) {
			connErr = errConnectionEndedMounted
		}
		s.mu.Lock()
		failed := connErr != nil && !rt.stopping && !rt.detached && rt.mfs == mfs
		if failed {
			rt.failedMount = mfs
			rt.mounts.Add(1)
		}
		s.mu.Unlock()
		if failed {
			defer rt.mounts.Done()
			s.logger.Error("fuse connection failed, unmounting", "repo", rt.cfg.Name, "error", auth.RedactLogString(connErr.Error(), rt.cfg.RemoteURL))
			s.recordMountError(rt.ctx, rt.cfg, connErr)
			if err := s.unmountFailedMount(rt); err != nil {
				s.logger.Error("fuse unmount failed, runtime will retry", "repo", rt.cfg.Name, "error", err)
			}
		}
		close(done)
		s.mu.Lock()
		if failed || !rt.stopping && !rt.detached && rt.mfs == mfs {
			rt.mfs = nil
			rt.joinDone = nil
			rt.state.State = repoStateDegraded
		}
		s.mu.Unlock()
	}()
}

// unmountFailedMount unmounts the mount whose connection failed, so that no
// mount lands on top of it. A failed unmount leaves it for the next call
// unless the mount point no longer holds a mount. The caller holds rt.mounts.
func (s *Service) unmountFailedMount(rt *repoRuntime) error {
	s.mu.Lock()
	mfs := rt.failedMount
	s.mu.Unlock()
	if mfs == nil {
		return nil
	}
	if err := mfs.Unmount(); err != nil && mountPointHeld(rt.cfg.MountPath) {
		return fmt.Errorf("unmount %s after its connection failed: %w", rt.cfg.Name, err)
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
// cannot tell. The mount table lists resolved paths, so it resolves the
// parent's symlinks; a dead mount point itself can fail with ENOTCONN.
func mountPointHeld(path string) bool {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return true
	}
	mounted, err := mountTableHas(filepath.Join(parent, filepath.Base(path)))
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
		s.logger.Error("fuse unmount failed, runtime will retry", "repo", cfg.Name, "error", err)
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
		if mf.backoff == 0 {
			mf.backoff = 30 * time.Second
		} else {
			mf.backoff = min(mf.backoff*2, 5*time.Minute)
		}
		s.mu.Unlock()
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
	}
	s.joinRuntimeMount(rt)
}
