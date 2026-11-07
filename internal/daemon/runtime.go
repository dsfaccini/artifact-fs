package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/cloudflare/artifact-fs/internal/watcher"
)

const (
	// mountTimeout bounds the wait for one FUSE mount, so a mount service
	// that stalls costs the sync loop at most this long.
	mountTimeout = 30 * time.Second
	// unmountTimeout bounds how long an unmount retries a busy mount and how
	// long a stop waits for it. An unmount(2) that hangs keeps running on its
	// own goroutine past it.
	unmountTimeout = 20 * time.Second
	// unmountReturnSlack is how long after unmountTimeout an unmount that
	// does not hang takes to return.
	unmountReturnSlack = time.Second
	// drainTimeout bounds how long a stop waits for a runtime's workers and
	// its FUSE server once it is unmounted. It stays above gitstore's
	// rollback timeout, so a stop waits out the rollback of the preparation
	// it cancels.
	drainTimeout = 30 * time.Second
)

// mountState is where a runtime's FUSE mount stands.
type mountState string

const (
	// mountNone: nothing of the runtime is mounted, and its next mount
	// attempt mounts it.
	mountNone mountState = "none"
	// mountServing: rt.mfs is mounted and its server serves.
	mountServing mountState = "serving"
	// mountFailed: rt.mfs's connection ended while its mount may still hold
	// the mount point. It is unmounted before the runtime mounts again.
	mountFailed mountState = "failed"
	// mountAbandoned: a mount whose wait timed out ends in the background,
	// and rt.abandoned reports when it has.
	mountAbandoned mountState = "abandoned"
	// mountUnmounted: a stop unmounted the runtime, which never mounts again.
	mountUnmounted mountState = "unmounted"
)

// fetchState is the result of a runtime's last remote refresh, which status
// reports as last_fetch's result.
type fetchState string

const (
	fetchNever  fetchState = "never"
	fetchOK     fetchState = "ok"
	fetchFailed fetchState = "failed"
)

// servedHead is the snapshot generation a runtime serves and the HEAD it was
// built from.
type servedHead struct {
	oid string
	ref string
	gen int64
}

// fetchStatus is a runtime's last remote refresh and how its HEAD compares
// with the fetched ref.
type fetchStatus struct {
	state    fetchState
	at       time.Time
	ahead    int
	behind   int
	diverged bool
}

type repoRuntime struct {
	// Set when the runtime starts, then read-only. ctx ends when the runtime
	// stops or the daemon shuts down, and stops the runtime's workers.
	ctx    context.Context
	cancel context.CancelFunc
	// opCtx is the parent of every FUSE op's context. Only a stop ends it,
	// after the unmount, so a shutdown unmounts before it cancels ops.
	opCtx     context.Context
	cancelOps context.CancelFunc
	snapshot  *snapshot.Store
	overlay   *overlay.Store
	hydrator  *hydrator.Service
	sizes     *sizeUpdateBatcher
	resolver  *fusefs.Resolver
	engine    *fusefs.Engine
	// gate holds FUSE ops while an async repo prepares; nil for a sync repo.
	gate    *fusefs.ReadyGate
	refresh chan time.Duration
	headMu  sync.Mutex
	workers sync.WaitGroup

	// Guarded by Service.mu.
	cfg model.RepoConfig
	// prepare is the preparation the runtime serves: preparing or failed
	// while its gate holds ops, ready or empty once it serves a prepared
	// snapshot.
	prepare     model.PrepareState
	head        servedHead
	fetch       fetchStatus
	mount       mountState
	mfs         fusefs.MountedFS
	abandoned   fusefs.AbandonedMount
	headChanges chan struct{}
	// joinDone closes when the join of the runtime's last mount ends.
	joinDone chan struct{}
	// drained closes once a stop finds the runtime's workers and server done.
	drained chan struct{}
	active  bool
	// stopping holds from a stop's start until its unmount fails or the
	// runtime is gone. Nothing mounts or reconciles HEAD meanwhile.
	stopping bool
	// stopRunning is set while a stop runs.
	stopRunning bool
	// retired holds the generations the resolver stopped serving, with when.
	// retiredMu guards it, and is taken alone.
	retiredMu sync.Mutex
	retired   []retiredGeneration
}

