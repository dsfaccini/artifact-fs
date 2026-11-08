package gitstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

var _ model.GitStore = (*Store)(nil)

type Store struct {
	logger         *slog.Logger
	mu             sync.Mutex
	poolMaxSize    int
	pools          map[string]*batchPool // gitDir -> pool
	gitRetryDelays []time.Duration
	// rollbackContext makes the context of one rollback. Tests replace it.
	rollbackContext func(context.Context) (context.Context, context.CancelFunc)
	// sshWrapperDir holds the wrapper batchModeSSH writes; Close removes it.
	sshWrapperDir string
	closed        bool
	// testHookSSHWrapperDirMade, when set, runs in batchModeSSH between
	// making a wrapper's directory and writing the wrapper.
	testHookSSHWrapperDirMade func(dir string)
	// testHookBeforeHookRename, when set, runs in replaceExecutable between
	// writing the new file and renaming it over path.
	testHookBeforeHookRename func(path string)
}

func New(logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{
		logger:          logger,
		poolMaxSize:     4,
		pools:           map[string]*batchPool{},
		gitRetryDelays:  append([]time.Duration(nil), defaultGitRetryDelays...),
		rollbackContext: defaultRollbackContext,
	}
}

// Close shuts down all persistent batch processes and removes the SSH
// wrapper.
func (s *Store) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pools := make([]*batchPool, 0, len(s.pools))
	for dir, p := range s.pools {
		pools = append(pools, p)
		delete(s.pools, dir)
	}
	sshWrapperDir := s.sshWrapperDir
	s.sshWrapperDir = ""
	s.mu.Unlock()
	for _, p := range pools {
		p.closeAll()
	}
	if sshWrapperDir != "" {
		_ = os.RemoveAll(sshWrapperDir)
	}
}

func (s *Store) SetBatchPoolSize(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.poolMaxSize = n
	for _, p := range s.pools {
		p.setMaxSize(n)
	}
}

func (s *Store) ResolveHEAD(ctx context.Context, repo model.RepoConfig) (oid string, ref string, err error) {
	oid, err = runGit(ctx, repo.GitDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	ref, err = runGit(ctx, repo.GitDir, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		ref = "DETACHED"
		err = nil
	}
	return strings.TrimSpace(oid), strings.TrimSpace(ref), nil
}

// CommitTimestamp returns the committer timestamp of the given commit OID.
func (s *Store) CommitTimestamp(ctx context.Context, repo model.RepoConfig, oid string) (int64, error) {
	out, err := runGit(ctx, repo.GitDir, "show", "-s", "--format=%ct", oid)
	if err != nil {
		return 0, err
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse commit timestamp %q: %w", out, err)
	}
	return ts, nil
}

// ComputeAheadBehind compares HEAD with the tracking ref the refresh fetches
// into. It reports 0/0 while that ref is missing.
func (s *Store) ComputeAheadBehind(ctx context.Context, repo model.RepoConfig) (ahead int, behind int, diverged bool, err error) {
	_, tracking, err := refreshTarget(repo)
	if err != nil {
		return 0, 0, false, err
	}
	if _, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", "--quiet", tracking+"^{commit}"); err != nil {
		if exitErr, ok := errors.AsType[*gitCommandError](err); ok && exitErr.exitCode == 1 {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	// With --depth, every refresh of a verified source passes --depth. That
	// cuts a tip that moved by --depth commits or more, or was rewritten, off
	// from HEAD. Without a common ancestor, its counts would read as a
	// divergence, so it reports 0/0.
	if repo.RequiredCommit != "" && repo.HistoryDepth > 0 {
		if _, err := runGit(ctx, repo.GitDir, "merge-base", "HEAD", tracking); err != nil {
			if exitErr, ok := errors.AsType[*gitCommandError](err); ok && exitErr.exitCode == 1 {
				return 0, 0, false, nil
			}
			return 0, 0, false, err
		}
	}
	out, err := runGit(ctx, repo.GitDir, "rev-list", "--left-right", "--count", "HEAD..."+tracking)
	if err != nil {
		return 0, 0, false, err
	}
	parts := strings.Fields(out)
	if len(parts) < 2 {
		return 0, 0, false, nil
	}
	ahead, _ = strconv.Atoi(parts[0])
	behind, _ = strconv.Atoi(parts[1])
	diverged = ahead > 0 && behind > 0
	return ahead, behind, diverged, nil
}
