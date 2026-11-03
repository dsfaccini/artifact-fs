package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func (s *Service) withRepoConfigLock(ctx context.Context, repoName string, fn func() error) error {
	return s.withRepoLock(ctx, repoName+".lock", fn)
}

func (s *Service) withRepoPrepareLock(ctx context.Context, repoName string, fn func() error) error {
	return s.withRepoLock(ctx, repoName+".prepare.lock", fn)
}

func (s *Service) withRepoLock(ctx context.Context, lockName string, fn func() error) error {
	lockDir := filepath.Join(s.root, "config", "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(filepath.Join(lockDir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	for {
		err = unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		timer := time.NewTimer(repoConfigLockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	return fn()
}
