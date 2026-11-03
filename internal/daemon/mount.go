package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// mountFunc mounts a repo's filesystem. A nil gate mounts it ungated.
type mountFunc func(model.RepoConfig, *fusefs.Resolver, *fusefs.Engine, *fusefs.ReadyGate) (fusefs.MountedFS, error)

// defaultMount is what New installs as Service.mount. Package tests replace it so they cannot mount.
var defaultMount mountFunc = fusefs.MountRepoWithGate

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
		if err := ov.ReconcileChecked(prepareCtx, baseLookup); err != nil {
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

	mfs, err := s.mountFS(ctx, cfg, resolver, engine, nil)
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
func (s *Service) mountFS(ctx context.Context, cfg model.RepoConfig, resolver *fusefs.Resolver, engine *fusefs.Engine, gate *fusefs.ReadyGate) (fusefs.MountedFS, error) {
	mfs, err := s.mount(cfg, resolver, engine, gate)
	mountErr := ""
	if err != nil {
		mountErr = auth.RedactLogString(err.Error(), cfg.RemoteURL)
	}
	if writeErr := s.registry.UpdateMountError(ctx, cfg.ID, mountErr); writeErr != nil {
		s.logger.Error("repo mount error write failed", "repo", cfg.Name, "error", writeErr)
	}
	if err != nil {
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

	mfs, err := s.mountFS(ctx, cfg, resolver, engine, gate)
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
		_ = mfs.Join(context.Background())
		close(done)
		s.mu.Lock()
		if !rt.stopping && !rt.detached && rt.mfs == mfs {
			rt.mfs = nil
			rt.joinDone = nil
			rt.state.State = repoStateDegraded
		}
		s.mu.Unlock()
	}()
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
	cfg, resolver, engine, gate := rt.cfg, rt.resolver, rt.engine, rt.gate
	rt.mounts.Add(1)
	s.mu.Unlock()
	defer rt.mounts.Done()

	mfs, err := s.mountFS(rt.ctx, cfg, resolver, engine, gate)
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
