package state

import "golang.org/x/sys/unix"

// fileSystem returns the f_type statfs(2) reports for path.
func fileSystem(path string) (int64, string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, "", err
	}
	return int64(st.Type), "", nil
}
