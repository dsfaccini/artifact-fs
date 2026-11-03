package gitstore

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

// GitOperationError describes a failed Git operation without exposing
// credentials. Retryable reports whether its command failure matched a known
// transient smart-Git transport symptom.
type GitOperationError struct {
	Operation GitOperation
	Attempts  int
	Retryable bool

	cause      error
	contextErr error
}

func (e *GitOperationError) Error() string {
	if e == nil {
		return "git operation failed"
	}
	operation := string(e.Operation)
	if operation == "" {
		operation = "operation"
	}
	var msg string
	if e.Attempts == 0 {
		msg = fmt.Sprintf("git %s failed before first attempt", operation)
	} else {
		noun := "attempts"
		if e.Attempts == 1 {
			noun = "attempt"
		}
		msg = fmt.Sprintf("git %s failed after %d %s", operation, e.Attempts, noun)
	}
	if e.cause != nil {
		msg += ": " + auth.RedactString(e.cause.Error())
	}
	if e.contextErr != nil {
		msg += "; caller context: " + e.contextErr.Error()
	}
	return msg
}

func (e *GitOperationError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := make([]error, 0, 2)
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if e.contextErr != nil {
		errs = append(errs, e.contextErr)
	}
	return errs
}

// gitCommandError preserves the command failure and caller cancellation as
// distinct process-boundary errors. cause has already been redacted.
type gitCommandError struct {
	cause      error
	contextErr error
	retryable  bool // Classified before cause is redacted.
	exitCode   int
}

type redactedCause struct {
	message string
	cause   error
}

func (e *redactedCause) Error() string { return e.message }

func (e *redactedCause) Unwrap() error { return e.cause }

func (e *gitCommandError) Error() string {
	if e == nil {
		return "git command failed"
	}
	if e.cause == nil {
		if e.contextErr != nil {
			return "caller context: " + e.contextErr.Error()
		}
		return "git command failed"
	}
	msg := e.cause.Error()
	if e.contextErr != nil {
		msg += "; caller context: " + e.contextErr.Error()
	}
	return msg
}

func (e *gitCommandError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := make([]error, 0, 2)
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if e.contextErr != nil {
		errs = append(errs, e.contextErr)
	}
	return errs
}

const (
	maxGitErrorBytes       = 16 << 10
	gitErrorScanOverlap    = 128
	gitErrorTruncatedLabel = "[git stderr truncated]"
)

type boundedGitError struct {
	buf          bytes.Buffer
	limit        int
	truncated    bool
	scanTail     string
	sawPermanent bool
	sawTransient bool
}

func (b *boundedGitError) Write(p []byte) (int, error) {
	n := len(p)
	scan := strings.ToLower(b.scanTail + string(p))
	b.sawPermanent = b.sawPermanent || containsGitSymptom(scan, permanentGitFailureSymptoms)
	b.sawTransient = b.sawTransient || containsGitSymptom(scan, transientGitFailureSymptoms)
	if len(scan) > gitErrorScanOverlap {
		b.scanTail = scan[len(scan)-gitErrorScanOverlap:]
	} else {
		b.scanTail = scan
	}
	remaining := max(b.limit-b.buf.Len(), 0)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	if len(p) > 0 {
		_, _ = b.buf.Write(p)
	}
	return n, nil
}

func (b *boundedGitError) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return strings.TrimRight(b.buf.String(), "\r\n") + "\n" + gitErrorTruncatedLabel
}

func (b *boundedGitError) Retryable() bool {
	return !b.sawPermanent && b.sawTransient
}

func redactTruncatedCredentialPrefix(message string, credentials []string) string {
	suffix := "\n" + gitErrorTruncatedLabel
	body, truncated := strings.CutSuffix(message, suffix)
	if !truncated {
		return message
	}
	for _, credential := range credentials {
		for prefixLen := len(credential) - 1; prefixLen > 0; prefixLen-- {
			if strings.HasSuffix(body, credential[:prefixLen]) {
				body = body[:len(body)-prefixLen] + "REDACTED"
				break
			}
		}
	}
	return body + suffix
}
