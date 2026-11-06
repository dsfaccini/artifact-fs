package daemon

import (
	"cmp"
	"context"
	"time"
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
	prevOID := rt.state.CurrentHEADOID
	prevRef := rt.state.CurrentHEADRef
	prevGen := rt.state.SnapshotGeneration
	s.mu.Unlock()
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
			rt.state.CurrentHEADRef = ref
		}
		s.mu.Unlock()
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
	if err := rt.resolver.SwitchGeneration(ctx, gen, commitTime); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("overlay reconcile failed", "repo", cfg.Name, "error", err)
		return err
	}

	s.mu.Lock()
	if s.running[cfg.ID] == rt {
		setHeadState(&rt.state, oid, ref, gen)
	}
	s.mu.Unlock()
	if err := rt.snapshot.PruneGenerations(ctx, min(prevGen, gen-1)); err != nil {
		s.logger.Warn("snapshot generation cleanup failed", "repo", cfg.Name, "error", err)
	}
	return nil
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
	minBackoff := cmp.Or(s.headReconcileBackoff, headReconcileMinBackoff)
	backoff := minBackoff
	for {
		select {
		case <-rt.ctx.Done():
			return
		case <-rt.headChanges:
		}

		for {
			if err := s.reconcileHEAD(rt.ctx, rt); err == nil {
				backoff = minBackoff
				break
			}
			if rt.ctx.Err() != nil {
				return
			}

			timer := time.NewTimer(backoff)
			select {
			case <-rt.ctx.Done():
				timer.Stop()
				return
			case <-rt.headChanges:
				timer.Stop()
			case <-timer.C:
			}
			backoff = min(backoff*2, headReconcileMaxBackoff)
		}
	}
}
