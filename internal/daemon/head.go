package daemon

import (
	"cmp"
	"context"
	"time"

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
	prev := rt.resolver.Generation()
	if err := rt.resolver.SwitchGeneration(ctx, gen, commitTime); err != nil {
		return err
	}
	if prev != 0 && prev != gen {
		rt.retiredMu.Lock()
		rt.retired = append(rt.retired, retiredGeneration{gen: prev, at: time.Now()})
		rt.retiredMu.Unlock()
	}
	s.mu.Lock()
	mfs := rt.mfs
	serving := rt.mount == mountServing
	s.mu.Unlock()
	if serving {
		if err := mfs.InvalidateChanged(); err != nil {
			s.logger.Warn("kernel cache invalidation failed", "repo", rt.cfg.Name, "error", err)
		}
	}
	return nil
}

// pruneGenerations prunes snap's generations below keepFrom, except those
// that rt, when not nil, retired within the retention window. An inode whose
// attributes the kernel received before a switch keeps reading the generation
// they came from until it asks again.
func (s *core) pruneGenerations(ctx context.Context, rt *repoRuntime, snap *snapshot.Store, keepFrom int64) error {
	if rt != nil {
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
	return snap.PruneGenerations(ctx, keepFrom)
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
				break
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
