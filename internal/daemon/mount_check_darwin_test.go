package daemon

import "testing"

// The mount table lists macFUSE's FSKit mounts with the type macfuse, which
// identifies a mount the daemon process before this one left.
func TestMountTableReadsTheFUSEType(t *testing.T) {
	entry, found, err := mountTableEntry("/")
	if err != nil || !found || entry.fstype == "" || isFUSEType(entry.fstype) {
		t.Fatalf("mount table entry for / = %+v, %t, %v; want a listed volume that is not FUSE", entry, found, err)
	}
	for fstype, want := range map[string]bool{"macfuse": true, "apfs": false, "fskit": false} {
		if got := isFUSEType(fstype); got != want {
			t.Errorf("isFUSEType(%q) = %t, want %t", fstype, got, want)
		}
	}
}
