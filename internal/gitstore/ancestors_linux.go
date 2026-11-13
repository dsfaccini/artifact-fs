package gitstore

import (
	"errors"

	"golang.org/x/sys/unix"
)

// hasACL reports whether path carries a POSIX access ACL, or whether that
// cannot be read. A file system without ACL support carries none.
func hasACL(path string) bool {
	_, err := unix.Lgetxattr(path, "system.posix_acl_access", nil)
	return !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP)
}

// onNFS reports whether path lies on NFS, or whether that cannot be read. The
// NFSv4 client shows no ACL as system.posix_acl_access, and an NFSv4 ACL can
// grant another user write that the mode does not show. Tests replace it.
var onNFS = func(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) != nil || st.Type == unix.NFS_SUPER_MAGIC
}
