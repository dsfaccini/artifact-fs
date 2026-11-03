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
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

type preparationPhaseError struct {
	phase string
	err   error
}

func (e *preparationPhaseError) Error() string { return e.err.Error() }

func (e *preparationPhaseError) Unwrap() error { return e.err }

func withPreparationPhase(phase string, err error) error {
	if err == nil {
		return nil
	}
	return &preparationPhaseError{phase: phase, err: err}
}

func preparationFailurePhase(err error) string {
	if phaseErr, ok := errors.AsType[*preparationPhaseError](err); ok {
		return phaseErr.phase
	}
	return preparePhaseUnknown
}

func (s *Service) configVersionIsCurrent(ctx context.Context, cfg model.RepoConfig) (bool, error) {
	latest, err := s.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		return false, err
	}
	return latest.ConfigVersion == cfg.ConfigVersion, nil
}

func samePrepareConfig(a model.RepoConfig, b model.RepoConfig) bool {
	return a.Branch == b.Branch &&
		a.RemoteURL == b.RemoteURL &&
		a.RequiredCommit == b.RequiredCommit &&
		a.HistoryDepth == b.HistoryDepth &&
		a.RemoteRefreshDisabled == b.RemoteRefreshDisabled &&
		a.PreparedGitDir == b.PreparedGitDir &&
		a.AdoptGitDir == b.AdoptGitDir &&
		a.FetchRef == b.FetchRef &&
		a.GitDir == b.GitDir &&
		a.MetaDBPath == b.MetaDBPath &&
		a.OverlayDir == b.OverlayDir &&
		a.OverlayDBPath == b.OverlayDBPath &&
		a.BlobCacheDir == b.BlobCacheDir &&
		a.MountPath == b.MountPath &&
		a.ConfigVersion == b.ConfigVersion
}

func normalizeSourceConfig(cfg *model.RepoConfig) error {
	if cfg.AdoptGitDir {
		if strings.TrimSpace(cfg.GitDir) == "" {
			return errors.New("--adopt-gitdir requires --git-dir")
		}
		if cfg.PreparedGitDir || cfg.RemoteURL != "" || cfg.RequiredCommit != "" || cfg.HistoryDepth != 0 || cfg.FetchRef != "" || (cfg.Branch != "" && cfg.Branch != "HEAD") {
			return errors.New("--adopt-gitdir cannot fetch, select a source ref, or prepare a remote git dir")
		}
		cfg.Branch = "HEAD"
		cfg.RemoteRefreshDisabled = true
		return nil
	}
	cfg.Branch = strings.TrimSpace(cfg.Branch)
	if cfg.Branch == "" {
		cfg.Branch = "refs/heads/main"
	} else if !strings.HasPrefix(cfg.Branch, "refs/") {
		cfg.Branch = "refs/heads/" + cfg.Branch
	}
	cfg.RequiredCommit = strings.ToLower(strings.TrimSpace(cfg.RequiredCommit))
	if cfg.HistoryDepth < 0 {
		return errors.New("--depth must not be negative")
	}
	if cfg.RequiredCommit == "" {
		return nil
	}
	if len(cfg.RequiredCommit) != 40 && len(cfg.RequiredCommit) != 64 {
		return errors.New("--require-commit must be a full 40- or 64-character commit OID")
	}
	for _, character := range cfg.RequiredCommit {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("--require-commit must be hexadecimal")
		}
	}
	return nil
}

func (s *Service) prepareConfigStillCurrent(ctx context.Context, cfg model.RepoConfig) bool {
	latest, err := s.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		s.logger.Error("repo prepare state refresh failed", "repo", cfg.Name, "error", err)
		return false
	}
	s.fillPaths(&latest)
	if strings.TrimSpace(latest.FetchRef) == "" {
		latest.FetchRef = defaultFetchRef(latest.Branch)
	}
	return samePrepareConfig(cfg, latest)
}

