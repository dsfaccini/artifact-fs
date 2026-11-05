package daemon

import (
	"cmp"
	"context"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// registrySyncInterval is how often the daemon polls the registry.
const registrySyncInterval = 3 * time.Second

func (s *Service) Start(ctx context.Context) error {
	// A record a stopped daemon left behind describes nothing this daemon serves.
	if err := s.registry.ClearRuntime(ctx); err != nil {
		return err
	}
	control, err := s.listenControl()
	if err != nil {
		return err
	}
	if control != nil {
		defer control.Close()
		go s.serveControl(control)
	}
	// Initial mount of all registered repos.
	if err := s.syncRepos(ctx); err != nil {
		return err
	}

	// Poll the registry for repos added or removed after startup, and sync at
	// once when a CLI command nudges the control socket.
	ticker := time.NewTicker(cmp.Or(s.syncInterval, registrySyncInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-s.wake:
		}
		if err := s.syncRepos(ctx); err != nil {
			s.logger.Error("registry sync failed", "error", auth.RedactLogString(err.Error()))
			s.recordRegistrySyncFailure(err)
		}
	}
}

// syncRepos reconciles the running set with the registry. Mounts new repos
// and unmounts repos that were removed or disabled.
func (s *Service) syncRepos(ctx context.Context) error {
	repos, err := s.registry.ListRepos(ctx)
	if err != nil {
		return err
	}

	registered := map[model.RepoID]bool{}
	for _, repo := range repos {
		registered[repo.ID] = true
		if !repo.Enabled {
			s.stopRepo(repo, repo.MountRequest, "repo unmount failed")
			continue
		}
		s.mu.Lock()
		rt, running := s.running[repo.ID]
		_, alreadyPreparing := s.preparing[repo.ID]
		runningConfigVersion, runningRequest := "", ""
		if running {
			runningConfigVersion, runningRequest = rt.cfg.ConfigVersion, rt.cfg.MountRequest
		}
		s.mu.Unlock()
		if running && (runningConfigVersion != repo.ConfigVersion || runningRequest != repo.MountRequest) {
			s.logger.InfoContext(ctx, logRepoConfigChanged, "repo", auth.RedactLogString(repo.Name))
			if !s.stopRepo(repo, runningRequest, "repo prepare remount unmount failed") {
				continue
			}
			s.supersedePrepare(repo.ID)
			delete(s.mountFailures, repo.ID)
			running = false
			alreadyPreparing = false
		}
		if running {
			s.retryRuntimeMount(rt)
			s.updateRuntimeRefresh(rt, repo.RefreshInterval, repo.RemoteRefreshDisabled)
			s.restartRunningPrepareIfCurrent(ctx, repo, rt, alreadyPreparing)
			continue
		}
		if repo.PrepareState == model.PrepareStateSyncPreparing {
			continue
		}
		if shouldMountAsync(repo) {
			if err := s.mountAsyncRepo(ctx, repo); err != nil {
				s.logger.Error("repo async mount failed", "repo", repo.Name, "error", auth.RedactLogString(err.Error(), repo.RemoteURL))
				s.recordMountFailure(repo, repoStateUnmounted, err, time.Now().Add(registrySyncInterval))
				continue
			}
			if repo.PrepareState == model.PrepareStatePreparing {
				s.startPrepareWorker(ctx, repo)
			}
			continue
		}
		// A remount request retries at once.
		if mf, ok := s.mountFailures[repo.ID]; ok && mf.request == repo.MountRequest && time.Since(mf.lastAttempt) < mf.backoff {
			continue
		}
		s.logger.Info("mounting repo", "repo", repo.Name)
		if err := s.mountRepo(ctx, repo); err != nil {
			s.logger.Error("repo mount failed", "repo", repo.Name, "error", auth.RedactLogString(err.Error(), repo.RemoteURL))
			mf := s.mountFailures[repo.ID]
			if mf == nil {
				mf = &mountFailure{}
				s.mountFailures[repo.ID] = mf
			}
			mf.lastAttempt = time.Now()
			mf.request = repo.MountRequest
			if mf.backoff == 0 {
				mf.backoff = 30 * time.Second
			} else {
				mf.backoff = min(mf.backoff*2, 5*time.Minute)
			}
			s.recordMountFailure(repo, repoStateUnmounted, err, mf.lastAttempt.Add(mf.backoff))
		} else {
			delete(s.mountFailures, repo.ID)
		}
	}

	// Unmount repos that were removed from the registry.
	s.mu.Lock()
	var stale []model.RepoID
	for id := range s.running {
		if !registered[id] {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		s.logger.Info("unmounting removed repo", "repo", id)
		if err := s.unmount(id); err != nil {
			s.logger.Error("removed repo unmount failed", "repo", id, "error", auth.RedactLogString(err.Error()))
			continue
		}
		delete(s.mountFailures, id)
	}
	for id := range s.mountFailures {
		if !registered[id] {
			delete(s.mountFailures, id)
		}
	}
	s.mu.Lock()
	var forgotten []model.RepoID
	for id := range s.records {
		if _, running := s.running[id]; !registered[id] && !running {
			forgotten = append(forgotten, id)
		}
	}
	s.mu.Unlock()
	for _, id := range forgotten {
		s.forgetRuntime(id)
	}
	s.clearRegistrySyncFailures()

	return nil
}

// stopRepo unmounts repo. A failure is recorded under repo's mount request,
// and a stopped repo under stopped, the request the stop completes: a
// remount completes only once it mounted again. It reports whether repo no
// longer runs.
func (s *Service) stopRepo(repo model.RepoConfig, stopped string, logMsg string) bool {
	s.mu.Lock()
	_, running := s.running[repo.ID]
	rec := s.records[repo.ID]
	recorded := rec != nil && rec.State == repoStateUnmounted && rec.MountRequest == stopped
	s.mu.Unlock()
	if err := s.unmount(repo.ID); err != nil {
		s.logger.Error(logMsg, "repo", repo.Name, "error", auth.RedactLogString(err.Error(), repo.RemoteURL))
		s.recordFailure(repo, phaseUnmount, err, time.Now().Add(registrySyncInterval))
		s.recordRuntime(repo.ID, func(rec *model.RepoRuntimeRecord) { rec.MountRequest = repo.MountRequest })
		return false
	}
	if running || !recorded {
		repo.MountRequest = stopped
		s.recordStopped(repo)
	}
	return true
}

func (s *Service) restartRunningPrepareIfCurrent(ctx context.Context, repo model.RepoConfig, rt *repoRuntime, alreadyPreparing bool) {
	if repo.PrepareState != model.PrepareStatePreparing || rt == nil {
		return
	}
	latest, err := s.registry.GetRepo(ctx, repo.Name)
	if err != nil {
		s.logger.Error("repo prepare state refresh failed", "repo", repo.Name, "error", err)
		return
	}
	if latest.PrepareState != model.PrepareStatePreparing {
		return
	}
	s.mu.Lock()
	if s.running[repo.ID] != rt {
		s.mu.Unlock()
		return
	}
	runtimeCfg := rt.cfg
	active := rt.active
	s.mu.Unlock()
	configMatches := samePrepareConfig(runtimeCfg, latest)
	if alreadyPreparing && configMatches {
		return
	}
	if active || !configMatches {
		if err := s.unmount(latest.ID); err != nil {
			s.logger.Error("repo prepare remount unmount failed", "repo", latest.Name, "error", err)
			return
		}
		if err := s.mountAsyncRepo(ctx, latest); err != nil {
			s.logger.Error("repo async remount failed", "repo", latest.Name, "error", err)
			return
		}
		s.supersedePrepare(latest.ID)
		s.startPrepareWorker(ctx, latest)
		return
	}
	if alreadyPreparing {
		return
	}
	if s.resetRunningPrepareState(latest) {
		s.startPrepareWorker(ctx, latest)
	}
}
