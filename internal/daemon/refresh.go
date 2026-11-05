package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// fetchResultFailed is the fetch result of a failed refresh, whose error the
// runtime record holds.
const fetchResultFailed = "failed"

type aheadBehind struct {
	ahead    int
	behind   int
	diverged bool
}

func (s *Service) refreshLoop(rt *repoRuntime) {
	s.mu.Lock()
	baseInterval := rt.cfg.RefreshInterval
	s.mu.Unlock()
	if baseInterval <= 0 {
		baseInterval = 30 * time.Second
	}
	backoff := baseInterval
	ticker := time.NewTicker(backoff)
	defer ticker.Stop()
	for {
		select {
		case <-rt.ctx.Done():
			return
		case interval := <-rt.refresh:
			if interval <= 0 {
				continue
			}
			baseInterval = interval
			backoff = interval
			ticker.Reset(backoff)
		case <-ticker.C:
			s.mu.Lock()
			cfg := rt.cfg
			s.mu.Unlock()
			if cfg.RemoteRefreshDisabled {
				continue
			}
			ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
			err := s.git.Fetch(ctx, cfg)
			if err != nil {
				s.mu.Lock()
				markFetchFailure(&rt.state, fetchResultFailed)
				s.mu.Unlock()
				cancel()
				// Exponential backoff on failure, capped at maxBackoff
				maxBackoff := max(10*time.Minute, baseInterval)
				if backoff >= maxBackoff/2 {
					backoff = maxBackoff
				} else {
					backoff *= 2
				}
				ticker.Reset(backoff)
				s.recordFailure(cfg, phaseFetch, err, time.Now().Add(backoff))
				s.recordRuntimeState(rt, "")
				continue
			}
			state, abErr := s.fetchState(ctx, cfg)
			cancel()
			// Reset backoff on success
			backoff = baseInterval
			ticker.Reset(backoff)
			s.mu.Lock()
			markFetchResult(&rt.state, time.Now(), "ok", rt.mfs != nil)
			if abErr == nil {
				applyAheadBehind(&rt.state, state)
			}
			s.mu.Unlock()
			s.recordRuntimeState(rt, phaseFetch)
		}
	}
}

func (s *Service) refreshCommitTime(ctx context.Context, cfg model.RepoConfig, oid string, resolver *fusefs.Resolver, warnMsg string) {
	if ts, err := s.git.CommitTimestamp(ctx, cfg, oid); err == nil {
		resolver.SetCommitTime(ts)
	} else {
		s.logger.Warn(warnMsg, "repo", cfg.Name, "error", err)
	}
}

func (s *Service) fetchState(ctx context.Context, cfg model.RepoConfig) (aheadBehind, error) {
	ahead, behind, diverged, err := s.git.ComputeAheadBehind(ctx, cfg)
	if err != nil {
		return aheadBehind{}, err
	}
	return aheadBehind{ahead: ahead, behind: behind, diverged: diverged}, nil
}

func ParseRefresh(v string) (time.Duration, error) {
	if strings.EqualFold(strings.TrimSpace(v), "never") {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid refresh interval %q", v)
	}
	if d <= 0 {
		return 0, errors.New("refresh interval must be positive")
	}
	return d, nil
}
