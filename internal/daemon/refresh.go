package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type aheadBehind struct {
	ahead    int
	behind   int
	diverged bool
}

// newRefreshRetrier backs off the remote refreshes of a repo refreshed every
// interval: twice the interval after the first failure, doubling up to
// fetchRetryMax, or up to the interval when it is longer.
func newRefreshRetrier(interval time.Duration) *retrier {
	limit := max(fetchRetryMax, interval)
	return newRetrier(phaseFetch, min(2*interval, limit), limit)
}

func (s *Service) refreshLoop(rt *repoRuntime) {
	s.mu.Lock()
	baseInterval := rt.cfg.RefreshInterval
	s.mu.Unlock()
	if baseInterval <= 0 {
		baseInterval = 30 * time.Second
	}
	retry := newRefreshRetrier(baseInterval)
	ticker := time.NewTicker(baseInterval)
	defer ticker.Stop()
	ticks := ticker.C
	if s.refreshTicks != nil {
		ticks = s.refreshTicks
	}
	for {
		select {
		case <-rt.ctx.Done():
			return
		case interval := <-rt.refresh:
			if interval <= 0 {
				continue
			}
			baseInterval = interval
			retry = newRefreshRetrier(baseInterval)
			ticker.Reset(baseInterval)
		case <-ticks:
			s.mu.Lock()
			cfg := rt.cfg
			s.mu.Unlock()
			if cfg.RemoteRefreshDisabled {
				continue
			}
			// A preparation fetches into the same tracking ref, and two
			// fetches that overlap fail each other's ref update. So the
			// refresh waits for the repo's prepare lock, and its timeout
			// starts once it holds the lock.
			var state aheadBehind
			var abErr error
			err := s.withRepoPrepareLock(rt.ctx, cfg.Name, func() error {
				ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
				defer cancel()
				if err := s.git.FetchNonInteractive(ctx, cfg); err != nil {
					return err
				}
				state, abErr = s.fetchState(ctx, cfg)
				return nil
			})
			if err != nil {
				// A stop ends rt.ctx, also while the refresh waits for the
				// lock, and that is no fetch failure to record.
				if rt.ctx.Err() != nil {
					return
				}
				s.mu.Lock()
				rt.fetch.state = fetchFailed
				s.mu.Unlock()
				ticker.Reset(s.retryFailure(cfg, retry, err, s.runtimeState(rt)))
				continue
			}
			retry.succeed()
			ticker.Reset(baseInterval)
			s.mu.Lock()
			rt.fetch.state, rt.fetch.at = fetchOK, time.Now()
			if abErr == nil {
				rt.fetch.ahead, rt.fetch.behind, rt.fetch.diverged = state.ahead, state.behind, state.diverged
			}
			s.mu.Unlock()
			s.recordRuntimeState(rt, phaseFetch)
		}
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
