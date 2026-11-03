package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// stateRootDirs are the state root's fixed subdirectories. Every per-repo
// directory under the root sits inside one of them.
var stateRootDirs = []string{"config", filepath.Join("config", "locks"), "repos", "overlays", "cache", "meta"}

// ensureStateRoot creates the state root and its fixed subdirectories 0700.
// It refuses any of them that another user owns or that group or others can
// write: whoever can write there can replace the fsmonitor hook that Git runs
// on every status, or the cached blobs that reads serve.
func ensureStateRoot(root string) error {
	for _, dir := range append([]string{"."}, stateRootDirs...) {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		if err := checkPrivateDir(path); err != nil {
			return err
		}
	}
	return nil
}

func checkPrivateDir(path string) error {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("refusing state directory %s: owned by another user", path)
	}
	if st.Mode&0o022 != 0 {
		return fmt.Errorf("refusing state directory %s: writable by group or others (mode %04o)", path, st.Mode&0o777)
	}
	return nil
}