type retiredGeneration struct {
	gen int64
	at  time.Time
}

// reportedState is the runtime's state in status: the prepare state while an
// async repo prepares, degraded while its mount or its last remote refresh
// failed, and mounted otherwise.
func (rt *repoRuntime) reportedState() string {
	switch {
	case isPendingOrFailedPrepareState(rt.prepare):
		return string(rt.prepare)
	case rt.mount != mountServing || rt.fetch.state == fetchFailed:
		return repoStateDegraded
	default:
		return repoStateMounted
	}
}

// phase is the runtime's phase in status. A runtime with no FUSE mount
// serves nothing while the daemon retries the mount.
func (rt *repoRuntime) phase() string {
	switch {
	case rt.mount != mountServing:
		return phaseMount
	case isPendingOrFailedPrepareState(rt.prepare):
		return phasePrepare
	default:
		return phaseServe
	}
}

// startRuntime builds cfg's runtime around snap and ov, which it owns from
// then on, records it as running and mounts it. head is the generation it
// serves. An async repo passes the gate that holds FUSE ops until its
// preparation ends; a prepared repo passes nil.
func (s *Service) startRuntime(ctx context.Context, cfg model.RepoConfig, snap *snapshot.Store, ov *overlay.Store, head servedHead, gate *fusefs.ReadyGate) *repoRuntime {
	h := hydrator.New(s.git)
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(head.gen)
	if head.oid != "" {
		s.refreshCommitTime(ctx, cfg, head.oid, resolver, "commit timestamp unavailable, mtime will use generation fallback")
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	opCtx, cancelOps := context.WithCancel(context.WithoutCancel(ctx))
	sizes := newSizeUpdateBatcher(snap, s.logger, cfg.Name)
	sizes.Start(runtimeCtx)
	h.SetOnHydrated(func(_ model.RepoID, objectOID string, size int64) {
		sizes.Add(resolver.Generation(), objectOID, size)
	})
	h.Start(s.hydrationWorkers(), cfg.RepoPaths)
	prepare := cfg.PrepareState
	if gate != nil && prepare == "" {
		prepare = model.PrepareStatePreparing
	}
	rt := &repoRuntime{
		ctx:       runtimeCtx,
		cancel:    cancel,
		opCtx:     opCtx,
		cancelOps: cancelOps,
		snapshot:  snap,
		overlay:   ov,
		hydrator:  h,
		sizes:     sizes,
		resolver:  resolver,
		engine:    &fusefs.Engine{Resolver: resolver, Repo: cfg.RepoPaths, Overlay: ov, Hydrator: h},
		gate:      gate,
		refresh:   make(chan time.Duration, 1),
		cfg:       cfg,
		prepare:   prepare,
		head:      head,
		fetch:     fetchStatus{state: fetchNever},
		mount:     mountNone,
	}
	s.mu.Lock()
	s.running[cfg.ID] = rt
	s.mu.Unlock()
	// A runtime that starts ends the backoff of the mounts before it. The next
	// registry sync retries a first mount that failed; later failures back off.
	s.mountSucceeded(cfg.ID)
	if _, err := s.attachMount(rt); err != nil {
		s.recordFailure(cfg, phaseMount, err, time.Now().Add(registrySyncInterval))
	}
	runHook(s.runtimeStarted)
	s.recordRuntimeState(rt)
	return rt
}

// retryMount mounts rt when it has no mount and its mount backoff has
// passed, and records the outcome.
func (s *Service) retryMount(rt *repoRuntime) {
	s.mu.Lock()
	cfg := rt.cfg
	s.mu.Unlock()
	if !s.mountDue(cfg) {
		return
	}
	attached, err := s.attachMount(rt)
	if _, settling := errors.AsType[*settlingError](err); settling {
		s.recordFailure(cfg, phaseMount, err, time.Now().Add(registrySyncInterval))
		return
	}
	if err != nil {
		s.recordFailure(cfg, phaseMount, err, s.backoffMount(cfg))
		return
	}
	if attached {
		s.mountSucceeded(cfg.ID)
		s.recordRuntimeState(rt)
	}
}

// attachMount mounts rt once every precondition holds:
//   - rt is not stopping and has no mount;
//   - no unmount of the repo runs;
//   - a failed mount of rt is unmounted, and an abandoned one has ended;
//   - the mount point holds no mount that an earlier daemon process left;
//   - an adopted worktree's mount point is empty, because a mount would hide
//     its native files.
//
// It reports whether it mounted rt. A runtime that serves, stops or has
// stopped returns false and no error. An unmount of the repo that runs, or an
// abandoned mount that has not ended, returns a settlingError, and the next
// attempt checks again. The mount waits at most mountTimeout, and rt's op
// context is the parent of every op's context.
func (s *Service) attachMount(rt *repoRuntime) (bool, error) {
	s.mu.Lock()
	cfg, state, failed, abandoned := rt.cfg, rt.mount, rt.mfs, rt.abandoned
	settled := rt.stopping || state == mountServing || state == mountUnmounted || rt.ctx.Err() != nil
	unmounting := s.unmounting[cfg.ID] != nil
	s.mu.Unlock()
	if settled {
		return false, nil
	}
	if unmounting {
		return false, fmt.Errorf("mount %s: %w", cfg.Name, errUnmountInProgress)
	}
	switch state {
	case mountFailed:
		op, started := s.startUnmount(cfg, s.unmountHeld(cfg, failed), func(err error) { s.releaseMount(rt, failed, err, mountNone) })
		if !started {
			return false, fmt.Errorf("mount %s: %w", cfg.Name, errUnmountInProgress)
		}
		if err := s.waitUnmount(op); err != nil {
			return false, fmt.Errorf("unmount %s after its connection failed: %w", cfg.Name, err)
		}
	case mountAbandoned:
		if err := s.releaseAbandonedMount(cfg, abandoned); err != nil {
			return false, err
		}
		s.mu.Lock()
		if rt.abandoned == abandoned {
			rt.abandoned, rt.mount = nil, mountNone
		}
		s.mu.Unlock()
	}
	if err := s.releaseLeftoverMount(cfg); err != nil {
		return false, err
	}
	if cfg.AdoptGitDir {
		entries, err := os.ReadDir(cfg.MountPath)
		if err != nil {
			return false, err
		}
		if len(entries) != 0 {
			return false, errors.New("adopted mountpoint must be empty; native working files are not imported")
		}
	}
	ctx, cancel := context.WithTimeout(rt.ctx, cmp.Or(s.mountTimeout, mountTimeout))
	defer cancel()
	mfs, err := s.mount(ctx, rt.opCtx, cfg.RepoPaths, rt.engine, rt.gate, s.logger, func(msg string) string { return auth.RedactLogString(msg, cfg.RemoteURL) })
	if err != nil {
		if a, ok := errors.AsType[fusefs.AbandonedMount](err); ok {
			s.mu.Lock()
			rt.abandoned, rt.mount = a, mountAbandoned
			s.mu.Unlock()
		}
		s.logger.Error("fuse mount failed, runtime will retry", "repo", cfg.Name, "error", auth.RedactLogString(err.Error(), cfg.RemoteURL))
		return false, err
	}
	s.logger.Info("fuse mounted", "repo", cfg.Name, "backend", mfs.Backend())
	s.mu.Lock()
	if rt.stopping || s.running[cfg.ID] != rt {
		s.mu.Unlock()
		// A stop began while the mount ran, which no longer serves rt.
		op, started := s.startUnmount(cfg, mfs.Unmount, nil)
		if started {
			_ = s.waitUnmount(op)
		}
		return false, nil
	}
	rt.mfs, rt.mount = mfs, mountServing
	// Every later mount at the mount point is this daemon's.
	delete(s.leftovers, cfg.ID)
	s.mu.Unlock()
	// A mount that serves ends the mount and connection failures before it.
	// The new connection can fail at once, so the join starts after.
	s.recordRuntime(cfg.ID, func(rec *model.RepoRuntimeRecord) {
		clearFailure(rec, phaseMount)
		clearFailure(rec, phaseServe)
	})
	s.joinMount(rt, mfs)
	if !isPendingOrFailedPrepareState(cfg.PrepareState) {
		s.configureStatusOptimization(rt.ctx, cfg)
	}
	return true, nil
}

// settlingError is a mount precondition that is still settling: an unmount
// of the repo that runs, or an abandoned mount that has not ended. The next
// sync checks it again, with no backoff.
type settlingError struct{ msg string }

func (e *settlingError) Error() string { return e.msg }

var (
	// errUnmountInProgress is the error of a mount or a stop while an
	// earlier unmount of the repo has not returned.
	errUnmountInProgress = &settlingError{"an earlier unmount of the repo has not returned"}
	// errAbandonedMountRunning is the error of a mount while an abandoned
	// mount of the repo has not ended.
	errAbandonedMountRunning = &settlingError{"an abandoned mount of the repo has not ended"}
)

// releaseAbandonedMount returns nil once abandoned, a mount whose wait timed
// out, has ended and left nothing at cfg's mount point. A late mount whose
// cleanup failed stays at the mount point whatever source the mount table
// names, so it unmounts the mount point by path.
func (s *Service) releaseAbandonedMount(cfg model.RepoConfig, abandoned fusefs.AbandonedMount) error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := abandoned.Wait(ctx)
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("mount %s: %w", cfg.Name, errAbandonedMountRunning)
	}
	if err == nil {
		return nil
	}
	if _, held := s.mountPointEntry(cfg.MountPath); !held {
		return nil
	}
	s.logger.Warn("unmounting the abandoned mount the library could not", "repo", cfg.Name, "error", auth.RedactLogString(err.Error(), cfg.RemoteURL))
	return s.unmountMountPoint(cfg)
}

