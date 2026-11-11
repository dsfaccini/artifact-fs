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

// errPrepareSuperseded is the error of a preparation whose config or prepare
// request is no longer the registry's.
var errPrepareSuperseded = fmt.Errorf("prepare superseded by a newer repo config or prepare request: %w", registry.ErrRepoChanged)

// prepareIsCurrent reports whether cfg's config and prepare request are still
// the registry's.
func (s *core) prepareIsCurrent(ctx context.Context, cfg model.RepoConfig) (bool, error) {
	latest, err := s.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		return false, err
	}
	return latest.ConfigVersion == cfg.ConfigVersion && latest.PrepareRequest == cfg.PrepareRequest, nil
}

// withCurrentPrepare runs fn under cfg's repo config lock while cfg's config
// and prepare request are still the registry's, and returns
// errPrepareSuperseded otherwise.
func (s *core) withCurrentPrepare(ctx context.Context, cfg model.RepoConfig, fn func() error) error {
	return s.withRepoConfigLock(ctx, cfg.Name, func() error {
		current, err := s.prepareIsCurrent(ctx, cfg)
		if err != nil {
			return err
		}
		if !current {
			return errPrepareSuperseded
		}
		return fn()
	})
}

// preparation is what differs between the runs of the one prepare pipeline:
// add-repo and prepare on a sync repo, the daemon's async preparation, and
// the daemon's check of a prepared repo before it mounts it.
type preparation struct {
	// mode is the mode field of the preparation logs. An empty mode logs
	// nothing.
	mode    string
	attempt int64
	// ambient makes git use only ambient credentials, and rejects a remote
	// URL with inline credentials. Otherwise a credential helper hands git the
	// remote URL's inline credentials.
	ambient bool
	// fetch makes an existing clone fetch FetchRef and move its branch to it.
	// Otherwise the pipeline takes the clone's HEAD as it is.
	fetch bool
	// mountPoint creates cfg's mount point first.
	mountPoint bool
	// snapshot returns the snapshot store the pipeline publishes in, and
	// whether the pipeline owns it. nil opens cfg's store.
	snapshot func(context.Context, model.RepoConfig) (*snapshot.Store, bool, error)
	// complete persists the result, or starts or activates the runtime that
	// serves it. It runs under the repo's prepare lock.
	complete func(context.Context, *prepared) error
	// fail records a failed preparation and returns the error the caller gets.
	// nil records nothing.
	fail func(context.Context, model.RepoConfig, error) error
}

// prepared is what the pipeline hands complete.
type prepared struct {
	cfg    model.RepoConfig
	source model.PreparedSource
	snap   *snapshot.Store
	// gen is the generation that holds source's commit, and prevGen the one
	// the store held before the pipeline published.
	gen, prevGen int64
	// kept means complete took snap over, so the pipeline must not close it.
	kept bool
}

// prepare runs the one prepare pipeline on cfg under its repo's prepare lock:
// it checks that cfg's config and prepare request are current, acquires
// cfg's source, publishes the snapshot of the source's commit, records the
// acquisition, then completes as p says. prepareContext bounds it.
func (s *core) prepare(ctx context.Context, cfg model.RepoConfig, p preparation) (retErr error) {
	prepareCtx, cancel := s.withPrepareTimeout(ctx)
	defer cancel()
	source := prepareSourceOf(cfg)
	var res prepared
	if p.mode != "" {
		logEnd := s.logPreparation(prepareCtx, cfg, p, &source)
		defer func() { logEnd(res, retErr) }()
	}
	err := s.withRepoPrepareLock(prepareCtx, cfg.Name, func() error {
		current, err := s.prepareIsCurrent(prepareCtx, cfg)
		if err != nil {
			return withPreparationPhase(preparePhaseValidate, err)
		}
		if !current {
			return withPreparationPhase(preparePhaseValidate, errPrepareSuperseded)
		}
		if p.mountPoint {
			if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
				return withPreparationPhase(preparePhaseInitializeStorage, err)
			}
		}
		res.cfg = cfg
		res.source, source, err = s.acquireSource(prepareCtx, cfg, p)
		if err != nil {
			return err
		}
		owned := true
		if p.snapshot != nil {
			res.snap, owned, err = p.snapshot(prepareCtx, cfg)
		} else {
			res.snap, err = snapshot.New(prepareCtx, cfg.MetaDBPath)
		}
		if err != nil {
			return withPreparationPhase(preparePhaseOpenSnapshot, err)
		}
		if owned {
			defer func() {
				if !res.kept {
					res.snap.Close()
				}
			}()
		}
		if err := s.publishPrepared(prepareCtx, &res); err != nil {
			return err
		}
		if err := s.recordAcquisition(prepareCtx, cfg, res.source); err != nil {
			return withPreparationPhase(preparePhasePersistReady, err)
		}
		return p.complete(prepareCtx, &res)
	})
	if err == nil {
		return nil
	}
	if ctxErr := prepareCtx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = errors.Join(err, ctxErr)
	}
	if p.fail != nil {
		return p.fail(ctx, cfg, err)
	}
	return err
}

