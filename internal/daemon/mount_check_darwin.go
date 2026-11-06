package daemon

import (
	"strings"

	"golang.org/x/sys/unix"
)

// mountTableEntry reports whether the given path is an active mount point,
// and what the kernel's mount table lists for it. It reads the table
// directly, so it needs no mount(8) on PATH. macOS reports /private/tmp even
// for /tmp paths. The comparison ignores case, as the default APFS volume
// does.
func mountTableEntry(path string) (mountEntry, bool, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return mountEntry{}, false, err
	}
	// Leave room for mounts added between the two calls.
	buf := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return mountEntry{}, false, err
	}
	// A later entry for the same path is mounted on top of an earlier one.
	var entry mountEntry
	found := false
	for _, st := range buf[:n] {
		dir := unix.ByteSliceToString(st.Mntonname[:])
		if strings.EqualFold(dir, path) || strings.EqualFold(dir, "/private"+path) {
			entry = mountEntry{source: unix.ByteSliceToString(st.Mntfromname[:]), fstype: unix.ByteSliceToString(st.Fstypename[:])}
			found = true
		}
	}
	return entry, found, nil
}
