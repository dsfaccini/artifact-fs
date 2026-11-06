package daemon

import (
	"strings"

	"golang.org/x/sys/unix"
)

// mountTableHas reports whether the given path is an active mount point. It
// reads the kernel's mount table, so it needs no mount(8) on PATH. macOS
// reports /private/tmp even for /tmp paths. The comparison ignores case, as
// the default APFS volume does.
func mountTableHas(path string) (bool, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return false, err
	}
	// Leave room for mounts added between the two calls.
	buf := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return false, err
	}
	for _, st := range buf[:n] {
		dir := unix.ByteSliceToString(st.Mntonname[:])
		if strings.EqualFold(dir, path) || strings.EqualFold(dir, "/private"+path) {
			return true, nil
		}
	}
	return false, nil
}