// prepareSourceOf is the source field of cfg's preparation logs.
func prepareSourceOf(cfg model.RepoConfig) string {
	switch {
	case cfg.RequiredCommit != "":
		return prepareSourceVerified
	case cfg.PreparedGitDir:
		return prepareSourcePreparedGitDir
	}
	if _, err := os.Stat(cfg.GitDir); err == nil {
		return prepareSourceExistingClone
	}
	return prepareSourceFreshClone
}

// logPreparation logs the start of cfg's preparation, and returns what logs
// its end. source is the source field, which acquisition can still change.
func (s *core) logPreparation(ctx context.Context, cfg model.RepoConfig, p preparation, source *string) func(prepared, error) {
	started := time.Now()
	deadlineSet := false
	timeoutMS := int64(0)
	if deadline, ok := ctx.Deadline(); ok {
		deadlineSet = true
		timeoutMS = max(time.Until(deadline).Milliseconds(), 0)
	}
	logger := s.logger.With("repo", auth.RedactLogString(cfg.Name), "mode", p.mode, "attempt", p.attempt)
	logger.InfoContext(ctx, logRepoPreparationStarted, "source", *source, "phase", preparePhaseValidate, "state", prepareLogStateStarted, "duration_ms", 0, "branch", auth.RedactLogString(cfg.Branch), "fetch_ref", auth.RedactLogString(cfg.FetchRef), "deadline_set", deadlineSet, "timeout_ms", timeoutMS)
	return func(res prepared, err error) {
		durationMS := time.Since(started).Milliseconds()
		if err == nil {
			logger.InfoContext(ctx, logRepoPreparationCompleted, "source", *source, "phase", preparePhaseComplete, "state", prepareLogStateCompleted, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "head_oid", auth.RedactLogString(res.source.Commit), "head_ref", auth.RedactLogString(res.source.Ref), "snapshot_generation", res.gen)
			return
		}
		timedOut := errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
		canceled := errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		state := prepareLogStateFailed
		if canceled && !timedOut {
			state = prepareLogStateCanceled
		}
		args := []any{"source", *source, "phase", preparationFailurePhase(err), "state", state, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "timed_out", timedOut, "canceled", canceled, "error", auth.RedactLogString(err.Error(), cfg.RemoteURL)}
		if canceled && !timedOut {
			logger.InfoContext(ctx, logRepoPreparationCanceled, args...)
			return
		}
		logger.ErrorContext(ctx, logRepoPreparationFailed, args...)
	}
}

