package daemon

import "testing"

// The mount table names a FUSE mount's type fuse.<subtype>, which identifies
// a mount the daemon process before this one left.
func TestMountTableReadsTheFUSEType(t *testing.T) {
	const procMounts = "/dev/root / ext4 rw 0 0\n" +
		"artifact-fs:repo /home/u/mnt/my\\040repo fuse.artifact-fs rw,nosuid,nodev 0 0\n"
	entry, found := matchProcMounts(procMounts, "/home/u/mnt/my repo")
	if !found || entry.source != "artifact-fs:repo" || entry.fstype != "fuse.artifact-fs" || !isFUSEType(entry.fstype) {
		t.Fatalf("/proc/mounts entry = %+v, %t; want the artifact-fs FUSE mount", entry, found)
	}
	const mountOutput = "/dev/root on / type ext4 (rw)\nartifact-fs:repo on /home/u/mnt/repo type fuse.artifact-fs (rw,nosuid,nodev)\n"
	entry, found = matchMountOutput(mountOutput, "/home/u/mnt/repo")
	if !found || entry.source != "artifact-fs:repo" || entry.fstype != "fuse.artifact-fs" {
		t.Fatalf("mount(8) entry = %+v, %t; want the artifact-fs FUSE mount", entry, found)
	}
	for fstype, want := range map[string]bool{"fuse": true, "fuse.artifact-fs": true, "fuseblk": false, "ext4": false} {
		if got := isFUSEType(fstype); got != want {
			t.Errorf("isFUSEType(%q) = %t, want %t", fstype, got, want)
		}
	}
}
