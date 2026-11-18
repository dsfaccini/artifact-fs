package mounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

// fsType is what the mount table lists as the type of a mount artifact-fs
// made.
const fsType = "fuse." + fusefs.FSSubtype

// mountIDMask asks statx for the unique mount ID, which Linux 6.8 added, and
// for the mountinfo ID, which Linux 5.8 added and which a later mount can
// reuse. A kernel that knows the unique ID returns it alone. An older one
// ignores the bit and returns the mountinfo ID.
const mountIDMask = unix.STATX_MNT_ID | unix.STATX_MNT_ID_UNIQUE

// mountID returns the ID of the mount that holds path, and whether the ID is
// unique. It sends the file system no request: AT_STATX_DONT_SYNC takes the
// attributes the kernel holds, and the ID comes from the mount.
func mountID(path string) (uint64, bool, error) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_NO_AUTOMOUNT|unix.AT_STATX_DONT_SYNC, mountIDMask, &st); err != nil {
		return 0, false, &os.PathError{Op: "statx", Path: path, Err: err}
	}
	switch {
	case st.Mask&unix.STATX_MNT_ID_UNIQUE != 0:
		return st.Mnt_id, true, nil
	case st.Mask&unix.STATX_MNT_ID != 0:
		return st.Mnt_id, false, nil
	}
	return 0, false, errors.New("statx returned no mount ID: slimtree needs Linux 5.8 or later")
}

// UniqueMountIDs reports whether the kernel gives each mount an ID that no
// later mount reuses, as Linux 6.8 and later do. It fails before Linux 5.8,
// which reports no mount ID.
func UniqueMountIDs() (bool, error) {
	_, unique, err := mountID("/")
	return unique, err
}

var bootID = sync.OnceValues(func() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("/proc/sys/kernel/random/boot_id is empty")
	}
	return id, nil
})

func readMountinfo() ([]mountinfoEntry, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return parseMountinfo(string(data))
}

// Identify returns the identity of the artifact-fs mount at path, or "" when
// nothing is mounted at path. It fails with ErrUnrelatedMount for any other
// mount there. The identity holds the boot ID, the mount's ID, and the
// mountinfo ID, device and source. From Linux 6.8 the mount's ID is unique.
// Before 6.8 it is the mountinfo ID, and both that ID and FUSE's device
// number reuse the lowest free number, while the source is
// artifact-fs:<repo>. So a later mount of the same repo at the same path
// most likely gets the same identity.
func Identify(path string) (Identity, error) {
	point, err := mountPoint(path)
	if err != nil {
		return "", err
	}
	entries, err := readMountinfo()
	if err != nil {
		return "", err
	}
	before, ok := mountAt(entries, point)
	if !ok {
		return "", nil
	}
	if before.fstype != fsType {
		return "", fmt.Errorf("%s: %w (%s)", point, ErrUnrelatedMount, before.fstype)
	}
	id, unique, err := mountID(point)
	if err != nil {
		return "", err
	}
	if entries, err = readMountinfo(); err != nil {
		return "", err
	}
	// The statx and the two reads describe one mount only when no mount or
	// unmount at point came between them.
	after, ok := mountAt(entries, point)
	if !ok || after != before || !unique && id != before.id {
		return "", fmt.Errorf("%s: the mount changed while slimtree read its identity", point)
	}
	boot, err := bootID()
	if err != nil {
		return "", err
	}
	kind := "mountinfo"
	if unique {
		kind = "unique"
	}
	return Identity(fmt.Sprintf("%s:%s-%d:%d:%d:%d:%s", boot, kind, id, before.id, before.major, before.minor, before.source)), nil
}

// KeyOf returns the key of the mount that holds path, a mount point or a
// path on a native file system: the mount's ID, and the device of the
// deepest mount at or above path in the mount table.
func KeyOf(path string) (Key, error) {
	point, err := mountPoint(path)
	if err != nil {
		return Key{}, err
	}
	id, _, err := mountID(point)
	if err != nil {
		return Key{}, err
	}
	entries, err := readMountinfo()
	if err != nil {
		return Key{}, err
	}
	e, ok := holdingMount(entries, point)
	if !ok {
		return Key{}, fmt.Errorf("no mount holds %s", point)
	}
	return Key{ID: id, Dev: unix.Mkdev(e.major, e.minor)}, nil
}

// Holders lists each process with its working directory, its root, an open
// file or a mapping under path on the mount key names, except ignore and its
// descendants. A process in the directory that a mount at path covers, which
// it entered while nothing was mounted there, does not hold the mount, and
// the mount check leaves it out. It scans /proc, so it misses the processes
// of another user that this process may not inspect. It reads links and
// statx's mount IDs, so it sends the mount's file system no request.
func Holders(ctx context.Context, path string, key Key, ignore int) ([]Holder, error) {
	point, err := mountPoint(path)
	if err != nil {
		return nil, err
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	parents := map[int]int{}
	var pids []int
	for _, p := range procs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		if parent, err := parentPID(pid); err == nil {
			parents[pid] = parent
		}
		if holds("/proc/"+p.Name(), point, key) {
			pids = append(pids, pid)
		}
	}
	commands := map[int]string{}
	for _, pid := range pids {
		for _, p := range []int{pid, parents[pid]} {
			if _, ok := commands[p]; !ok {
				data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", p))
				commands[p] = strings.TrimSpace(string(data))
			}
		}
	}
	return holders(pids, ignore, parents, commands), nil
}