// acquireSource brings cfg's git dir to the commit the snapshot must hold
// and returns it, with the source field of the preparation logs.
func (s *core) acquireSource(ctx context.Context, cfg model.RepoConfig, p preparation) (model.PreparedSource, string, error) {
	source := prepareSourceOf(cfg)
	switch {
	case cfg.AdoptGitDir:
		if err := s.git.ConfigureAdoptedWorktree(ctx, cfg); err != nil {
			return model.PreparedSource{}, source, withPreparationPhase(preparePhaseClone, err)
		}
		// Adoption runs once per prepare and once per mount.
		s.git.WarnAdoptedRemoteCredentials(ctx, cfg)
	case cfg.RequiredCommit != "":
		prepared, err := s.git.PrepareSource(ctx, cfg, model.SourceRequirement{
			Ref:            cfg.Branch,
			RequiredCommit: cfg.RequiredCommit,
			Depth:          cfg.HistoryDepth,
		})
		return prepared, source, withPreparationPhase(preparePhaseClone, err)
	case cfg.PreparedGitDir && p.fetch:
		if err := s.git.ValidatePreparedGitDir(ctx, cfg); err != nil {
			return model.PreparedSource{}, source, withPreparationPhase(preparePhaseValidate, err)
		}
		if err := s.git.FetchRefAmbient(ctx, cfg, cfg.FetchRef); err != nil {
			return model.PreparedSource{}, source, withPreparationPhase(preparePhaseFetch, err)
		}
		if err := s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef); err != nil {
			return model.PreparedSource{}, source, withPreparationPhase(preparePhaseUpdateBranch, err)
		}
	default:
		_, statErr := os.Stat(cfg.GitDir)
		switch {
		case statErr == nil:
			if p.fetch {
				if err := s.refreshClone(ctx, cfg, p); err != nil {
					return model.PreparedSource{}, source, err
				}
			}
		case !errors.Is(statErr, os.ErrNotExist) || cfg.PreparedGitDir:
			return model.PreparedSource{}, source, withPreparationPhase(preparePhaseValidate, statErr)
		default:
			source = prepareSourceFreshClone
			if err := s.cloneSource(ctx, cfg, p); err != nil {
				return model.PreparedSource{}, source, err
			}
		}
	}
	headOID, headRef, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return model.PreparedSource{}, source, withPreparationPhase(preparePhaseResolveHEAD, err)
	}
	return model.PreparedSource{Ref: headRef, Commit: headOID}, source, nil
}

// cloneSource clones cfg's branch blobless, then moves it to FetchRef when
// FetchRef names another ref.
func (s *core) cloneSource(ctx context.Context, cfg model.RepoConfig, p preparation) error {
	if p.ambient {
		if err := s.git.ValidateAmbientRemote(cfg); err != nil {
			return withPreparationPhase(preparePhaseValidate, err)
		}
	}
	if err := s.git.CloneBlobless(ctx, cfg); err != nil {
		return withPreparationPhase(preparePhaseClone, err)
	}
	if cfg.FetchRef == cfg.Branch {
		return nil
	}
	return s.refreshClone(ctx, cfg, p)
}

// refreshClone points an existing clone's origin at cfg's remote, fetches
// FetchRef and moves the clone's branch to it. A clone registered without a
// remote URL, because add-repo dropped its inline credentials, keeps its
// origin and is not refreshed.
func (s *core) refreshClone(ctx context.Context, cfg model.RepoConfig, p preparation) error {
	if p.ambient {
		if err := s.git.ValidateAmbientRemote(cfg); err != nil {
			return withPreparationPhase(preparePhaseValidate, err)
		}
		if err := s.git.ConfigureRemoteAmbient(ctx, cfg); err != nil {
			return withPreparationPhase(preparePhaseConfigureRemote, err)
		}
		if err := s.git.FetchRefAmbient(ctx, cfg, cfg.FetchRef); err != nil {
			return withPreparationPhase(preparePhaseFetch, err)
		}
	} else {
		if strings.TrimSpace(cfg.RemoteURL) == "" {
			return nil
		}
		if err := s.git.ConfigureRemoteWithCredentials(ctx, cfg); err != nil {
			return withPreparationPhase(preparePhaseConfigureRemote, err)
		}
		if err := s.git.FetchRefWithCredentials(ctx, cfg, cfg.FetchRef); err != nil {
			return withPreparationPhase(preparePhaseFetch, err)
		}
	}
	return withPreparationPhase(preparePhaseUpdateBranch, s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef))
}