// unmountMountPoint unmounts whatever cfg's mount point holds, by path.
func (s *Service) unmountMountPoint(cfg model.RepoConfig) error {
	op, started := s.startUnmount(cfg, func(context.Context) error { return s.unmountPath(cfg.MountPath) }, nil)
	if !started {
		return fmt.Errorf("mount %s: %w", cfg.Name, errUnmountInProgress)
	}
	if err := s.waitUnmount(op); err != nil {
		return fmt.Errorf("unmount %s: %w", cfg.MountPath, err)
	}
	return nil
}

// releaseMount marks mfs, rt's mount, as gone once its unmount succeeded,
// leaving rt's mount in next. The caller holds s.mu.
func (s *Service) releaseMount(rt *repoRuntime, mfs fusefs.MountedFS, err error, next mountState) {
	if err == nil && rt.mfs == mfs {
		rt.mfs, rt.mount = nil, next
	}
}

// releaseLeftoverMount unmounts the mount at cfg's mount point when the
// daemon process before this one ran cfg's repo and the mount is one an
// artifact-fs daemon made for it (ownsLeftover), because that mount is the
// one it left. It leaves any other mount alone, such as one of a daemon on
// another state root. A mount table it cannot read leaves the claim for the
// next attempt.
func (s *Service) releaseLeftoverMount(cfg model.RepoConfig) error {
	s.mu.Lock()
	leftover := s.leftovers[cfg.ID]
	s.mu.Unlock()
	if !leftover {
		return nil
	}
	entry, held := s.mountPointEntry(cfg.MountPath)
	if held && entry == (mountEntry{}) {
		return nil
	}
	if held && ownsLeftover(runtime.GOOS, entry, cfg.Name) {
		s.logger.Warn("unmounting a mount an earlier daemon left", "repo", cfg.Name, "mount_path", cfg.MountPath)
		if err := s.unmountMountPoint(cfg); err != nil {
			return fmt.Errorf("unmount the mount an earlier daemon left: %w", err)
		}
	}
	s.mu.Lock()
	delete(s.leftovers, cfg.ID)
	s.mu.Unlock()
	return nil
}

