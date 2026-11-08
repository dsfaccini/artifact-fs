package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// The phases of a repo's runtime, which the record's Phase names, and of a
// recorded failure. A runtime goes through prepare (async preparation), mount,
// serve and unmount. A failure can also name registry_sync, head_reconcile
// or fetch, which run while the runtime serves.
const (
	phasePrepare       = "prepare"
	phaseRegistrySync  = "registry_sync"
	phaseMount         = "mount"
	phaseServe         = "serve"
	phaseUnmount       = "unmount"
	phaseHEADReconcile = "head_reconcile"
	phaseFetch         = "fetch"
)

// recordPhase records that the daemon entered phase for cfg.
func (s *Service) recordPhase(cfg model.RepoConfig, phase string) {
	s.recordRuntime(cfg.ID, func(rec *model.RepoRuntimeRecord) { rec.Phase = phase })
}

// errFUSEServerExited stands in for the error of a FUSE server that exited
// cleanly while the daemon still served its repo.
var errFUSEServerExited = errors.New("FUSE server exited")

// recordRuntime applies update, under s.mu, to the daemon's record of id and
// writes the record to repo_runtime, which status reads. The record also
// carries whether the daemon claims a leftover at id's mount point. The
// caller must not hold s.mu.
func (s *Service) recordRuntime(id model.RepoID, update func(*model.RepoRuntimeRecord)) {
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	s.recordRuntimeLocked(id, update)
}

// recordRuntimeLocked is recordRuntime for a caller that holds recordMu.
func (s *Service) recordRuntimeLocked(id model.RepoID, update func(*model.RepoRuntimeRecord)) {
	s.mu.Lock()
	rec := s.records[id]
	if rec == nil {
		rec = &model.RepoRuntimeRecord{RepoID: id}
		s.records[id] = rec
	}
	update(rec)
	rec.ClaimsLeftover = s.leftovers[id]
	written := *rec
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), prepareStateWriteTimeout)
	defer cancel()
	if err := s.registry.PutRuntime(ctx, written); err != nil {
		s.logger.Error("repo runtime status write failed", "repo", auth.RedactLogString(string(id)), "error", auth.RedactLogString(err.Error()))
	}
}

// recordRuntimeState writes rt's state: what it serves, its fetch result and
// its divergence from the remote. cleared names the phases that just
// succeeded, whose recorded failure it clears. It reads the state under recordMu, so
// the last write holds the latest state. It writes nothing of a runtime a
// stop unmounted, which stays running until its stop finishes, so a late
// write of its join or its preparation leaves the stop's record.
func (s *Service) recordRuntimeState(rt *repoRuntime, cleared ...string) {
	s.mu.Lock()
	id := rt.cfg.ID
	s.mu.Unlock()
	s.recordRuntime(id, func(rec *model.RepoRuntimeRecord) {
		if s.running[id] != rt || rt.mount == mountUnmounted {
			return
		}
		rec.State = rt.reportedState()
		rec.Phase = rt.phase()
		rec.ServedGeneration = rt.head.gen
		rec.ServedCommit = rt.head.oid
		rec.ServedRef = rt.head.ref
		rec.LastFetchAt = rt.fetch.at
		rec.LastFetchResult = string(rt.fetch.state)
		rec.AheadCount = rt.fetch.ahead
		rec.BehindCount = rt.fetch.behind
		rec.Diverged = rt.fetch.diverged
		rec.MountRequest = rt.cfg.MountRequest
		for _, phase := range cleared {
			clearFailure(rec, phase)
		}
	})
}

// recordStopped records that the daemon serves cfg no more.
func (s *Service) recordStopped(cfg model.RepoConfig) {
	s.recordRuntime(cfg.ID, func(rec *model.RepoRuntimeRecord) {
		*rec = model.RepoRuntimeRecord{RepoID: cfg.ID, State: repoStateUnmounted, Phase: phaseUnmount, MountRequest: cfg.MountRequest,
			LastError: rec.LastError, LastErrorPhase: rec.LastErrorPhase, LastErrorAt: rec.LastErrorAt, Attempts: rec.Attempts}
		clearFailure(rec, phaseUnmount)
		// Nothing retries a stopped repo's runtime.
		rec.NextRetryAt = time.Time{}
	})
}

// recordFailure records err, redacted, as a failure of phase that the daemon
// retries at nextRetry. A zero nextRetry means nothing retries it.
func (s *Service) recordFailure(cfg model.RepoConfig, phase string, err error, nextRetry time.Time) {
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	s.recordFailureLocked(cfg, phase, err, nextRetry)
}

// recordFailureLocked is recordFailure for a caller that holds recordMu.
func (s *Service) recordFailureLocked(cfg model.RepoConfig, phase string, err error, nextRetry time.Time) {
	msg := auth.RedactLogString(err.Error(), cfg.RemoteURL)
	now := time.Now()
	s.recordRuntimeLocked(cfg.ID, func(rec *model.RepoRuntimeRecord) {
		if rec.LastErrorPhase == phase && rec.LastError != "" {
			rec.Attempts++
		} else {
			rec.Attempts = 1
		}
		rec.LastError, rec.LastErrorPhase, rec.LastErrorAt, rec.NextRetryAt = msg, phase, now, nextRetry
	})
}

// recordMountFailure records a failure to mount cfg, which the daemon retries
// at nextRetry, and the request it was carrying out.
func (s *Service) recordMountFailure(cfg model.RepoConfig, state string, err error, nextRetry time.Time) {
	s.recordFailure(cfg, phaseMount, err, nextRetry)
	s.recordRuntime(cfg.ID, func(rec *model.RepoRuntimeRecord) {
		rec.State = state
		rec.Phase = phaseMount
		rec.MountRequest = cfg.MountRequest
	})
}

// recordRegistrySyncFailure records err on every repo the daemon serves,
// because a failed sync applies no registry change to any of them.
func (s *Service) recordRegistrySyncFailure(err error) {
	for _, cfg := range s.runningConfigs() {
		s.recordFailure(cfg, phaseRegistrySync, err, time.Now().Add(registrySyncInterval))
	}
}

// clearRegistrySyncFailures clears the failure recordRegistrySyncFailure recorded.
func (s *Service) clearRegistrySyncFailures() {
	s.mu.Lock()
	var ids []model.RepoID
	for id, rec := range s.records {
		if rec.LastErrorPhase == phaseRegistrySync {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.recordRuntime(id, func(rec *model.RepoRuntimeRecord) { clearFailure(rec, phaseRegistrySync) })
	}
}

// forgetRuntime deletes the record of a repo removed from the registry.
func (s *Service) forgetRuntime(id model.RepoID) {
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	s.mu.Lock()
	delete(s.records, id)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), prepareStateWriteTimeout)
	defer cancel()
	if err := s.registry.DeleteRuntime(ctx, id); err != nil {
		s.logger.Error("repo runtime status delete failed", "repo", auth.RedactLogString(string(id)), "error", auth.RedactLogString(err.Error()))
	}
}

func (s *Service) runningConfigs() []model.RepoConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfgs := make([]model.RepoConfig, 0, len(s.running))
	for _, rt := range s.running {
		cfgs = append(cfgs, rt.cfg)
	}
	return cfgs
}

// clearFailure clears rec's recorded failure when phase failed last.
func clearFailure(rec *model.RepoRuntimeRecord, phase string) {
	if phase == "" || rec.LastErrorPhase != phase {
		return
	}
	rec.LastError, rec.LastErrorPhase = "", ""
	rec.LastErrorAt, rec.NextRetryAt = time.Time{}, time.Time{}
	rec.Attempts = 0
}