// publishPrepared publishes the snapshot of res's source in res.snap unless
// the store already holds it, while res's config and request are current.
func (s *core) publishPrepared(ctx context.Context, res *prepared) error {
	phase := preparePhaseBuildTree
	err := s.withCurrentPrepare(ctx, res.cfg, func() error {
		storedOID, storedRef, storedGen, readErr := res.snap.ReadState(ctx)
		res.gen, res.prevGen = storedGen, storedGen
		if readErr == nil && storedGen != 0 && storedOID == res.source.Commit && storedRef == res.source.Ref {
			return nil
		}
		gen, publishPhase, err := s.publishSnapshot(ctx, res.cfg, res.snap, res.source.Commit, res.source.Ref)
		if publishPhase == snapshotPhasePublish {
			phase = preparePhasePublishSnapshot
		}
		res.gen = gen
		return err
	})
	return withPreparationPhase(phase, err)
}

func (s *core) recordAcquisition(ctx context.Context, cfg model.RepoConfig, source model.PreparedSource) error {
	if !source.Verified || (!source.Acquired && cfg.AcquiredRef == source.Ref && strings.EqualFold(cfg.AcquiredCommit, source.Commit)) {
		return nil
	}
	return s.registry.RecordAcquisition(ctx, cfg, source)
}

// prepareSync prepares cfg in the CLI, with cfg's inline credentials, and
// persists the outcome for the daemon, which mounts the repo once it
// succeeded.
func (s *core) prepareSync(ctx context.Context, cfg model.RepoConfig) error {
	return s.prepare(ctx, cfg, preparation{
		mode:       prepareModeSync,
		attempt:    1,
		fetch:      true,
		mountPoint: true,
		complete: func(ctx context.Context, res *prepared) error {
			stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
			defer cancel()
			return withPreparationPhase(preparePhasePersistReady, s.registry.UpdatePrepareStateForRequest(stateCtx, res.cfg, "", ""))
		},
		fail: s.persistSyncFailure,
	})
}

// persistSyncFailure records a failed sync preparation as the empty prepare
// state with its error, unless a newer config or request superseded it.
func (s *core) persistSyncFailure(ctx context.Context, cfg model.RepoConfig, prepareErr error) error {
	if errors.Is(prepareErr, registry.ErrRepoChanged) {
		return prepareErr
	}
	stateErr := prepareErr
	if errors.Is(prepareErr, context.DeadlineExceeded) {
		stateErr = errors.New("prepare timed out")
	} else if errors.Is(prepareErr, context.Canceled) {
		stateErr = errors.New("prepare canceled")
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
	defer cancel()
	if err := s.registry.UpdatePrepareStateForRequest(stateCtx, cfg, "", auth.RedactLogString(stateErr.Error(), cfg.RemoteURL)); err != nil {
		if errors.Is(err, registry.ErrRepoChanged) {
			return prepareErr
		}
		s.logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "repo", auth.RedactLogString(cfg.Name), "mode", prepareModeSync, "phase", preparationFailurePhase(prepareErr), "target_state", "", "error", auth.RedactLogString(err.Error()))
		return errors.Join(prepareErr, fmt.Errorf("persist failed prepare state: %w", err))
	}
	return prepareErr
}

// prepareAsync prepares cfg in the daemon with ambient credentials, then
// persists it ready and activates the runtime that waits behind its gate.
func (s *Service) prepareAsync(ctx context.Context, cfg model.RepoConfig, attempt int64) error {
	return s.prepare(ctx, cfg, preparation{
		mode:     prepareModeAsync,
		attempt:  attempt,
		ambient:  true,
		fetch:    true,
		snapshot: s.snapshotForPrepare,
		complete: s.activatePrepared,
		fail:     s.persistAsyncFailure,
	})
}

