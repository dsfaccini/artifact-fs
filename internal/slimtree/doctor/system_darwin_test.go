package doctor

import (
	"errors"
	"os"
	"testing"
)

// The probe clones a file in an APFS directory and leaves nothing there.
// darwin-test's temporary directory is on APFS, and / is the read-only
// system volume.
func TestAPFSProbe(t *testing.T) {
	dir := t.TempDir()
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
