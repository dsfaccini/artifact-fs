package daemon

import (
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// retrier spaces the attempts of a step that keeps failing: the first retry
// waits min, each later failure doubles the wait up to max, and a success
// resets it. The daemon records each failure of phase with its next attempt
// in repo_runtime, so status shows the retry. The daemon backs off its FUSE
// mounts, its HEAD reconciles and its remote refreshes this way; any other
// failure retries at the next registry sync.
type retrier struct {
	phase    string
	min, max time.Duration
	delay    time.Duration
	next     time.Time
}

func newRetrier(phase string, minDelay, maxDelay time.Duration) *retrier {
	return &retrier{phase: phase, min: minDelay, max: max(minDelay, maxDelay)}
}

// fail records a failure at now and returns when the step runs again.
func (r *retrier) fail(now time.Time) time.Time {
	r.delay = min(max(2*r.delay, r.min), r.max)
	r.next = now.Add(r.delay)
	return r.next
}

func (r *retrier) succeed() { r.delay, r.next = 0, time.Time{} }

// due reports whether the step may run at now.
func (r *retrier) due(now time.Time) bool { return !now.Before(r.next) }

// retryFailure records err, redacted, as a failure of r's phase for cfg, with
// updates in the same write, and returns how long until r runs the step
// again.
func (s *Service) retryFailure(cfg model.RepoConfig, r *retrier, err error, updates ...func(*model.RepoRuntimeRecord)) time.Duration {
	now := time.Now()
	next := r.fail(now)
	s.recordFailure(cfg, r.phase, err, next, updates...)
	return next.Sub(now)
}

const (
	mountRetryMin = 30 * time.Second
	mountRetryMax = 5 * time.Minute
	// fetchRetryMax caps the refresh backoff. A refresh interval longer than
	// fetchRetryMax is the cap instead.
	fetchRetryMax = 10 * time.Minute
)

// mountRetry backs off the mounts of one repo. request is the mount request
// the failed attempt carried out: a new request mounts at once.
type mountRetry struct {
	*retrier
	request string
}