// prepareRepo clones the git repo and builds the initial snapshot. It does NOT
// start a FUSE mount or any background goroutines, so it's safe to call from
// short-lived CLI commands like add-repo.
func (s *Service) prepareRepo(ctx context.Context, cfg model.RepoConfig) (retErr error) {
	started := time.Now()
	safeRepo := auth.RedactLogString(cfg.Name)
	safeBranch := auth.RedactLogString(cfg.Branch)
	safeFetchRef := auth.RedactLogString(cfg.FetchRef)
	source := prepareSourceFreshClone
	existingClone := false
	if cfg.RequiredCommit != "" {
		source = prepareSourceVerified
	} else if _, err := os.Stat(cfg.GitDir); err == nil {
		source = prepareSourceExistingClone
		existingClone = true
	}
	deadlineSet := false
	timeoutMS := int64(0)
	if deadline, ok := ctx.Deadline(); ok {
		deadlineSet = true
		timeoutMS = max(time.Until(deadline).Milliseconds(), 0)
	}
	logger := s.logger.With("repo", safeRepo, "mode", prepareModeSync, "attempt", 1)
	logger.InfoContext(ctx, logRepoPreparationStarted, "source", source, "phase", preparePhaseValidate, "state", prepareLogStateStarted, "duration_ms", 0, "branch", safeBranch, "fetch_ref", safeFetchRef, "deadline_set", deadlineSet, "timeout_ms", timeoutMS)

	var headOID, headRef string
	var gen int64
	defer func() {
		durationMS := time.Since(started).Milliseconds()
		if retErr == nil {
			logger.InfoContext(ctx, logRepoPreparationCompleted, "source", source, "phase", preparePhaseComplete, "state", prepareLogStateCompleted, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "head_oid", auth.RedactLogString(headOID), "head_ref", auth.RedactLogString(headRef), "snapshot_generation", gen)
			return
		}
		phase := preparationFailurePhase(retErr)
		timedOut := errors.Is(retErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
		canceled := errors.Is(retErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		state := prepareLogStateFailed
		if canceled && !timedOut {
			state = prepareLogStateCanceled
		}
		args := []any{"source", source, "phase", phase, "state", state, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "timed_out", timedOut, "canceled", canceled, "error", auth.RedactLogString(retErr.Error(), cfg.RemoteURL)}
		if canceled && !timedOut {
			logger.InfoContext(ctx, logRepoPreparationCanceled, args...)
			return
		}
		logger.ErrorContext(ctx, logRepoPreparationFailed, args...)
	}()
	return s.withRepoPrepareLock(ctx, cfg.Name, func() error {
		current, err := s.configVersionIsCurrent(ctx, cfg)
		if err != nil {
			return withPreparationPhase(preparePhaseValidate, err)
		}
		if !current {
			return withPreparationPhase(preparePhaseValidate, fmt.Errorf("prepare superseded by newer repo config: %w", registry.ErrRepoChanged))
		}
		existingClone = false
		if _, err := os.Stat(cfg.GitDir); err == nil {
			existingClone = true
			source = prepareSourceExistingClone
		} else if !errors.Is(err, os.ErrNotExist) {
			return withPreparationPhase(preparePhaseValidate, err)
		} else if cfg.RequiredCommit == "" {
			source = prepareSourceFreshClone
		}
		if existingClone && strings.TrimSpace(cfg.RemoteURL) != "" {
			if err := s.git.ConfigureRemoteWithCredentials(ctx, cfg); err != nil {
				return withPreparationPhase(preparePhaseConfigureRemote, err)
			}
			if err := s.git.FetchRefWithCredentials(ctx, cfg, cfg.FetchRef); err != nil {
				return withPreparationPhase(preparePhaseFetch, err)
			}
			if err := s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef); err != nil {
				return withPreparationPhase(preparePhaseUpdateBranch, err)
			}
		}

		snap, resolvedOID, resolvedRef, generation, err := s.ensurePreparedRepo(ctx, cfg, false)
		if err != nil {
			return err
		}
		headOID, headRef, gen = resolvedOID, resolvedRef, generation
		defer snap.Close()
		return nil
	})
}

func (s *Service) prepareSource(ctx context.Context, cfg model.RepoConfig) (model.PreparedSource, error) {
	if cfg.AdoptGitDir {
		if err := s.git.ConfigureAdoptedWorktree(ctx, cfg); err != nil {
			return model.PreparedSource{}, err
		}
		headOID, headRef, err := s.git.ResolveHEAD(ctx, cfg)
		if err != nil {
			return model.PreparedSource{}, err
		}
		return model.PreparedSource{Ref: headRef, Commit: headOID}, nil
	}
	if cfg.RequiredCommit != "" {
		return s.git.PrepareSource(ctx, cfg, model.SourceRequirement{
			Ref:            cfg.Branch,
			RequiredCommit: cfg.RequiredCommit,
			Depth:          cfg.HistoryDepth,
		})
	}
	if err := s.git.CloneBlobless(ctx, cfg); err != nil {
		return model.PreparedSource{}, err
	}
	headOID, headRef, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return model.PreparedSource{}, err
	}
	return model.PreparedSource{Ref: headRef, Commit: headOID}, nil
}

