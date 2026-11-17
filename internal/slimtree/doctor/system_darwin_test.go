package doctor

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// The probe clones a file in an APFS directory and leaves nothing there. /
// is the read-only system volume.
func TestAPFSProbe(t *testing.T) {
	dir := t.TempDir()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	if fstype := unix.ByteSliceToString(st.Fstypename[:]); fstype != "apfs" {
		t.Skipf("the temporary directory %s is on %s, not APFS", dir, fstype)
	}
	if err := apfs(dir); err != nil {
		t.Fatalf("apfs(%s): %v", dir, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("after the probe, %s holds %v (%v), want nothing", dir, entries, err)
	}
	if err := apfs("/"); !errors.Is(err, errNotWritable) {
		t.Errorf("apfs(/) = %v, want errNotWritable", err)
	}
}
