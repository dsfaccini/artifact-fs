package daemon

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func (s *Service) reconcileHEAD(ctx context.Context, rt *repoRuntime) error {
	rt.headMu.Lock()
	defer rt.headMu.Unlock()
	s.mu.Lock()
	if s.running[rt.cfg.ID] != rt || rt.stopping {
		s.mu.Unlock()
		return nil
	}
	cfg := rt.cfg
	s.mu.Unlock()
	if cfg.RequiredCommit != "" {
		return nil
	}
	oid, ref, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Error("HEAD resolve failed", "repo", cfg.Name, "error", err)
		return err
	}
	s.logger.Debug("HEAD resolved", "repo", cfg.Name, "oid", oid, "ref", ref)
	s.mu.Lock()
	prev := rt.head
	s.mu.Unlock()
	prevOID, prevRef, prevGen := prev.oid, prev.ref, prev.gen
	storedOID, storedRef, gen, stateErr := rt.snapshot.ReadState(ctx)
	// A failed overlay transition can leave the published snapshot ahead of the active generation.
	if oid == prevOID && stateErr == nil && storedOID == oid && gen == prevGen {
		if ref == prevRef && storedRef == ref {
			return nil
		}
		if err := rt.snapshot.UpdateHEADRef(ctx, ref); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Warn("snapshot head_ref update failed", "repo", cfg.Name, "error", err)
			return err
		}
		s.mu.Lock()
		if s.running[cfg.ID] == rt && !rt.stopping {
			rt.head.ref = ref
		}
		s.mu.Unlock()
		s.recordRuntimeState(rt, phaseHEADReconcile)
		return nil
	}
	if stateErr != nil || storedOID != oid || storedRef != ref || gen == 0 {
		var phase string
		gen, phase, err = s.publishSnapshot(ctx, cfg, rt.snapshot, oid, ref)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			msg := "tree rebuild failed"
			if phase == snapshotPhasePublish {
				msg = "snapshot publish failed"
			}
			s.logger.Error(msg, "repo", cfg.Name, "error", err)
			return err
		}
	}
	commitTime := int64(0)
	if ts, timestampErr := s.git.CommitTimestamp(ctx, cfg, oid); timestampErr == nil {
		commitTime = ts
	} else if ctx.Err() == nil {
		s.logger.Warn("commit timestamp unavailable", "repo", cfg.Name, "error", timestampErr)
	}
	if err := s.switchGeneration(ctx, rt, gen, commitTime); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("overlay reconcile failed", "repo", cfg.Name, "error", err)
		return err
	}

	s.mu.Lock()
	if s.running[cfg.ID] == rt {
		rt.head = servedHead{oid: oid, ref: ref, gen: gen}
	}
	s.mu.Unlock()
	s.recordRuntimeState(rt, phaseHEADReconcile)
	if err := s.pruneGenerations(ctx, rt, rt.snapshot, min(prevGen, gen-1)); err != nil {
		s.logger.Warn("snapshot generation cleanup failed", "repo", cfg.Name, "error", err)
	}
	return nil
}

// switchGeneration switches rt's resolver to gen, then tells the kernel to
// drop what it caches that the switch changed. It records when the previous
// generation retired.
func (s *Service) switchGeneration(ctx context.Context, rt *repoRuntime, gen, commitTime int64) error {
	sw, err := rt.resolver.SwitchGeneration(ctx, gen, commitTime)
	if err != nil {
		return err
	}
	if sw.Replaced != 0 && sw.Replaced != gen {
		rt.retiredMu.Lock()
		rt.retired = append(rt.retired, retiredGeneration{gen: sw.Replaced, at: time.Now()})
		rt.retiredMu.Unlock()
	}
	s.mu.Lock()
	mfs := rt.mfs
	serving := rt.mount == mountServing
	name := rt.cfg.Name
	s.mu.Unlock()
	if sw.DiffErr != nil {
		s.logger.Warn("generation diff failed; Git rescans the work tree", "repo", name, "error", sw.DiffErr)
	}
	if serving {
		if err := mfs.InvalidateChanged(); err != nil {
			s.logger.Warn("kernel cache invalidation failed", "repo", name, "error", err)
		}
	}
	if err := rt.overlay.LogSwitched(ctx, sw.Changed); err != nil {
		s.logger.Warn("change log after the kernel invalidations failed", "repo", name, "error", err)
	}
	// FSKit sends the kernel no invalidations, so the kernel can answer with
	// attributes it cached from the old generation until they expire.
	if serving && mfs.Backend() == fusefs.BackendFSKit {
		s.logSwitchedAfterCacheTTL(rt, name, sw.Changed)
	}
	return nil
}

