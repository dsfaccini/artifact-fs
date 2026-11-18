package state

import (
	"os"

	"golang.org/x/sys/unix"
)

// fileSystem returns the f_type statfs(2) reports for path.
func fileSystem(path string) (int64, string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, "", err
	}
	return int64(st.Type), "", nil
}

// diskSize returns the size of the file system that holds path. f_blocks
// counts f_frsize units.
func diskSize(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	return st.Blocks * uint64(st.Frsize), nil
}
