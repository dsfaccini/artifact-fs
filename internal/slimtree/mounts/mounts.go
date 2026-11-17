// Package mounts reads the artifact-fs mounts slimtree makes and the
// processes that use them, unmounts them, and signals the daemons that serve
// them. It reads mounts from the kernel's mount table and processes from the
// kernel's process table, so no read reaches a mount's file system, and a
// hung daemon cannot block one. It unmounts only a mount whose identity
// matches, and signals only a process whose start token matches.
package mounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Identity tells one mount from every other, a later mount at the same path
// included.
type Identity string

// Key identifies the mount that holds a path, read without reaching that
// mount's file system.
type Key struct {
	// ID is the mount's ID on Linux and 0 on macOS.
	ID uint64
	// Dev is the mount's device number. On macOS it keeps the low 32 bits,
	// as lsof prints them.
	Dev uint64
}

// Holder is a process with its working directory, its root, an open file or
// a mapping under a path.
type Holder struct {
	PID           int
	Command       string
	ParentPID     int
	ParentCommand string
}

var (
	// ErrUnrelatedMount reports a mount at the path that artifact-fs did not
	// make.
	ErrUnrelatedMount = errors.New("a mount artifact-fs did not make")
	// ErrOtherMount reports an artifact-fs mount at the path whose identity
	// is not the expected one.
	ErrOtherMount = errors.New("another mount than the expected one")
	// ErrGone reports that a PID no longer names the process its start token
	// named.
	ErrGone = errors.New("the process has exited")
)

// identifyHook, when set, replaces Identify in Unmount.
var identifyHook func(path string) (Identity, error)

// Unmount unmounts the mount at path when its identity is id, then checks
// that it went. It does nothing when nothing is mounted at path, and refuses
// every other mount there. It leaves holders alone: on Linux, a mount that a
// normal unmount cannot take is detached lazily, and stays alive for each
// process that holds it. Check Holders first.
func Unmount(ctx context.Context, path string, id Identity) error {
	identify := Identify
	if identifyHook != nil {
		identify = identifyHook
	}
	point, err := mountPoint(path)
	if err != nil {
		return err
	}
	got, err := identify(point)
	if err != nil || got == "" {
		return err
	}
	if got != id {
		return fmt.Errorf("unmount %s: %w", point, ErrOtherMount)
	}
	if err := unmountPath(ctx, point, id, identify); err != nil {
		return err
	}
	if got, err = identify(point); err != nil {
		return err
	}
	if got == id {
		return fmt.Errorf("unmount %s: still mounted", point)
	}
	return nil
}

// mountPoint returns path absolute with its parent's symlinks resolved, as
// the mount table lists a mount point. It never touches path itself, which
// can be a hung mount.
func mountPoint(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if path == "/" {
		return path, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// under reports whether target is path or a path below it.
func under(target, path string) bool {
	return target == path || len(target) > len(path) && target[:len(path)] == path && (path == "/" || target[len(path)] == '/')
}

// descends reports whether pid is ancestor or one of its descendants, by
// parents. An ancestor of 0 or below matches nothing.
func descends(pid, ancestor int, parents map[int]int) bool {
	if ancestor <= 0 {
		return false
	}
	seen := map[int]bool{}
	for pid > 0 && !seen[pid] {
		if pid == ancestor {
			return true
		}
		seen[pid] = true
		pid = parents[pid]
	}
	return false
}

// holders lists each of pids that is not ignore or one of its descendants,
// with its command and its parent.
func holders(pids []int, ignore int, parents map[int]int, commands map[int]string) []Holder {
	var out []Holder
	for _, pid := range pids {
		if descends(pid, ignore, parents) {
			continue
		}
		parent := parents[pid]
		out = append(out, Holder{PID: pid, Command: commands[pid], ParentPID: parent, ParentCommand: commands[parent]})
	}
	return out
}

// absent reports whether err says a file or a process is gone.
func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}