func (s *Service) recordAcquisition(ctx context.Context, cfg model.RepoConfig, source model.PreparedSource) error {
	if !source.Verified || (!source.Acquired && cfg.AcquiredRef == source.Ref && strings.EqualFold(cfg.AcquiredCommit, source.Commit)) {
		return nil
	}
	return s.registry.RecordAcquisition(ctx, cfg, source)
}

// ensurePreparedRepo makes sure the repo is cloned and has an initial snapshot.
// The returned snapshot store remains open for callers that need to continue
// into runtime startup.
func (s *Service) ensurePreparedRepo(ctx context.Context, cfg model.RepoConfig, pruneOldGenerations bool) (*snapshot.Store, string, string, int64, error) {
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return nil, "", "", 0, withPreparationPhase(preparePhaseInitializeStorage, err)
	}
	source, err := s.prepareSource(ctx, cfg)
	if err != nil {
		return nil, "", "", 0, withPreparationPhase(preparePhaseClone, err)
	}
	headOID, headRef := source.Commit, source.Ref
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return nil, "", "", 0, withPreparationPhase(preparePhaseOpenSnapshot, err)
	}
	gen := int64(0)
	failurePhase := preparePhaseValidate
	err = s.withRepoConfigLock(ctx, cfg.Name, func() error {
		current, err := s.configVersionIsCurrent(ctx, cfg)
		if err != nil {
			return err
		}
		if !current {
			return registry.ErrRepoChanged
		}
		storedOID, storedRef, storedGen, readErr := snap.ReadState(ctx)
		gen = storedGen
		if readErr != nil || gen == 0 || storedOID != headOID || storedRef != headRef {
			failurePhase = preparePhaseBuildTree
			var publishPhase string
			gen, publishPhase, err = s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
			if publishPhase == snapshotPhasePublish {
				failurePhase = preparePhasePublishSnapshot
			}
			if err != nil {
				return err
			}
		}
		if pruneOldGenerations {
			failurePhase = preparePhaseCleanupSnapshot
			return snap.PruneGenerations(ctx, gen)
		}
		return nil
	})
	if err != nil {
		snap.Close()
		return nil, "", "", 0, withPreparationPhase(failurePhase, err)
	}
	if err := s.recordAcquisition(ctx, cfg, source); err != nil {
		snap.Close()
		return nil, "", "", 0, withPreparationPhase(preparePhasePersistReady, err)
	}
	return snap, headOID, headRef, gen, nil
}

func (s *Service) startPrepareWorker(ctx context.Context, cfg model.RepoConfig) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	if _, ok := s.preparing[cfg.ID]; ok {
		s.mu.Unlock()
		return
	}
	s.prepareSeq++
	token := s.prepareSeq
	s.prepareAttempts[cfg.ID]++
	attempt := s.prepareAttempts[cfg.ID]
	workerCtx := ctx
	rt := s.running[cfg.ID]
	trackedRuntime := rt != nil && rt.ctx != nil && !rt.stopping
	if trackedRuntime {
		workerCtx = rt.ctx
		rt.workers.Add(1)
	}
	s.preparing[cfg.ID] = token
	s.prepareWorkers.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.prepareWorkers.Done()
		if trackedRuntime {
			defer rt.workers.Done()
		}
		defer func() {
			s.mu.Lock()
			if s.preparing[cfg.ID] == token {
				delete(s.preparing, cfg.ID)
			}
			s.mu.Unlock()
		}()
		prepareCtx, cancel := context.WithTimeout(workerCtx, s.prepareTimeoutDuration())
		defer cancel()
		_ = s.runPrepareAttempt(prepareCtx, cfg, attempt)
	}()
}