// logSwitchedAfterCacheTTL logs switched once more after fusefs.CacheTTL,
// when every attribute the kernel cached before the switch has expired. A
// stop of rt cancels it.
func (s *Service) logSwitchedAfterCacheTTL(rt *repoRuntime, name string, switched model.SwitchedPaths) {
	if len(switched.Paths) == 0 && !switched.Unknown {
		return
	}
	rt.workers.Go(func() {
		expired := time.NewTimer(fusefs.CacheTTL)
		defer expired.Stop()
		select {
		case <-rt.ctx.Done():
			return
		case <-expired.C:
		}
		if err := rt.overlay.LogSwitched(rt.ctx, switched); err != nil {
			s.logger.Warn("change log after the kernel cache expired failed", "repo", name, "error", err)
		}
	})
}

// pruneGenerations prunes snap's generations below keepFrom, except those
// that rt, when not nil, retired within the retention window, and those its
// serving mount's open files read. An inode whose attributes the kernel received
// before a switch keeps reading the generation they came from until it asks
// again. It then records the generations it kept for open files. The caller
// holds rt.headMu when rt is not nil, so prunes record them one at a time.
func (s *Service) pruneGenerations(ctx context.Context, rt *repoRuntime, snap *snapshot.Store, keepFrom int64) error {
	var open []int64
	var mfs fusefs.MountedFS
	if rt != nil {
		s.mu.Lock()
		serving := rt.mount == mountServing
		if serving {
			mfs = rt.mfs
		}
		s.mu.Unlock()
		if serving {
			open = mfs.OpenFileGenerations()
		}
		retention := cmp.Or(s.generationRetention, defaultGenerationRetention)
		now := time.Now()
		rt.retiredMu.Lock()
		retired := rt.retired[:0]
		for _, r := range rt.retired {
			if now.Sub(r.at) < retention {
				retired = append(retired, r)
				keepFrom = min(keepFrom, r.gen)
			}
		}
		rt.retired = retired
		rt.retiredMu.Unlock()
	}
	if err := snap.PruneGenerations(ctx, keepFrom, open...); err != nil || rt == nil {
		return err
	}
	var kept []int64
	for _, gen := range open {
		if gen < keepFrom {
			kept = append(kept, gen)
		}
	}
	s.recordKeptGenerations(ctx, rt, snap, mfs, kept)
	return nil
}

// recordKeptGenerations records kept, the generations a prune of rt kept
// only because open files of mfs read them. While they number at least the
// repo's WarnKeptGenerations, it records a warning, and it logs the warning
// once as their count reaches that threshold. It logs again only after their
// count fell below it.
func (s *Service) recordKeptGenerations(ctx context.Context, rt *repoRuntime, snap *snapshot.Store, mfs fusefs.MountedFS, kept []int64) {
	s.mu.Lock()
	cfg := rt.cfg
	s.mu.Unlock()
	var warning string
	var attrs []any
	if len(kept) > 0 && len(kept) >= cfg.WarnKeptGenerations {
		warning, attrs = keptGenerationsWarning(ctx, cfg, snap, mfs, kept)
	}
	s.mu.Lock()
	crossed := warning != "" && rt.keptWarning == ""
	changed := rt.kept != len(kept) || rt.keptWarning != warning
	rt.kept, rt.keptWarning = len(kept), warning
	s.mu.Unlock()
	if crossed {
		s.logger.Warn(logKeptGenerations, attrs...)
	}
	if changed {
		s.recordRuntimeState(rt)
	}
}

// keptGenerationsWarning describes kept, as status shows it and as log
// attributes. It lists no caller IDs on FSKit.
func keptGenerationsWarning(ctx context.Context, cfg model.RepoConfig, snap *snapshot.Store, mfs fusefs.MountedFS, kept []int64) (string, []any) {
	raise := fmt.Sprintf("artifact-fs set-warn-kept-generations --name %s --count <n>", cfg.Name)
	attrs := []any{"repo", cfg.Name, "kept_generations", len(kept), "warn_kept_generations", cfg.WarnKeptGenerations}
	size := "of unknown size"
	if bytes, err := snap.EstimateGenerationBytes(ctx, kept); err == nil {
		size = fmt.Sprintf("about %.1f MB", float64(bytes)/1e6)
		attrs = append(attrs, "estimated_bytes", bytes)
	} else {
		attrs = append(attrs, "estimate_error", err.Error())
	}
	pids := "unavailable on FSKit"
	if mfs.Backend() != fusefs.BackendFSKit {
		pids = formatOpenFilePIDs(mfs.OpenFilePIDs(kept), func(id uint32) (uint32, bool) {
			return openerProcess(runtime.GOOS, os.ReadFile, id)
		})
	}
	attrs = append(attrs, "pids", pids,
		"frees", "at the first HEAD change after these files close",
		"raise_threshold", raise)
	return fmt.Sprintf("open files keep %d older snapshot generations, %s, reaching the warning threshold %d; pids holding the files: %s; the generations free at the first HEAD change after the files close; raise the threshold with %s",
		len(kept), size, cfg.WarnKeptGenerations, pids, raise), attrs
}

