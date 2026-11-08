package gitstore

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

// GitOperation identifies an explicitly retried Git operation.
type GitOperation string

const (
	GitOperationClone GitOperation = "clone"
	GitOperationFetch GitOperation = "fetch"

	gitCommandWaitDelay = 250 * time.Millisecond

	logGitOperationAttemptFailed = "git operation attempt failed"
	logGitOperationRetrying      = "retrying transient git operation failure"
	logGitOperationRecovered     = "git operation recovered"
	logGitOperationInterrupted   = "git operation interrupted during retry backoff"
)

var defaultGitRetryDelays = []time.Duration{250 * time.Millisecond, time.Second}

func (s *Store) retryGitOperationForRemotes(ctx context.Context, operation GitOperation, repoName string, remotes []string, attempt func() error) error {
	return s.retryGitOperationForRemoteLookup(ctx, operation, repoName, func() ([]string, bool) {
		return remotes, true
	}, attempt)
}

func (s *Store) retryGitOperationForRemoteLookup(ctx context.Context, operation GitOperation, repoName string, remotesForLogging func() ([]string, bool), attempt func() error) error {
	delays := s.gitRetryDelays
	var lastErr *GitOperationError
	operationStarted := time.Now()
	safeRepoName := auth.RedactLogString(repoName)
	logger := s.logger.With("operation", operation, "repo", safeRepoName)
	logInterrupted := func(operationErr *GitOperationError, contextErr error) {
		operationErr.contextErr = contextErr
		args := []any{"attempts", operationErr.Attempts, "duration_ms", time.Since(operationStarted).Milliseconds(), "timed_out", errors.Is(contextErr, context.DeadlineExceeded), "canceled", errors.Is(contextErr, context.Canceled), "error", auth.RedactLogString(operationErr.Error())}
		if errors.Is(contextErr, context.Canceled) {
			logger.InfoContext(ctx, logGitOperationInterrupted, args...)
		} else {
			logger.WarnContext(ctx, logGitOperationInterrupted, args...)
		}
	}
	for n := 0; ; n++ {
		if contextErr := ctx.Err(); contextErr != nil {
			if lastErr != nil {
				logInterrupted(lastErr, contextErr)
				return lastErr
			}
			return &GitOperationError{Operation: operation, Attempts: n, contextErr: contextErr}
		}

		attemptStarted := time.Now()
		err := attempt()
		if err == nil {
			if n > 0 {
				logger.InfoContext(ctx, logGitOperationRecovered, "attempts", n+1, "duration_ms", time.Since(operationStarted).Milliseconds())
			}
			return nil
		}
		cause, contextErr, retryable := gitOperationCauses(err)
		if cause != nil {
			remotes, complete := remotesForLogging()
			message := "git command failed"
			if complete {
				message = auth.RedactLogString(cause.Error(), remotes...)
			}
			cause = &redactedCause{message: message, cause: cause}
		}
		// Read ctx after remotesForLogging, whose wait can end because ctx did.
		if contextErr == nil {
			contextErr = ctx.Err()
		}
		operationErr := &GitOperationError{
			Operation:  operation,
			Attempts:   n + 1,
			Retryable:  retryable,
			cause:      cause,
			contextErr: contextErr,
		}
		lastErr = operationErr
		safeError := ""
		if cause != nil {
			safeError = auth.RedactLogString(cause.Error())
		} else if contextErr != nil {
			safeError = auth.RedactLogString(contextErr.Error())
		} else {
			safeError = auth.RedactLogString(operationErr.Error())
		}
		logger.WarnContext(ctx, logGitOperationAttemptFailed,
			"attempt", n+1,
			"max_attempts", len(delays)+1,
			"retryable", retryable,
			"duration_ms", time.Since(attemptStarted).Milliseconds(),
			"timed_out", errors.Is(contextErr, context.DeadlineExceeded),
			"canceled", errors.Is(contextErr, context.Canceled),
			"error", safeError,
		)
		if contextErr != nil {
			return operationErr
		}

		if !retryable || n == len(delays) {
			return operationErr
		}
		delay := equalJitter(delays[n])
		logger.InfoContext(ctx, logGitOperationRetrying, "attempt", n+1, "next_attempt", n+2, "backoff_ms", delay.Milliseconds())
		if waitErr := waitForGitRetry(ctx, delay); waitErr != nil {
			logInterrupted(operationErr, waitErr)
			return operationErr
		}
	}
}

func gitOperationCauses(err error) (cause error, contextErr error, retryable bool) {
	if commandErr, ok := errors.AsType[*gitCommandError](err); ok {
		return commandErr.cause, commandErr.contextErr, commandErr.retryable
	}
	return err, nil, isTransientGitError(err)
}

func equalJitter(maximum time.Duration) time.Duration {
	if maximum <= 1 {
		return maximum
	}
	half := maximum / 2
	return half + time.Duration(rand.Int64N(int64(maximum-half)))
}

func waitForGitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTransientGitError(err error) bool {
	if err == nil {
		return false
	}
	return isTransientGitMessage(err.Error())
}

var permanentGitFailureSymptoms = []string{
	"no space left on device",
	"disk quota exceeded",
	"permission denied",
	"read-only file system",
}

var transientGitFailureSymptoms = []string{
	"http 408", "http/1.0 408", "http/1.1 408", "http/2 408", "returned error: 408",
	"http 500", "http/1.0 500", "http/1.1 500", "http/2 500", "returned error: 500",
	"http 502", "http/1.0 502", "http/1.1 502", "http/2 502", "returned error: 502",
	"http 503", "http/1.0 503", "http/1.1 503", "http/2 503", "returned error: 503",
	"http 504", "http/1.0 504", "http/1.1 504", "http/2 504", "returned error: 504",
	"http 522", "http/1.0 522", "http/1.1 522", "http/2 522", "returned error: 522",
	"http 524", "http/1.0 524", "http/1.1 524", "http/2 524", "returned error: 524",
	"curl 18 ",
	"curl 55 ",
	"curl 56 ",
	"curl 92 ",
	"unexpected disconnect",
	"remote end hung up unexpectedly",
	"remote hung up",
	"connection reset",
	"connection timed out",
	"could not resolve host",
	"couldn't connect to server",
	"failed to connect to",
	"operation timed out",
	"timeout was reached",
	"empty reply from server",
	"transfer closed with",
	"send failure: broken pipe",
	"tls connection was non-properly terminated",
	"was not closed cleanly: cancel",
	"reset by server",
	"bytes of body are still expected",
	"bytes of length header were received",
	"early eof",
}

func containsGitSymptom(message string, symptoms []string) bool {
	for _, symptom := range symptoms {
		if strings.Contains(message, symptom) {
			return true
		}
	}
	return false
}

func isTransientGitMessage(message string) bool {
	message = strings.ToLower(message)
	// Generic index-pack summaries can follow local failures that retrying cannot repair.
	return !containsGitSymptom(message, permanentGitFailureSymptoms) && containsGitSymptom(message, transientGitFailureSymptoms)
}