// holds reports whether the process at proc, a /proc/<pid> directory, has
// its working directory, its root, an open file or a mapping under path on
// the mount key names.
func holds(proc, path string, key Key) bool {
	links := []string{proc + "/cwd", proc + "/root"}
	if fds, err := os.ReadDir(proc + "/fd"); err == nil {
		for _, fd := range fds {
			links = append(links, proc+"/fd/"+fd.Name())
		}
	}
	for _, link := range links {
		if target, err := os.Readlink(link); err == nil && under(target, path) {
			if id, _, err := mountID(link); err == nil && id == key.ID {
				return true
			}
		}
	}
	maps, err := os.ReadFile(proc + "/maps")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(maps), "\n") {
		// address perms offset dev inode path, where the path can hold spaces.
		fields := strings.SplitN(line, " ", 6)
		if len(fields) < 6 {
			continue
		}
		if !under(strings.TrimLeft(fields[5], " "), path) {
			continue
		}
		if major, minor, err := parseDevice(fields[3], 16); err == nil && unix.Mkdev(major, minor) == key.Dev {
			return true
		}
	}
	return false
}

// ParentPID returns the PID of pid's parent.
func ParentPID(pid int) (int, error) { return parentPID(pid) }

// parentPID returns pid's parent from /proc/<pid>/stat.
func parentPID(pid int) (int, error) {
	fields, err := readStat(pid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(fields[1])
}

func readStat(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	return statFields(string(data))
}

// StartToken returns the start token of the process pid names, or "" when
// no process has pid. The token is the boot ID and field 22 of
// /proc/<pid>/stat, the start time in clock ticks since boot, which fork
// sets and exec keeps. It reads no argv: right after exec, Linux shows an
// empty cmdline until the new argv is in place. It answers for a zombie too,
// so a launcher can record the token of a child that already exited. A
// matching token therefore does not mean that the process runs: Signal with
// signal 0 tells that. A pid and its token name one process; two processes
// can share a token. Tell that a child of this process exited only from its
// wait status.
func StartToken(pid int) (string, error) {
	token, _, err := startTokenState(pid)
	return token, err
}

// startTokenState returns StartToken's answer and whether the process has
// exited: a zombie, or dead.
func startTokenState(pid int) (string, bool, error) {
	fields, err := readStat(pid)
	if absent(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	boot, err := bootID()
	if err != nil {
		return "", false, err
	}
	return boot + ":" + fields[19], fields[0] == "Z" || fields[0] == "X", nil
}

// Signal sends sig to the process pid names while its start token is token
// and it has not exited, and returns ErrGone otherwise, also for a zombie.
// With signal 0 it only tells whether the process runs. It opens a pidfd
// before it reads the token. A process that held pid since before the open
// and still holds it is the one the pidfd names, so no other process can
// take the signal. The open fails for a pid no process leader holds, such as
// a thread's ID, so the token decides then too.
func Signal(pid int, token string, sig syscall.Signal) error {
	if token == "" {
		return errors.New("signal: no start token")
	}
	fd, openErr := unix.PidfdOpen(pid, 0)
	if openErr == nil {
		defer unix.Close(fd)
	}
	got, exited, err := startTokenState(pid)
	if err != nil {
		return err
	}
	if got != token || exited {
		return ErrGone
	}
	if openErr != nil {
		return os.NewSyscallError("pidfd_open", openErr)
	}
	if err := unix.PidfdSendSignal(fd, sig, nil, 0); errors.Is(err, unix.ESRCH) {
		return ErrGone
	} else if err != nil {
		return os.NewSyscallError("pidfd_send_signal", err)
	}
	return nil
}

// unmountPath unmounts the mount at path, whose identity is id, with
// fusermount3, else fusermount, which the fuse library falls back to. A
// daemon that died leaves a mount the kernel no longer serves, which a
// normal unmount can fail to take. A lazy unmount then detaches it, but only
// while identify still finds id at path: the normal unmount can fail
// because the mount already went, and another mount can take the path.
func unmountPath(ctx context.Context, path string, id Identity, identify func(string) (Identity, error)) error {
	prog, err := exec.LookPath("fusermount3")
	if err != nil {
		if prog, err = exec.LookPath("fusermount"); err != nil {
			return fmt.Errorf("unmount %s: neither fusermount3 nor fusermount is on PATH; run slimtree doctor", path)
		}
	}
	_, err = run(ctx, prog, "-u", path)
	if err == nil {
		return nil
	}
	got, idErr := identify(path)
	switch {
	case idErr != nil:
		return fmt.Errorf("unmount %s: %w; then: %w", path, err, idErr)
	case got == "":
		return nil
	case got != id:
		return fmt.Errorf("unmount %s: %w", path, ErrOtherMount)
	}
	if _, lazyErr := run(ctx, prog, "-uz", path); lazyErr != nil {
		return fmt.Errorf("unmount %s: %w; lazy unmount: %w", path, err, lazyErr)
	}
	return nil
}
