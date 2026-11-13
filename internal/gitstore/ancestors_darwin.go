package gitstore

import "golang.org/x/sys/unix"

// hasACL reports false: macOS keeps ACLs out of the mode and out of the
// attributes this check reads.
func hasACL(string) bool { return false }

// onNFS reports whether path lies on NFS, or whether that cannot be read. An
// NFSv4 ACL can grant another user write that the mode does not show. Tests
// replace it.
var onNFS = func(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) != nil || unix.ByteSliceToString(st.Fstypename[:]) == "nfs"
}
