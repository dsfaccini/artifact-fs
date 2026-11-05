package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// daemonLockWait is how long a starting daemon waits for the daemon lock.
	// A CLI liveness probe holds it for an instant; a running daemon holds it
	// until it exits.
	daemonLockWait = time.Second
	// controlDialTimeout bounds the CLI's nudge of a daemon that stopped
	// accepting.
	controlDialTimeout = time.Second
)

func daemonLockPath(root string) string { return filepath.Join(root, "config", "daemon.lock") }

func controlSocketPath(root string) string { return filepath.Join(root, "config", "daemon.sock") }

// controlSocketFits reports whether path fits sockaddr_un's sun_path with its
// terminating NUL: 104 bytes on macOS, 108 on Linux.
func controlSocketFits(path string) bool {
	return len(path) < len(unix.RawSockaddrUnix{}.Path)
}

// acquireDaemonLock takes the flock that marks root's daemon as running and
// writes the daemon's pid into the lock file. Closing the file releases it.
func acquireDaemonLock(root string) (*os.File, error) {
	path := daemonLockPath(root)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(daemonLockWait); ; time.Sleep(repoConfigLockRetry) {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
			holder, _ := os.ReadFile(path)
			f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, fmt.Errorf("another artifact-fs daemon (pid %s) holds %s; stop it before starting a daemon on this state root", strings.TrimSpace(string(holder)), path)
			}
			return nil, &os.PathError{Op: "flock", Path: path, Err: err}
		}
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// daemonRunning reports whether a daemon holds root's daemon lock.
func daemonRunning(root string) (bool, error) {
	f, err := os.Open(daemonLockPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if err == nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return false, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// listenControl opens the control socket that CLI commands nudge so the
// daemon syncs the registry at once. The socket carries no data. It returns
// nil when the socket path does not fit sun_path; CLI commands then wait for
// the next registry poll.
func (s *Service) listenControl() (net.Listener, error) {
	path := controlSocketPath(s.root)
	if !controlSocketFits(path) {
		s.logger.Warn("control socket path too long; CLI commands wait for the registry poll", "path", path)
		return nil, nil
	}
	// The daemon lock makes this daemon the socket's only owner, so a socket
	// left here belongs to a daemon that stopped.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// serveControl turns each connection to l into a wake of the sync loop.
func (s *Service) serveControl(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		conn.Close()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// nudgeDaemon asks root's daemon to sync the registry now. A daemon it cannot
// reach still syncs at its next poll.
func nudgeDaemon(root string) {
	path := controlSocketPath(root)
	if !controlSocketFits(path) {
		return
	}
	conn, err := net.DialTimeout("unix", path, controlDialTimeout)
	if err != nil {
		return
	}
	conn.Close()
}
