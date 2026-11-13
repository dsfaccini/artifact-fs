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