// errConnectionEndedMounted is the mount error of a connection that ended
// with no error while the mount point still holds its mount.
var errConnectionEndedMounted = errors.New("fuse connection ended while the mount point still holds the mount")

// joinMount waits on its own goroutine for mfs, which rt just mounted, to
// end. A connection that fails, or ends while its mount point still holds the
// mount, leaves a dead mount, which it starts unmounting at once. A server
// that exits cleanly leaves nothing mounted. The next mount retry mounts rt
// again.
func (s *Service) joinMount(rt *repoRuntime, mfs fusefs.MountedFS) {
	done := make(chan struct{})
	s.mu.Lock()
	rt.joinDone = done
	mountPath := rt.cfg.MountPath
	s.mu.Unlock()
	go func() {
		// Join's error is fatal to the connection, and the dead mount can
		// still hold the mount point. Join also returns nil after an abort,
		// which leaves the dead mount in place. Join must not end with the
		// runtime's context, which ends before the stop's drain.
		connErr := mfs.Join(context.Background())
		if connErr == nil {
			if _, held := s.mountPointEntry(mountPath); held {
				connErr = errConnectionEndedMounted
			}
		}
		// The next registry sync mounts the runtime again.
		nextRetry := time.Now().Add(registrySyncInterval)
		// A retry may mount once a clean exit released mfs, and it clears
		// the serve failure. Holding recordMu until the exit is recorded
		// keeps that clear after it.
		s.recordMu.Lock()
		s.mu.Lock()
		exited := !rt.stopping && rt.mfs == mfs && rt.mount == mountServing
		failed := exited && connErr != nil
		switch {
		case failed:
			rt.mount = mountFailed
		case exited:
			rt.mfs, rt.mount = nil, mountNone
		}
		cfg := rt.cfg
		s.mu.Unlock()
		close(done)
		switch {
		case failed:
			s.logger.Error("fuse connection failed, unmounting", "repo", cfg.Name, "error", auth.RedactLogString(connErr.Error(), cfg.RemoteURL))
			s.recordFailureLocked(cfg, phaseServe, connErr, nextRetry)
		case exited:
			runHook(s.mountReleased)
			s.recordFailureLocked(cfg, phaseServe, errFUSEServerExited, nextRetry)
		}
		s.recordMu.Unlock()
		if failed {
			// The sync loop waits for no unmount it did not start, so an
			// unmount(2) that hangs on the dead mount stalls only this repo.
			s.startUnmount(cfg, s.unmountHeld(cfg, mfs), func(err error) { s.releaseMount(rt, mfs, err, mountNone) })
		}
		if exited {
			s.recordRuntimeState(rt)
		}
		runHook(s.joinFinished)
	}()
}