// opener is the process that opened files, or the thread when its process
// is unknown.
type opener struct {
	id     uint32
	thread bool
}

// formatOpenFilePIDs lists each process that opened the files pids counts by
// caller ID, with its open files, in ID order. process resolves a caller ID
// to its process, and reports false when the ID stays a thread ID. An ID of
// 0, which Linux sends for a thread outside the daemon's PID namespace, is
// unknown.
func formatOpenFilePIDs(pids map[uint32]int, process func(uint32) (uint32, bool)) string {
	files := make(map[opener]int)
	for id, n := range pids {
		o := opener{id: id}
		if id != 0 {
			pid, ok := process(id)
			o = opener{id: pid, thread: !ok}
		}
		files[o] += n
	}
	var out []string
	for _, o := range slices.SortedFunc(maps.Keys(files), func(a, b opener) int { return cmp.Compare(a.id, b.id) }) {
		id := strconv.FormatUint(uint64(o.id), 10)
		switch {
		case o.id == 0:
			id = "unknown"
		case o.thread:
			id = "thread " + id
		}
		noun := "open files"
		if files[o] == 1 {
			noun = "open file"
		}
		out = append(out, fmt.Sprintf("%s (%d %s)", id, files[o], noun))
	}
	if len(out) == 0 {
		return "none open"
	}
	return strings.Join(out, ", ")
}

// openerProcess returns the process that id, the caller ID of a FUSE op on
// goos, names. Linux sends the calling thread's ID, so openerProcess reads
// its thread group from /proc/<id>/status with readFile, and reports false
// when it cannot: id then stays a thread ID. The macFUSE kernel extension
// sends the process ID.
func openerProcess(goos string, readFile func(string) ([]byte, error), id uint32) (uint32, bool) {
	if goos != "linux" {
		return id, true
	}
	status, err := readFile(fmt.Sprintf("/proc/%d/status", id))
	if err != nil {
		return id, false
	}
	for line := range strings.Lines(string(status)) {
		if value, ok := strings.CutPrefix(line, "Tgid:"); ok {
			if tgid, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32); err == nil {
				return uint32(tgid), true
			}
		}
	}
	return id, false
}

func (s *Service) signalHEADReconcile(rt *repoRuntime) {
	s.mu.Lock()
	if rt.stopping || s.running[rt.cfg.ID] != rt {
		s.mu.Unlock()
		return
	}
	headChanges := rt.headChanges
	s.mu.Unlock()
	if headChanges == nil {
		return
	}
	select {
	case headChanges <- struct{}{}:
	default:
	}
}

// servesHEAD rebases rt's watcher on HEAD as it is now, then reports whether
// rt serves HEAD, or no longer follows it. A reconcile reads HEAD after the
// poll that signaled it, so HEAD can move to a state no poll saw, which the
// reconcile serves, and back to the polled state, which no poll signals. The
// rebase follows the reconcile's read: one before it leaves that hole.
func (s *Service) servesHEAD(rt *repoRuntime) bool {
	s.mu.Lock()
	cfg, served, w := rt.cfg, rt.head, rt.watcher
	following := s.running[cfg.ID] == rt && !rt.stopping && cfg.RequiredCommit == ""
	s.mu.Unlock()
	if !following {
		return true
	}
	w.Rebase(cfg.GitDir)
	oid, ref, err := s.git.ResolveHEAD(rt.ctx, cfg)
	return err == nil && oid == served.oid && ref == served.ref
}

func (s *Service) headReconcileLoop(rt *repoRuntime) {
	retry := newRetrier(phaseHEADReconcile, cmp.Or(s.headReconcileBackoff, headReconcileMinBackoff), headReconcileMaxBackoff)
	for {
		select {
		case <-rt.ctx.Done():
			return
		case <-rt.headChanges:
		}

		for {
			err := s.reconcileHEAD(rt.ctx, rt)
			if err == nil {
				retry.succeed()
				if s.servesHEAD(rt) {
					break
				}
				continue
			}
			if rt.ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			cfg := rt.cfg
			s.mu.Unlock()
			// A HEAD change can also start the retry early.
			timer := time.NewTimer(s.retryFailure(cfg, retry, err))
			select {
			case <-rt.ctx.Done():
				timer.Stop()
				return
			case <-rt.headChanges:
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}
