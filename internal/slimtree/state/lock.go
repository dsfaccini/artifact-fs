package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// LockWait bounds Lock's wait for the global lock.
const LockWait = 30 * time.Second

// lockWait is LockWait. Tests lower it.
var lockWait = LockWait

// ErrLockBusy is the error of a lock another command holds.
var ErrLockBusy = errors.New("slimtree's lock is busy")

// lockWaiting, when set, runs once Lock waits on the flock. Tests use it to
// end the wait.
var lockWaiting func()

// Lock is the home's global lock, held while a command changes a worktree
// or its record.
type Lock struct {
	f *os.File
}

// holder is what a lock's holder writes into the lock file, for the message
// of a command that gives up waiting.
type holder struct {
	PID   int       `json:"pid"`
	Argv  []string  `json:"argv"`
	Since time.Time `json:"since"`
	// Path is the worktree the holder works on.
	Path string `json:"path,omitempty"`
}

// Lock takes the global lock for a command that works on path, which may be
// "". It waits until ctx ends or LockWait passes, then returns an error that
// wraps ErrLockBusy and names the holder. It waits in flock, as an unbounded
// wait does, so commands that arrive later never take the lock first.
func (h *Home) Lock(ctx context.Context, path string) (*Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	f, err := h.openLock()
	if err != nil {
		return nil, err
	}
	acquired := make(chan error, 1)
	go func() { acquired <- flock(f, unix.LOCK_EX) }()
	if lockWaiting != nil {
		lockWaiting()
	}
	select {
	case err := <-acquired:
		if err != nil {
			f.Close()
			return nil, &os.PathError{Op: "flock", Path: f.Name(), Err: err}
		}
		return hold(f, path)
	case <-ctx.Done():
		busy := h.busy()
		// The flock still waits. Release the lock once it lands.
		go func() {
			if <-acquired == nil {
				unix.Flock(int(f.Fd()), unix.LOCK_UN)
			}
			f.Close()
		}()
		return nil, busy
	}
}

// TryLock takes the global lock for a command that works on path only when
// no other command holds it. Otherwise it returns an error that wraps
// ErrLockBusy and names the holder.
func (h *Home) TryLock(path string) (*Lock, error) {
	f, err := h.openLock()
	if err != nil {
		return nil, err
	}
	if err := flock(f, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, h.busy()
		}
		return nil, &os.PathError{Op: "flock", Path: f.Name(), Err: err}
	}
	return hold(f, path)
}

// Unlock clears the holder and releases the lock.
func (l *Lock) Unlock() error {
	err := l.f.Truncate(0)
	if closeErr := l.f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (h *Home) openLock() (*os.File, error) {
	return os.OpenFile(filepath.Join(h.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
}

// flock is unix.Flock on f, retried when a signal interrupts it, as the Go
// runtime's preemption signal can.
func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// hold records this process as the holder of the lock f holds.
func hold(f *os.File, path string) (*Lock, error) {
	data, err := json.Marshal(holder{PID: os.Getpid(), Argv: os.Args[1:], Since: time.Now(), Path: path})
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.WriteAt(data, 0)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// busy returns ErrLockBusy, wrapped with who holds the lock and for how long.
func (h *Home) busy() error {
	data, err := os.ReadFile(filepath.Join(h.dir, "lock"))
	var held holder
	if err != nil || json.Unmarshal(data, &held) != nil {
		return fmt.Errorf("%w: another slimtree command holds it; rerun the command", ErrLockBusy)
	}
	on := ""
	if held.Path != "" {
		on = " on " + held.Path
	}
	return fmt.Errorf("%w: pid %d (slimtree %s) has held it%s for %.1f s; rerun the command once it ends", ErrLockBusy, held.PID, strings.Join(held.Argv, " "), on, time.Since(held.Since).Seconds())
}
