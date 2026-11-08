package daemon

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
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
		if rec.ClaimsLeftover || rec.State != "" && rec.State != repoStateUnmounted {
			s.leftovers[rec.RepoID] = true
		}
	}
	claimed := slices.Collect(maps.Keys(s.leftovers))
	s.mu.Unlock()
	// Every record carries the daemon's claim, so the claim outlasts a daemon
	// that stops before it records the repo otherwise.
	for _, id := range claimed {
		s.recordRuntime(id, func(*model.RepoRuntimeRecord) {})
	}
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
			s.stopRepo(repo, repo.MountRequest, "repo unmount failed", nil)
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
			// The next mount carries out the new config or request at once.
			reset := func() {
				s.supersedePrepare(repo.ID)
				s.mu.Lock()
				delete(s.mountRetries, repo.ID)
				s.mu.Unlock()
			}
			if !s.stopRepo(repo, runningRequest, "repo prepare remount unmount failed", reset) {
				continue
			}
			reset()
			running = false
			alreadyPreparing = false
		}
		if running {
			s.retryMount(rt)
			s.updateRuntimeRefresh(rt, repo.RefreshPolicy)
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
		failed := func(err error) {
			if err != nil {
				s.logger.Error("removed repo unmount failed", "repo", id, "error", auth.RedactLogString(err.Error()))
			}
		}
		_, started, err := s.stop(id, failed)
		if started {
			s.logger.Info("unmounting removed repo", "repo", id)
		}
		failed(err)
	}
	s.mu.Lock()
	for id := range s.mountRetries {
		if _, running := s.running[id]; !registered[id] && !running {
			delete(s.mountRetries, id)
		}
	}
	// A repo added again under a removed repo's name, and so its ID,
	// inherits no claim.
	for id := range s.leftovers {
		if _, running := s.running[id]; !registered[id] && !running {
			delete(s.leftovers, id)
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

// stopRepo reports whether repo no longer runs. While it runs, stopRepo
// starts a stop of it unless one runs already, and does not wait for it. The
// stop records a failure under repo's mount request, retried at the next
// sync, and a stopped repo under stopped, the request the stop completes: a
// remount completes only once it mounted again. then, when set, runs before
// a successful stop records it. A stop whose unmount has not returned in
// time is recorded as failed while it keeps running. A disabled repo that
// does not run releases its leftover claim, retried at the next sync while
// the unmount of the leftover fails.
func (s *Service) stopRepo(repo model.RepoConfig, stopped string, logMsg string, then func()) bool {
	s.mu.Lock()
	rt, running := s.running[repo.ID]
	rec := s.records[repo.ID]
	recorded := rec != nil && rec.State == repoStateUnmounted && rec.MountRequest == stopped
	claimed := s.leftovers[repo.ID]
	s.mu.Unlock()
	failed := func(err error) {
		s.logger.Error(logMsg, "repo", repo.Name, "error", auth.RedactLogString(err.Error(), repo.RemoteURL))
		s.recordFailure(repo, phaseUnmount, err, time.Now().Add(registrySyncInterval))
		s.recordRuntime(repo.ID, func(rec *model.RepoRuntimeRecord) { rec.MountRequest = repo.MountRequest })
	}
	if !running {
		if claimed && !repo.Enabled {
			if err := s.releaseLeftoverMount(repo); err != nil {
				failed(err)
				return true
			}
		}
		if claimed || !recorded {
			repo.MountRequest = stopped
			s.recordStopped(repo)
		}
		return true
	}
	s.recordPhase(repo, phaseUnmount)
	op, started, err := s.stop(repo.ID, func(err error) {
		if err != nil {
			failed(err)
			return
		}
		if then != nil {
			then()
		}
		repo.MountRequest = stopped
		s.recordStopped(repo)
	})
	switch {
	case err != nil:
		failed(err)
	case !started && op != nil && s.stopHung(rt, op):
		failed(fmt.Errorf("unmount %s: unmount has not returned after %s", repo.Name, unmountTimeout+unmountReturnSlack))
	}
	return false
}

// drivePrepare carries out an async repo's latest prepare request on rt, the
// runtime that serves the repo's current config, and never unmounts rt. The
// repo prepares again behind rt's gate, which holds FUSE ops meanwhile, unless
// a worker already prepares that request or rt already finished it: repo can
// be older than rt. The CLI prepares a sync repo, and rt's watcher moves the
// mount to the HEAD that preparation leaves.
func (s *Service) drivePrepare(ctx context.Context, repo model.RepoConfig, rt *repoRuntime, alreadyPreparing bool) {
	if repo.PrepareState != model.PrepareStatePreparing {
		return
	}
	s.mu.Lock()
	if s.running[repo.ID] != rt {
		s.mu.Unlock()
		return
	}
	request, finished := rt.cfg.PrepareRequest, rt.prepare != model.PrepareStatePreparing
	s.mu.Unlock()
	if request == repo.PrepareRequest && (alreadyPreparing || finished) {
		return
	}
	if !s.resetRunningPrepareState(repo) {
		return
	}
	s.supersedePrepare(repo.ID)
	s.startPrepareWorker(ctx, repo)
}
