package gitstore

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type Store struct {
	logger         *slog.Logger
	mu             sync.Mutex
	poolMaxSize    int
	pools          map[string]*batchPool // gitDir -> pool
	gitRetryDelays []time.Duration
	closed         bool
}

func New(logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{
		logger:         logger,
		poolMaxSize:    4,
		pools:          map[string]*batchPool{},
		gitRetryDelays: append([]time.Duration(nil), defaultGitRetryDelays...),
	}
}

// Close shuts down all persistent batch processes.
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
	s.mu.Unlock()
	for _, p := range pools {
		p.closeAll()
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

func (s *Store) ComputeAheadBehind(ctx context.Context, repo model.RepoConfig) (ahead int, behind int, diverged bool, err error) {
	branch := branchName(repo.Branch)
	if branch == "" {
		return 0, 0, false, nil
	}
	rangeSpec := fmt.Sprintf("HEAD...origin/%s", branch)
	out, err := runGit(ctx, repo.GitDir, "rev-list", "--left-right", "--count", rangeSpec)
	if err != nil {
		if strings.Contains(err.Error(), "unknown revision") {
			return 0, 0, false, nil
		}
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
