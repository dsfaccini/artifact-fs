package daemon

import (
	"context"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func (s *Service) Start(ctx context.Context) error {
	// Initial mount of all registered repos.
	if err := s.syncRepos(ctx); err != nil {
		return err
	}

	// Poll the registry for repos added or removed after startup.
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.syncRepos(ctx); err != nil {
				s.logger.Error("registry sync failed", "error", err)
			}
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
			if err := s.unmount(repo.ID); err != nil {
				s.logger.Error("repo unmount failed", "repo", repo.Name, "error", err)
			}
			continue
		}
		s.mu.Lock()
		rt, running := s.running[repo.ID]
		_, alreadyPreparing := s.preparing[repo.ID]
		runningConfigVersion := ""
		if running {
			runningConfigVersion = rt.cfg.ConfigVersion
		}
		s.mu.Unlock()
		if running && runningConfigVersion != repo.ConfigVersion {
			s.logger.InfoContext(ctx, logRepoConfigChanged, "repo", auth.RedactLogString(repo.Name))
			if err := s.unmount(repo.ID); err != nil {
				s.logger.Error("repo prepare remount unmount failed", "repo", repo.Name, "error", err)
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
				s.logger.Error("repo async mount failed", "repo", repo.Name, "error", err)
				continue
			}
			if repo.PrepareState == model.PrepareStatePreparing {
				s.startPrepareWorker(ctx, repo)
			}
			continue
		}
		if mf, ok := s.mountFailures[repo.ID]; ok && time.Since(mf.lastAttempt) < mf.backoff {
			continue
		}
		s.logger.Info("mounting repo", "repo", repo.Name)
		if err := s.mountRepo(ctx, repo); err != nil {
			s.logger.Error("repo mount failed", "repo", repo.Name, "error", err)
			mf := s.mountFailures[repo.ID]
			if mf == nil {
				mf = &mountFailure{}
				s.mountFailures[repo.ID] = mf
			}
			mf.lastAttempt = time.Now()
			if mf.backoff == 0 {
				mf.backoff = 30 * time.Second
			} else {
				mf.backoff = min(mf.backoff*2, 5*time.Minute)
			}
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
			s.logger.Error("removed repo unmount failed", "repo", id, "error", err)
			continue
		}
		delete(s.mountFailures, id)
	}
	for id := range s.mountFailures {
		if !registered[id] {
			delete(s.mountFailures, id)
		}
	}

	return nil
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
