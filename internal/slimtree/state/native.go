package state

import "strings"

// fuseSuperMagic is the f_type statfs(2) reports on Linux for a FUSE mount.
const fuseSuperMagic = 0x65735546

// onFUSE reports whether the file system statfs(2) describes with magic and
// name is a FUSE mount on goos. Linux names it by magic number. macOS names
// macFUSE's mounts, kernel extension and FSKit alike, macfuse.
func onFUSE(goos string, magic int64, name string) bool {
	if goos == "linux" {
		return magic == fuseSuperMagic
	}
	return strings.Contains(name, "fuse")
}