// activatePrepared persists res ready, then switches the runtime that waits
// for it to res's generation and opens its gate.
func (s *Service) activatePrepared(ctx context.Context, res *prepared) error {
	cfg := res.cfg
	if err := s.setPrepareStateBeforeReadyGate(ctx, cfg); err != nil {
		return withPreparationPhase(preparePhasePersistReady, err)
	}
	runHook(s.readyRecorded)
	if err := s.completePreparedRuntime(ctx, cfg, res.source.Commit, res.source.Ref, res.gen); err != nil {
		return withPreparationPhase(preparePhaseActivateRuntime, err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	rt := s.running[cfg.ID]
	s.mu.Unlock()
	// A runtime's prunes run under its headMu, so they record its kept
	// generations one at a time. Take headMu before the repo config lock: a
	// reconcile can hold headMu through its git commands, and the wait must
	// not hold the config lock that CLI commands take.
	err := func() error {
		if rt != nil {
			rt.headMu.Lock()
			defer rt.headMu.Unlock()
		}
		return s.withCurrentPrepare(cleanupCtx, cfg, func() error {
			return s.pruneGenerations(cleanupCtx, rt, res.snap, min(res.prevGen, res.gen-1))
		})
	}()
	if err != nil && !errors.Is(err, registry.ErrRepoChanged) {
		s.logger.Warn("snapshot generation cleanup failed", "repo", cfg.Name, "error", err)
	}
	s.configureStatusOptimization(ctx, cfg)
	return nil
}

// persistAsyncFailure records a failed async preparation as failed and fails
// its runtime's gate. A canceled preparation stays queued for the next
// daemon, and a superseded one records nothing.
func (s *Service) persistAsyncFailure(ctx context.Context, cfg model.RepoConfig, prepareErr error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(prepareErr, context.Canceled) || errors.Is(prepareErr, registry.ErrRepoChanged) {
		return prepareErr
	}
	stateErr := prepareErr
	if errors.Is(prepareErr, context.DeadlineExceeded) {
		stateErr = errors.New("prepare timed out")
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
	defer cancel()
	if err := s.setPrepareState(stateCtx, cfg, model.PrepareStateFailed, stateErr); err != nil {
		if errors.Is(err, registry.ErrRepoChanged) {
			return prepareErr
		}
		s.logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "repo", auth.RedactLogString(cfg.Name), "mode", prepareModeAsync, "phase", preparationFailurePhase(prepareErr), "target_state", model.PrepareStateFailed, "error", auth.RedactLogString(err.Error()))
		return errors.Join(prepareErr, fmt.Errorf("persist failed prepare state: %w", err))
	}
	return prepareErr
}

// prepareWorker is a running async preparation. cancel ends it.
type prepareWorker struct {
	token  int64
	cancel context.CancelFunc
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
	workerCtx, cancel := context.WithCancel(workerCtx)
	s.preparing[cfg.ID] = prepareWorker{token: token, cancel: cancel}
	s.prepareWorkers.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.prepareWorkers.Done()
		if trackedRuntime {
			defer rt.workers.Done()
		}
		defer cancel()
		defer func() {
			s.mu.Lock()
			if s.preparing[cfg.ID].token == token {
				delete(s.preparing, cfg.ID)
			}
			s.mu.Unlock()
		}()
		_ = s.prepareAsync(workerCtx, cfg, attempt)
	}()
}

// supersedePrepare cancels id's running async preparation, which releases
// the repo's prepare lock for the preparation that replaces it.
func (s *Service) supersedePrepare(id model.RepoID) {
	s.mu.Lock()
	worker, ok := s.preparing[id]
	delete(s.preparing, id)
	s.mu.Unlock()
	if ok && worker.cancel != nil {
		worker.cancel()
	}
}

func (s *core) withPrepareTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.prepareContext != nil {
		return s.prepareContext(ctx)
	}
	return context.WithTimeout(ctx, defaultPrepareTimeout)
}

