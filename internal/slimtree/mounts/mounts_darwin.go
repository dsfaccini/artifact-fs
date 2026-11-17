package mounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// mntExtFSKit is MNT_EXT_FSKIT from sys/mount.h: an FSKit mount.
	mntExtFSKit = 0x2
	// szomb is SZOMB from sys/proc.h: a zombie.
	szomb = 5
)

// mountTable reads the kernel's mount table. MNT_NOWAIT returns what the
// kernel holds, without asking any file system, so a hung mount cannot block
// it.
func mountTable() ([]unix.Statfs_t, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	// Leave room for mounts added between the two calls.
	buf := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// mountAtPoint returns the mount at point: the last entry there, which is
// mounted on top of the earlier ones. Like the daemon's mount check, it
// ignores case, as the default APFS volume does.
func mountAtPoint(table []unix.Statfs_t, point string) (unix.Statfs_t, bool) {
	var found unix.Statfs_t
	ok := false
	for _, e := range table {
		if strings.EqualFold(unix.ByteSliceToString(e.Mntonname[:]), point) {
			found, ok = e, true
		}
	}
	return found, ok
}

// Identify returns the identity of the artifact-fs mount at path, or "" when
// nothing is mounted at path. It fails with ErrUnrelatedMount for any other
// mount there. The identity is the mount's source, macfuse://<UUID>, which
// macFUSE's FSKit backend gives each mount.
func Identify(path string) (Identity, error) {
	point, err := mountPoint(path)
	if err != nil {
		return "", err
	}
	table, err := mountTable()
	if err != nil {
		return "", err
	}
	return identityIn(table, point)
}

// identityIn is Identify on the mount table table.
func identityIn(table []unix.Statfs_t, point string) (Identity, error) {
	e, ok := mountAtPoint(table, point)
	if !ok {
		return "", nil
	}
	fstype := unix.ByteSliceToString(e.Fstypename[:])
	source := unix.ByteSliceToString(e.Mntfromname[:])
	if e.Flags_ext&mntExtFSKit == 0 || fstype != "macfuse" || !strings.HasPrefix(source, "macfuse://") {
		return "", fmt.Errorf("%s: %w (%s from %s)", point, ErrUnrelatedMount, fstype, source)
	}
	return Identity(source), nil
}

// KeyOf returns the key of the mount that holds path, a mount point or a
// path on a native file system: the device of the mount at path from the
// mount table, else path's device. Only a path no mount covers is stat'ed. A
// prefix match on the mount table would misread firmlinks such as
// /private/var.
func KeyOf(path string) (Key, error) {
	point, err := mountPoint(path)
	if err != nil {
		return Key{}, err
	}
	table, err := mountTable()
	if err != nil {
		return Key{}, err
	}
	if e, ok := mountAtPoint(table, point); ok {
		return Key{Dev: uint64(uint32(e.Fsid.Val[0]))}, nil
	}
	var st unix.Stat_t
	if err := unix.Stat(point, &st); err != nil {
		return Key{}, &os.PathError{Op: "stat", Path: point, Err: err}
	}
	return Key{Dev: uint64(uint32(st.Dev))}, nil
}

// Holders lists each process with its working directory, an open file or a
// mapping under path on the mount key names, except ignore and its
// descendants. A process in the directory that a mount at path covers, which
// it entered while nothing was mounted there, does not hold the mount, and
// the device check leaves it out. It runs lsof, which lists another user's
// processes only when slimtree runs as root.
func Holders(ctx context.Context, path string, key Key, ignore int) ([]Holder, error) {
	point, err := mountPoint(path)
	if err != nil {
		return nil, err
	}
	// -b keeps lsof from stat'ing paths itself, but the kernel still stats
	// each vnode lsof reports, so a hung volume anywhere can stall it. run
	// bounds it.
	out, err := run(ctx, "/usr/sbin/lsof", "-n", "-P", "-b", "-w", "-F", "pcnRD")
	if err != nil {
		// lsof exits 1 when it could not read some processes, and lists the
		// rest. A stopped lsof listed only some of them.
		if _, exited := errors.AsType[*exec.ExitError](err); !exited || len(out) == 0 {
			return nil, fmt.Errorf("list the processes using %s: %w", point, err)
		}
	}
	return parseLsof(string(out), point, key.Dev, ignore), nil
}

// StartToken returns the start token of the process pid names, or "" when
// no process has pid or it is a zombie. The token is the start time,
// start_sec:start_usec, which fork sets and exec keeps. kern.proc.pid reads
// the p_start that proc_pidinfo's PROC_PIDTBSDINFO reads, and answers for
// another user's process too. It reads no argv. A pid and its token name one
// process. Signal with signal 0 tells whether the process runs. Tell that a
// child of this process exited only from its wait status.
func StartToken(pid int) (string, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if len(procs) == 0 || procs[0].Proc.P_stat == szomb {
		return "", nil
	}
	t := procs[0].Proc.P_starttime
	return fmt.Sprintf("%d:%d", t.Sec, t.Usec), nil
}

// Signal sends sig to the process pid names while its start token is token,
// and returns ErrGone otherwise, also for a zombie, which has no token. With
// signal 0 it only tells whether the process runs. macOS has no pidfd: a process that took pid
// between the token read and the kill would need the PID space to wrap in
// that instant.
func Signal(pid int, token string, sig syscall.Signal) error {
	if token == "" {
		return errors.New("signal: no start token")
	}
	got, err := StartToken(pid)
	if err != nil {
		return err
	}
	if got != token {
		return ErrGone
	}
	if err := unix.Kill(pid, sig); errors.Is(err, unix.ESRCH) {
		return ErrGone
	} else if err != nil {
		return os.NewSyscallError("kill", err)
	}
	return nil
}

// unmountPath unmounts path with umount(8).
func unmountPath(ctx context.Context, path string, _ Identity, _ func(string) (Identity, error)) error {
	_, err := run(ctx, "/sbin/umount", path)
	return err
}