// unmountOp is an unmount that runs on its own goroutine, because unmount(2)
// can hang on a dead FSKit volume. A repo runs one at a time.
type unmountOp struct {
	done chan struct{}
	err  error
}

// startUnmount runs unmount for cfg's repo on its own goroutine, unless an
// unmount of the repo still runs, and reports whether it started one. A
// caller waits only for an unmount it started. The unmount's context ends
// after unmountTimeout. finished, when set, runs under s.mu with the result
// before waiters see it.
func (s *Service) startUnmount(cfg model.RepoConfig, unmount func(context.Context) error, finished func(error)) (*unmountOp, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op := s.unmounting[cfg.ID]; op != nil {
		return op, false
	}
	op := &unmountOp{done: make(chan struct{})}
	s.unmounting[cfg.ID] = op
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), unmountTimeout)
		err := unmount(ctx)
		cancel()
		s.mu.Lock()
		if s.unmounting[cfg.ID] == op {
			delete(s.unmounting, cfg.ID)
		}
		op.err = err
		if finished != nil {
			finished(err)
		}
		s.mu.Unlock()
		close(op.done)
	}()
	return op, true
}

// waitUnmount waits for op, which the caller started. An unmount that has
// not returned shortly after its context ended hangs, and the wait ends.
func (s *Service) waitUnmount(op *unmountOp) error {
	timer := time.NewTimer(unmountTimeout + unmountReturnSlack)
	defer timer.Stop()
	select {
	case <-op.done:
		return op.err
	case <-timer.C:
		return fmt.Errorf("unmount has not returned after %s", unmountTimeout+unmountReturnSlack)
	}
}

// unmountHeld unmounts mfs. A failed unmount counts only while the mount
// table still lists the mount point, or cannot tell, so a mount that is
// already gone, such as one unmounted from outside, neither blocks the next
// mount nor keeps a stop from finishing.
func (s *Service) unmountHeld(cfg model.RepoConfig, mfs fusefs.MountedFS) func(context.Context) error {
	return func(ctx context.Context) error {
		err := mfs.Unmount(ctx)
		if err == nil {
			return nil
		}
		if _, held := s.mountPointEntry(cfg.MountPath); !held {
			return nil
		}
		return err
	}
}

