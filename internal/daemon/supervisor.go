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
	// A record a stopped daemon left behind describes nothing this daemon
	// serves, but names the repos whose mounts that daemon may have left.
	previous, err := s.registry.ClearRuntime(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, rec := range previous {
		if rec.State != "" && rec.State != repoStateUnmounted {
			s.leftovers[rec.RepoID] = true
		}
	}
	s.mu.Unlock()
	// The socket only wakes the sync loop early, so the daemon runs without it.
	control, err := s.listenControl()
	if err != nil {
		s.logger.Warn("control socket unavailable; CLI commands wait for the registry poll", "error", auth.RedactLogString(err.Error()))
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
			s.mu.Lock()
			delete(s.mountRetries, repo.ID)
			s.mu.Unlock()
			running = false
			alreadyPreparing = false
		}
		if running {
			s.retryMount(rt)
			s.updateRuntimeRefresh(rt, repo.RefreshInterval, repo.RemoteRefreshDisabled)
			s.drivePrepare(ctx, repo, rt, alreadyPreparing)
			continue
		}
		if repo.PrepareState == model.PrepareStateSyncPreparing {
			continue
		}
		// A remount request retries at once.
		if !s.mountDue(repo) {
			continue
		}
		// Preparation creates the mount point, which fails on a dead mount.
		if err := s.releaseLeftoverMount(repo); err != nil {
			s.logger.Error("repo mount failed", "repo", repo.Name, "error", auth.RedactLogString(err.Error(), repo.RemoteURL))
			s.recordMountFailure(repo, repoStateUnmounted, err, s.backoffMount(repo))
			continue
		}
		if shouldMountAsync(repo) {
			s.recordPhase(repo, phaseMount)
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
		s.logger.Info("mounting repo", "repo", repo.Name)
		s.recordPhase(repo, phaseMount)
		if err := s.mountRepo(ctx, repo); err != nil {
			s.logger.Error("repo mount failed", "repo", repo.Name, "error", auth.RedactLogString(err.Error(), repo.RemoteURL))
			s.recordMountFailure(repo, repoStateUnmounted, err, s.backoffMount(repo))
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
		}
	}
	s.mu.Lock()
	for id := range s.mountRetries {
		if _, running := s.running[id]; !registered[id] && !running {
			delete(s.mountRetries, id)
		}
	}
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
	if running {
		s.recordPhase(repo, phaseUnmount)
	}
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

// drivePrepare carries out repo's latest prepare request on rt, the runtime
// that serves repo's current config, and never unmounts rt. An async repo
// prepares again behind rt's gate, which holds FUSE ops meanwhile, unless a
// worker already prepares that request or rt already finished it: repo can
// be older than rt. The CLI prepares a sync repo while rt keeps serving; once
// it is done, rt takes the request over and reconciles HEAD.
func (s *Service) drivePrepare(ctx context.Context, repo model.RepoConfig, rt *repoRuntime, alreadyPreparing bool) {
	s.mu.Lock()
	if s.running[repo.ID] != rt {
		s.mu.Unlock()
		return
	}
	request, finished := rt.cfg.PrepareRequest, rt.prepare != model.PrepareStatePreparing
	s.mu.Unlock()
	switch {
	case repo.PrepareState == model.PrepareStatePreparing:
		if request == repo.PrepareRequest && (alreadyPreparing || finished) {
			return
		}
		if !s.resetRunningPrepareState(repo) {
			return
		}
		s.supersedePrepare(repo.ID)
		s.startPrepareWorker(ctx, repo)
	case repo.PrepareState != model.PrepareStateSyncPreparing && request != repo.PrepareRequest:
		s.mu.Lock()
		rt.cfg.PrepareRequest, rt.cfg.PrepareState, rt.cfg.PrepareError = repo.PrepareRequest, repo.PrepareState, repo.PrepareError
		s.mu.Unlock()
		s.signalHEADReconcile(rt)
	}
}