func (s *Service) supersedePrepare(id model.RepoID) {
	s.mu.Lock()
	delete(s.preparing, id)
	s.mu.Unlock()
}

func (s *Service) prepareTimeoutDuration() time.Duration {
	if s.prepareTimeout > 0 {
		return s.prepareTimeout
	}
	return defaultPrepareTimeout
}

func (s *Service) resetRunningPrepareState(cfg model.RepoConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.running[cfg.ID]
	if !ok || rt.gate == nil {
		return false
	}
	rt.gate.Reset()
	rt.cfg = cfg
	rt.state.State = model.PrepareStatePreparing
	rt.state.PrepareError = ""
	return true
}

func (s *Service) runPrepare(ctx context.Context, cfg model.RepoConfig) error {
	return s.runPrepareAttempt(ctx, cfg, 1)
}

func (s *Service) runPrepareAttempt(ctx context.Context, cfg model.RepoConfig, attempt int64) (retErr error) {
	s.fillPaths(&cfg)
	if !cfg.AdoptGitDir && strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = defaultFetchRef(cfg.Branch)
	}
	started := time.Now()
	safeRepo := auth.RedactLogString(cfg.Name)
	safeBranch := auth.RedactLogString(cfg.Branch)
	safeFetchRef := auth.RedactLogString(cfg.FetchRef)
	phase := preparePhaseValidate
	source := prepareSourceFreshClone
	if cfg.RequiredCommit != "" {
		source = prepareSourceVerified
	} else if cfg.PreparedGitDir {
		source = prepareSourcePreparedGitDir
	} else if _, err := os.Stat(cfg.GitDir); err == nil {
		source = prepareSourceExistingClone
	}
	deadlineSet := false
	timeoutMS := int64(0)
	if deadline, ok := ctx.Deadline(); ok {
		deadlineSet = true
		timeoutMS = max(time.Until(deadline).Milliseconds(), 0)
	}
	logger := s.logger.With("repo", safeRepo, "mode", prepareModeAsync, "attempt", attempt)
	logger.InfoContext(ctx, logRepoPreparationStarted, "source", source, "phase", preparePhaseValidate, "state", prepareLogStateStarted, "duration_ms", 0, "branch", safeBranch, "fetch_ref", safeFetchRef, "deadline_set", deadlineSet, "timeout_ms", timeoutMS)

	var headOID, headRef string
	var gen int64
	defer func() {
		durationMS := time.Since(started).Milliseconds()
		if retErr == nil {
			logger.InfoContext(ctx, logRepoPreparationCompleted, "source", source, "phase", preparePhaseComplete, "state", prepareLogStateCompleted, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "head_oid", auth.RedactLogString(headOID), "head_ref", auth.RedactLogString(headRef), "snapshot_generation", gen)
			return
		}
		timedOut := errors.Is(retErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
		canceled := errors.Is(retErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		state := prepareLogStateFailed
		if canceled && !timedOut {
			state = prepareLogStateCanceled
		}
		args := []any{"source", source, "phase", phase, "state", state, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "timed_out", timedOut, "canceled", canceled, "error", auth.RedactLogString(retErr.Error(), cfg.RemoteURL)}
		if canceled && !timedOut {
			logger.InfoContext(ctx, logRepoPreparationCanceled, args...)
			return
		}
		logger.ErrorContext(ctx, logRepoPreparationFailed, args...)
	}()

	fail := func(err error) error {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return err
		}
		stateErr := err
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			stateErr = errors.New("prepare timed out")
		}
		stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
		defer cancel()
		if stateWriteErr := s.setPrepareState(stateCtx, cfg, model.PrepareStateFailed, stateErr); stateWriteErr != nil {
			if errors.Is(stateWriteErr, registry.ErrRepoChanged) {
				return err
			}
			logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "phase", phase, "target_state", model.PrepareStateFailed, "error", auth.RedactLogString(stateWriteErr.Error()))
			return errors.Join(err, fmt.Errorf("persist failed prepare state: %w", stateWriteErr))
		}
		return err
	}
	lockAcquired := false
	retErr = s.withRepoPrepareLock(ctx, cfg.Name, func() error {
		lockAcquired = true
		current, err := s.configVersionIsCurrent(ctx, cfg)
		if err != nil {
			return fail(err)
		}
		if !current {
			return fmt.Errorf("prepare superseded by newer repo config: %w", registry.ErrRepoChanged)
		}
		var preparedSource model.PreparedSource
		prepareExistingClone := func() error {
			phase = preparePhaseValidate
			if err := s.git.ValidateAmbientRemote(cfg); err != nil {
				return err
			}
			phase = preparePhaseConfigureRemote
			if err := s.git.ConfigureRemoteNonInteractive(ctx, cfg); err != nil {
				return err
			}
			phase = preparePhaseFetch
			if err := s.git.FetchRefNonInteractive(ctx, cfg, cfg.FetchRef); err != nil {
				return err
			}
			phase = preparePhaseUpdateBranch
			return s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef)
		}

		if cfg.RequiredCommit != "" {
			phase = preparePhaseClone
			preparedSource, err = s.git.PrepareSource(ctx, cfg, model.SourceRequirement{
				Ref:            cfg.Branch,
				RequiredCommit: cfg.RequiredCommit,
				Depth:          cfg.HistoryDepth,
			})
			if err != nil {
				return fail(err)
			}
			headOID, headRef = preparedSource.Commit, preparedSource.Ref
		} else {
			if cfg.PreparedGitDir {
				phase = preparePhaseValidate
				if err := s.git.ValidatePreparedGitDir(ctx, cfg); err != nil {
					return fail(err)
				}
				phase = preparePhaseFetch
				if err := s.git.FetchRefNonInteractive(ctx, cfg, cfg.FetchRef); err != nil {
					return fail(err)
				}
				phase = preparePhaseUpdateBranch
				if err := s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef); err != nil {
					return fail(err)
				}
			} else {
				phase = preparePhaseValidate
				if strings.TrimSpace(cfg.RemoteURL) == "" {
					return fail(errors.New("remote URL is required for async clone"))
				}
				if _, err := os.Stat(cfg.GitDir); err == nil {
					if err := prepareExistingClone(); err != nil {
						return fail(err)
					}
				} else if errors.Is(err, os.ErrNotExist) {
					phase = preparePhaseValidate
					if err := s.git.ValidateAmbientRemote(cfg); err != nil {
						return fail(err)
					}
					phase = preparePhaseClone
					if err := s.git.CloneBloblessNonInteractive(ctx, cfg); err != nil {
						return fail(err)
					}
					if !sameBranchRef(cfg.FetchRef, cfg.Branch) {
						if err := prepareExistingClone(); err != nil {
							return fail(err)
						}
					}
				} else {
					return fail(err)
				}
			}
			phase = preparePhaseResolveHEAD
			headOID, headRef, err = s.git.ResolveHEAD(ctx, cfg)
			if err != nil {
				return fail(err)
			}
			preparedSource = model.PreparedSource{Ref: headRef, Commit: headOID}
		}
		phase = preparePhaseOpenSnapshot
		snap, closeSnap, err := s.snapshotForPrepare(ctx, cfg)
		if err != nil {
			return fail(err)
		}
		if closeSnap {
			defer snap.Close()
		}
		prevGen := int64(0)
		phase = preparePhaseBuildTree
		var publishPhase string
		err = s.withRepoConfigLock(ctx, cfg.Name, func() error {
			current, err := s.configVersionIsCurrent(ctx, cfg)
			if err != nil {
				return err
			}
			if !current {
				return registry.ErrRepoChanged
			}
			_, _, prevGen, _ = snap.ReadState(ctx)
			gen, publishPhase, err = s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
			return err
		})
		if err != nil {
			if errors.Is(err, registry.ErrRepoChanged) {
				return fmt.Errorf("prepare superseded by newer repo config: %w", err)
			}
			if publishPhase == snapshotPhasePublish {
				phase = preparePhasePublishSnapshot
			}
			return fail(err)
		}
		if err := s.recordAcquisition(ctx, cfg, preparedSource); err != nil {
			return fail(err)
		}
		phase = preparePhasePersistReady
		latest, err := s.registry.GetRepo(ctx, cfg.Name)
		if err != nil {
			return fail(err)
		}
		s.fillPaths(&latest)
		if strings.TrimSpace(latest.FetchRef) == "" {
			latest.FetchRef = latest.Branch
		}
		if !samePrepareConfig(cfg, latest) {
			return errors.New("prepare superseded by newer repo config")
		}
		if err := s.setPrepareStateBeforeReadyGate(ctx, cfg); err != nil {
			return fail(err)
		}
		phase = preparePhaseActivateRuntime
		if err := s.completePreparedRuntime(ctx, cfg, headOID, headRef, gen); err != nil {
			return fail(err)
		}
		phase = preparePhaseCleanupSnapshot
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleanupErr := s.withRepoConfigLock(cleanupCtx, cfg.Name, func() error {
			current, err := s.configVersionIsCurrent(cleanupCtx, cfg)
			if err != nil {
				return err
			}
			if !current {
				return registry.ErrRepoChanged
			}
			return snap.PruneGenerations(cleanupCtx, min(prevGen, gen-1))
		})
		if cleanupErr != nil && !errors.Is(cleanupErr, registry.ErrRepoChanged) {
			s.logger.Warn("snapshot generation cleanup failed", "repo", cfg.Name, "error", cleanupErr)
		}
		cleanupCancel()
		s.configureStatusOptimization(ctx, cfg)
		return nil
	})
	if retErr != nil && !lockAcquired {
		return fail(retErr)
	}
	return retErr
}