func (s *Service) startRepoBackground(rt *repoRuntime) {
	s.mu.Lock()
	if rt.active || rt.stopping {
		s.mu.Unlock()
		return
	}
	rt.active = true
	gitDir := rt.cfg.GitDir
	fixedBase := rt.cfg.RequiredCommit != ""
	workerCount := 3
	if fixedBase {
		workerCount = 1
	} else {
		rt.headChanges = make(chan struct{}, 1)
	}
	rt.workers.Add(workerCount)
	s.mu.Unlock()

	go func() {
		defer rt.workers.Done()
		s.refreshLoop(rt)
	}()

	if !fixedBase {
		go func() {
			defer rt.workers.Done()
			s.headReconcileLoop(rt)
		}()

		w := watcher.New(500 * time.Millisecond)
		go func() {
			defer rt.workers.Done()
			w.Watch(rt.ctx, gitDir, func() {
				s.signalHEADReconcile(rt)
			})
		}()
	}
}

// unmount stops id's runtime and forgets it. A failed stop keeps it running.
func (s *Service) unmount(id model.RepoID) error {
	s.mu.Lock()
	rt, ok := s.running[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	if rt.stopRunning {
		s.mu.Unlock()
		return errors.New("runtime is already stopping")
	}
	rt.stopRunning = true
	s.mu.Unlock()
	err := s.stopRuntime(rt)
	s.mu.Lock()
	rt.stopRunning = false
	if err == nil {
		delete(s.running, id)
	}
	s.mu.Unlock()
	return err
}

// stopRuntime unmounts rt, then cancels its context, which cancels its FUSE
// ops in flight, waits for its workers and its server, and closes its stores
// and its git cat-file processes. A failed unmount leaves rt serving. After a
// drain that timed out, rt stays unmounted and the next stop waits again.
func (s *Service) stopRuntime(rt *repoRuntime) error {
	s.mu.Lock()
	cfg, state, mfs, abandoned := rt.cfg, rt.mount, rt.mfs, rt.abandoned
	rt.stopping = true
	s.mu.Unlock()
	keepServing := func(err error) error {
		s.mu.Lock()
		rt.stopping = false
		s.mu.Unlock()
		s.signalHEADReconcile(rt)
		return err
	}
	switch {
	case mfs != nil && (state == mountServing || state == mountFailed):
		op, started := s.startUnmount(cfg, s.unmountHeld(cfg, mfs), func(err error) { s.releaseMount(rt, mfs, err, mountUnmounted) })
		if !started {
			return keepServing(fmt.Errorf("unmount %s: %w", cfg.Name, errUnmountInProgress))
		}
		if err := s.waitUnmount(op); err != nil {
			return keepServing(fmt.Errorf("unmount %s: %w", cfg.Name, err))
		}
	case state == mountAbandoned:
		if err := s.releaseAbandonedMount(cfg, abandoned); err != nil {
			return keepServing(err)
		}
	}
	s.mu.Lock()
	rt.mfs, rt.abandoned, rt.mount = nil, nil, mountUnmounted
	drained := rt.drained
	if drained == nil {
		drained = make(chan struct{})
		rt.drained = drained
		joinDone := rt.joinDone
		go func() {
			rt.workers.Wait()
			if joinDone != nil {
				<-joinDone
			}
			close(drained)
		}()
	}
	s.mu.Unlock()
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.cancelOps != nil {
		rt.cancelOps()
	}
	if rt.gate != nil {
		rt.gate.MarkFailed(context.Canceled)
	}
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		return fmt.Errorf("timed out draining unmounted repo %s", cfg.Name)
	}
	if rt.hydrator != nil {
		rt.hydrator.Stop()
	}
	if rt.sizes != nil {
		rt.sizes.Stop()
	}
	if rt.snapshot != nil {
		_ = rt.snapshot.Close()
	}
	if rt.overlay != nil {
		_ = rt.overlay.Close()
	}
	s.git.CloseBatchPool(cfg.GitDir)
	return nil
}
