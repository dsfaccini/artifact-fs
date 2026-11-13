//go:build !windows

package gitstore

import (
	"os"
	"path/filepath"
	"syscall"
)

// lstat is syscall.Lstat. Tests replace it, because only root can give a
// directory another owner.
var lstat = syscall.Lstat

// privateAncestors reports whether only the effective user and root can
// write or rename stateRoot and each directory above it. Git runs the hook
// link by its path, so whoever can rename a directory on that path chooses
// what Git runs. A stateRoot that is not its own resolved path fails.
//
// It checks each directory as OpenSSH's StrictModes does: root or the
// effective user owns it, and others cannot write it. Its group can write it
// only when the effective user owns it and is the group's only member, as
// Debian's OpenSSH accepts. Under umask 002 on a system with a private group
// per user, every directory a user makes is group-writable, and the check
// would otherwise always fail there. A sticky directory such as /tmp fails:
// once an entry in it is gone, after a reboot or a cleanup, any user can make
// one at its path. On Linux a directory with an access ACL fails, because the
// ACL can let another user write it without showing in the mode. macOS ACLs
// go unread.
func privateAncestors(stateRoot string) bool {
	if resolved, err := filepath.EvalSymlinks(stateRoot); err != nil || resolved != stateRoot {
		return false
	}
	euid := uint32(os.Geteuid())
	privateGroups := map[uint32]bool{}
	for dir := stateRoot; ; dir = filepath.Dir(dir) {
		var st syscall.Stat_t
		if err := lstat(dir, &st); err != nil || hasACL(dir) {
			return false
		}
		mode := uint32(st.Mode)
		if (st.Uid != 0 && st.Uid != euid) || mode&0o002 != 0 {
			return false
		}
		if mode&0o020 != 0 {
			private, ok := privateGroups[st.Gid]
			if !ok {
				private = privateGroup(st.Gid, euid)
				privateGroups[st.Gid] = private
			}
			if st.Uid != euid || !private {
				return false
			}
		}
		if filepath.Dir(dir) == dir {
			return true
		}
	}
}
