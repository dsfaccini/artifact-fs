package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func (s *core) withRepoConfigLock(ctx context.Context, repoName string, fn func() error) error {
	return s.withRepoLock(ctx, repoName+".lock", fn)
}

func (s *core) withRepoPrepareLock(ctx context.Context, repoName string, fn func() error) error {
	return s.withRepoLock(ctx, repoName+".prepare.lock", fn)
}

// tryRepoPrepareLock runs fn under repoName's prepare lock when no other
// holder has the lock, and reports whether fn ran.
func (s *core) tryRepoPrepareLock(repoName string, fn func() error) (bool, error) {
	lockFile, err := s.openRepoLock(repoName + ".prepare.lock")
	if err != nil {
		return false, err
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return false, nil
		}
		return false, err
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	return true, fn()
}

func (s *core) openRepoLock(lockName string) (*os.File, error) {
	lockDir := filepath.Join(s.root, "config", "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(lockDir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
}

func (s *core) withRepoLock(ctx context.Context, lockName string, fn func() error) error {
	lockFile, err := s.openRepoLock(lockName)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	for first := true; ; first = false {
		err = unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		if first && s.lockBusy != nil {
			s.lockBusy(lockName)
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