// resetRunningPrepareState makes cfg's runtime wait for cfg's preparation:
// its gate holds FUSE ops again and it reports preparing. The runtime stays
// mounted. It reports false when the runtime has no gate.
func (s *Service) resetRunningPrepareState(cfg model.RepoConfig) bool {
	s.mu.Lock()
	rt, ok := s.running[cfg.ID]
	if !ok || rt.gate == nil {
		s.mu.Unlock()
		return false
	}
	rt.gate.Reset()
	rt.cfg.PrepareRequest = cfg.PrepareRequest
	rt.cfg.PrepareState = cfg.PrepareState
	rt.cfg.PrepareError = cfg.PrepareError
	rt.prepare = model.PrepareStatePreparing
	s.mu.Unlock()
	s.recordRuntimeState(rt)
	return true
}

// samePrepare reports whether a and b hold the same config and prepare
// request.
func samePrepare(a, b model.RepoConfig) bool {
	return a.ConfigVersion == b.ConfigVersion && a.PrepareRequest == b.PrepareRequest
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
	if rt != nil && !samePrepare(rt.cfg, cfg) {
		s.mu.Unlock()
		return errPrepareSuperseded
	}
	s.mu.Unlock()
	if rt == nil {
		return nil
	}
	if err := s.bootstrapCleanStatusIndex(ctx, cfg, headOID, gen, rt.snapshot, rt.overlay); err != nil {
		return err
	}
	commitTime := int64(0)
	if ts, err := s.git.CommitTimestamp(ctx, cfg, headOID); err == nil {
		commitTime = ts
	} else {
		s.logger.Warn("commit timestamp unavailable", "repo", cfg.Name, "error", err)
	}
	if err := s.switchGeneration(ctx, rt, gen, commitTime); err != nil {
		return err
	}
	s.mu.Lock()
	if s.running[cfg.ID] != rt || !samePrepare(rt.cfg, cfg) {
		s.mu.Unlock()
		return errPrepareSuperseded
	}
	rt.cfg.PrepareState = model.PrepareStateReady
	rt.cfg.PrepareError = ""
	rt.head = servedHead{oid: headOID, ref: headRef, gen: gen}
	rt.prepare = model.PrepareStateReady
	s.mu.Unlock()
	rt.gate.MarkReady()
	s.recordRuntimeState(rt)
	s.startRepoBackground(rt)
	return nil
}

func (s *Service) setPrepareState(ctx context.Context, cfg model.RepoConfig, state model.PrepareState, stateErr error) error {
	return s.applyPrepareState(ctx, cfg, state, stateErr, true)
}

func (s *Service) setPrepareStateBeforeReadyGate(ctx context.Context, cfg model.RepoConfig) error {
	return s.applyPrepareState(ctx, cfg, model.PrepareStateReady, nil, false)
}

func (s *Service) applyPrepareState(ctx context.Context, cfg model.RepoConfig, state model.PrepareState, stateErr error, applyReadyRuntime bool) error {
	msg := ""
	if stateErr != nil {
		msg = auth.RedactLogString(stateErr.Error(), cfg.RemoteURL)
	}
	if err := s.registry.UpdatePrepareStateForRequest(ctx, cfg, state, msg); err != nil {
		return err
	}
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok {
		if !samePrepare(rt.cfg, cfg) {
			s.mu.Unlock()
			return nil
		}
		defer s.recordRuntimeState(rt)
		rt.cfg.PrepareState = state
		rt.cfg.PrepareError = msg
		if state != model.PrepareStateReady || applyReadyRuntime {
			rt.prepare = state
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

func (s *core) publishSnapshot(ctx context.Context, cfg model.RepoConfig, snap *snapshot.Store, oid string, ref string) (int64, string, error) {
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
	switch cfg.PrepareState {
	case model.PrepareStatePreparing, model.PrepareStateReady, model.PrepareStateFailed:
		return true
	default:
		return false
	}
}

func shouldMountAsync(cfg model.RepoConfig) bool {
	return isPendingOrFailedPrepareState(cfg.PrepareState)
}

func isPendingOrFailedPrepareState(state model.PrepareState) bool {
	switch state {
	case model.PrepareStatePreparing, model.PrepareStateFailed:
		return true
	default:
		return false
	}
}

func prepareGateError(msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fusefs.ErrRepoNotReady
	}
	return errors.New(msg)
}
