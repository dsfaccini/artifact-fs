package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func osVersion() (string, error) { return unix.Sysctl("kern.osproductversion") }

// apfs checks dir's file system type, then clones a probe file there, because
// clonefile is what the apfsclone backend needs.
func apfs(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return err
	}
	if fstype := unix.ByteSliceToString(st.Fstypename[:]); fstype != "apfs" {
		return fmt.Errorf("%s is on %s, not APFS", dir, fstype)
	}
	f, err := os.CreateTemp(dir, ".slimtree-probe-")
	if err != nil {
		return err
	}
	src := f.Name()
	defer os.Remove(src)
	if err := f.Close(); err != nil {
		return err
	}
	dst := filepath.Join(dir, filepath.Base(src)+"-clone")
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err != nil {
		return fmt.Errorf("clonefile in %s: %w", dir, err)
	}
	return os.Remove(dst)
}