func defaultFetchRef(sourceRef string) string {
	if branch := branchRefName(sourceRef); branch != "" {
		return branch
	}
	return strings.TrimSpace(sourceRef)
}

func sameBranchRef(fetchRef string, branch string) bool {
	return canonicalFetchRef(fetchRef) == canonicalFetchRef(branch)
}

func canonicalFetchRef(ref string) string {
	if branch := branchRefName(ref); branch != "" {
		return "refs/heads/" + branch
	}
	return strings.TrimSpace(ref)
}

func branchRefName(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{"refs/heads/", "refs/remotes/origin/", "origin/"} {
		if after, ok := strings.CutPrefix(ref, prefix); ok {
			return after
		}
	}
	if strings.HasPrefix(ref, "refs/") {
		return ""
	}
	return ref
}

func (s *Service) snapshotForPrepare(ctx context.Context, cfg model.RepoConfig) (*snapshot.Store, bool, error) {
	s.mu.Lock()
	rt := s.running[cfg.ID]
	s.mu.Unlock()
	if rt != nil && rt.snapshot != nil {
		return rt.snapshot, false, nil
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return nil, false, err
	}
	return snap, true, nil
}

func (s *Service) completePreparedRuntime(ctx context.Context, cfg model.RepoConfig, headOID string, headRef string, gen int64) error {
	s.mu.Lock()
	rt := s.running[cfg.ID]
	if rt != nil && !samePrepareConfig(rt.cfg, cfg) {
		s.mu.Unlock()
		return registry.ErrRepoChanged
	}
	s.mu.Unlock()
	if rt == nil {
		return nil
	}
	if !s.prepareConfigStillCurrent(ctx, cfg) {
		return registry.ErrRepoChanged
	}
	if err := s.bootstrapCleanStatusIndex(ctx, cfg, headOID, gen, rt.snapshot, rt.overlay); err != nil {
		return err
	}
	baseLookup := func(path string) (model.BaseNode, bool, error) {
		return rt.snapshot.LookupNode(ctx, gen, path)
	}
	commitTime := int64(0)
	if ts, err := s.git.CommitTimestamp(ctx, cfg, headOID); err == nil {
		commitTime = ts
	} else {
		s.logger.Warn("commit timestamp unavailable", "repo", cfg.Name, "error", err)
	}
	if err := rt.resolver.Transition(func() error {
		if err := rt.overlay.ReconcileChecked(ctx, baseLookup); err != nil {
			return err
		}
		rt.resolver.SetCommitTime(commitTime)
		rt.resolver.SetGeneration(gen)
		return nil
	}); err != nil {
		return err
	}
	if !s.prepareConfigStillCurrent(ctx, cfg) {
		return registry.ErrRepoChanged
	}
	s.mu.Lock()
	if s.running[cfg.ID] != rt || !samePrepareConfig(rt.cfg, cfg) {
		s.mu.Unlock()
		return registry.ErrRepoChanged
	}
	rt.cfg = cfg
	rt.cfg.PrepareState = model.PrepareStateReady
	rt.cfg.PrepareError = ""
	setHeadState(&rt.state, headOID, headRef, gen)
	rt.state.State = repoStateMounted
	rt.state.PrepareError = ""
	s.mu.Unlock()
	rt.gate.MarkReady()
	s.startRepoBackground(rt)
	return nil
}

