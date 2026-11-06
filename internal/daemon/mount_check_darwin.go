package daemon

import (
	"strings"

	"golang.org/x/sys/unix"
)

// mountTableSource reports whether the given path is an active mount point,
// and the source the kernel's mount table names for it. It reads the table
// directly, so it needs no mount(8) on PATH. macOS reports /private/tmp even
// for /tmp paths. The comparison ignores case, as the default APFS volume
// does.
func mountTableSource(path string) (string, bool, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return "", false, err
	}
	// Leave room for mounts added between the two calls.
	buf := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return "", false, err
	}
	// A later entry for the same path is mounted on top of an earlier one.
	source, found := "", false
	for _, st := range buf[:n] {
		dir := unix.ByteSliceToString(st.Mntonname[:])
		if strings.EqualFold(dir, path) || strings.EqualFold(dir, "/private"+path) {
			source, found = unix.ByteSliceToString(st.Mntfromname[:]), true
		}
	}
	return source, found, nil
}
