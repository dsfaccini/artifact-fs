//go:build !windows

package gitstore

import (
	"os"
	"path/filepath"
	"syscall"
)

// privateAncestors reports whether only the effective user and root can
// rename each directory above stateRoot. Git runs the hook link by its path,
// so whoever can rename a directory on that path chooses what Git runs.
// ensureStateRoot checks the state root itself.
//
// It checks each directory as OpenSSH's StrictModes does: root or the
// effective user owns it, and neither its group nor others can write it. It
// accepts two exceptions. In a sticky directory, such as /tmp, only an
// entry's owner can rename the entry. Group write is safe when the group is
// private to the effective user, as Debian's OpenSSH accepts: under umask 002
// on a system with a private group per user, every directory a user makes is
// group-writable, and the check would otherwise always fail there.
func privateAncestors(stateRoot string) bool {
	euid := uint32(os.Geteuid())
	for dir := filepath.Dir(stateRoot); ; dir = filepath.Dir(dir) {
		var st syscall.Stat_t
		if err := syscall.Lstat(dir, &st); err != nil {
			return false
		}
		mode := uint32(st.Mode)
		switch {
		case st.Uid != 0 && st.Uid != euid:
			return false
		case mode&syscall.S_ISVTX != 0:
		case mode&0o002 != 0:
			return false
		case mode&0o020 != 0 && (st.Uid != euid || !privateGroup(st.Gid, euid)):
			return false
		}
		if filepath.Dir(dir) == dir {
			return true
		}
	}
}