func (s *Service) setPrepareState(ctx context.Context, cfg model.RepoConfig, state string, stateErr error) error {
	return s.applyPrepareState(ctx, cfg, state, stateErr, true)
}

func (s *Service) setPrepareStateBeforeReadyGate(ctx context.Context, cfg model.RepoConfig) error {
	return s.applyPrepareState(ctx, cfg, model.PrepareStateReady, nil, false)
}

func (s *Service) applyPrepareState(ctx context.Context, cfg model.RepoConfig, state string, stateErr error, applyReadyRuntime bool) error {
	msg := ""
	if stateErr != nil {
		msg = auth.RedactLogString(stateErr.Error(), cfg.RemoteURL)
	}
	if err := s.registry.UpdatePrepareStateForConfig(ctx, cfg, state, msg); err != nil {
		return err
	}
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok {
		if !samePrepareConfig(rt.cfg, cfg) {
			s.mu.Unlock()
			return nil
		}
		rt.cfg.PrepareState = state
		rt.cfg.PrepareError = msg
		rt.state.PrepareError = msg
		if state != model.PrepareStateReady || applyReadyRuntime {
			rt.state.State = runtimeStateForPrepareState(state)
		}
		if rt.gate != nil {
			switch state {
			case model.PrepareStateFailed:
				rt.gate.MarkFailed(prepareGateError(msg))
			case model.PrepareStateReady:
				if applyReadyRuntime {
					rt.gate.MarkReady()
				}
			default:
				rt.gate.Reset()
			}
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) publishSnapshot(ctx context.Context, cfg model.RepoConfig, snap *snapshot.Store, oid string, ref string) (int64, string, error) {
	nodes, err := s.git.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		return 0, snapshotPhaseBuild, err
	}
	gen, err := snap.PublishGeneration(ctx, oid, ref, nodes)
	if err != nil {
		return 0, snapshotPhasePublish, err
	}
	return gen, "", nil
}

func isAsyncRepo(cfg model.RepoConfig) bool {
	if cfg.PreparedGitDir {
		return true
	}
	switch strings.TrimSpace(cfg.PrepareState) {
	case model.PrepareStatePreparing, model.PrepareStateReady, model.PrepareStateFailed:
		return true
	default:
		return false
	}
}

func shouldMountAsync(cfg model.RepoConfig) bool {
	return isPendingOrFailedPrepareState(cfg.PrepareState)
}

func isPendingOrFailedPrepareState(state string) bool {
	switch strings.TrimSpace(state) {
	case model.PrepareStatePreparing, model.PrepareStateFailed:
		return true
	default:
		return false
	}
}

func runtimeStateForPrepareState(state string) string {
	if state == model.PrepareStateReady {
		return repoStateMounted
	}
	return state
}

func prepareGateError(msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fusefs.ErrRepoNotReady
	}
	return errors.New(msg)
}
