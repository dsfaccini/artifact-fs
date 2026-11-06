package daemon

import (
	"runtime"
	"testing"
)

// The Linux mount table names a mount's source and type, which tell a
// daemon's own mount of a repo from any other mount.
func TestMountTableReadsTheMountSourceAndType(t *testing.T) {
	const procMounts = "/dev/root / ext4 rw 0 0\n" +
		"artifact-fs:my\\040repo /home/u/mnt/my\\040repo fuse.artifact-fs rw,nosuid,nodev 0 0\n"
	entry, found := matchProcMounts(procMounts, "/home/u/mnt/my repo")
	if !found || entry != (mountEntry{source: "artifact-fs:my repo", fstype: "fuse.artifact-fs"}) || !ownsLeftover(runtime.GOOS, entry, "my repo") {
		t.Fatalf("/proc/mounts entry = %+v, %t; want the daemon's mount of my repo", entry, found)
	}
	const mountOutput = "/dev/root on / type ext4 (rw)\nartifact-fs:repo on /home/u/mnt/repo type fuse.artifact-fs (rw,nosuid,nodev)\n"
	entry, found = matchMountOutput(mountOutput, "/home/u/mnt/repo")
	if !found || entry != (mountEntry{source: "artifact-fs:repo", fstype: "fuse.artifact-fs"}) {
		t.Fatalf("mount(8) entry = %+v, %t; want the daemon's mount of repo", entry, found)
	}
}
