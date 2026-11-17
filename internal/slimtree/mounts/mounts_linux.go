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
// later mount reuses, as Linux 6.8 and later do. Before 6.8, Identify tells a
// mount from a later one at the same path by a reusable ID, the device and
// the source. It fails before Linux 5.8, which reports no mount ID.
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
// mountinfo ID, device and source.
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
// empty cmdline until the new argv is in place. It answers for a zombie too.
// A pid and its token name one process; two processes can share a token.
// For a child of this process, only its wait status tells that it exited.
func StartToken(pid int) (string, error) {
	fields, err := readStat(pid)
	if absent(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	boot, err := bootID()
	if err != nil {
		return "", err
	}
	return boot + ":" + fields[19], nil
}

// Signal sends sig to the process pid names while its start token is token,
// and returns ErrGone otherwise. It opens a pidfd before it reads the token.
// A process that held pid since before the open and still holds it is the
// one the pidfd names, so no other process can take the signal.
func Signal(pid int, token string, sig syscall.Signal) error {
	if token == "" {
		return errors.New("signal: no start token")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return ErrGone
	}
	if err != nil {
		return os.NewSyscallError("pidfd_open", err)
	}
	defer unix.Close(fd)
	got, err := StartToken(pid)
	if err != nil {
		return err
	}
	if got != token {
		return ErrGone
	}
	if err := unix.PidfdSendSignal(fd, sig, nil, 0); errors.Is(err, unix.ESRCH) {
		return ErrGone
	} else if err != nil {
		return os.NewSyscallError("pidfd_send_signal", err)
	}
	return nil
}

// unmountPath unmounts path with fusermount3, else fusermount, which the
// fuse library falls back to. A daemon that died leaves a mount the kernel
// no longer serves, which a normal unmount can fail to take. A lazy unmount
// then detaches it.
func unmountPath(ctx context.Context, path string) error {
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
	if _, lazyErr := run(ctx, prog, "-uz", path); lazyErr != nil {
		return fmt.Errorf("unmount %s: %w; lazy unmount: %w", path, err, lazyErr)
	}
	return nil
}
