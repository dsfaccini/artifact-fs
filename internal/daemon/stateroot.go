package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lstat is unix.Lstat. Tests replace it, because only root can create a
// symlink that another user owns.
var lstat = unix.Lstat

// stateRootDirs are the state root's fixed subdirectories. Every per-repo
// directory under the root sits inside one of them.
var stateRootDirs = []string{"config", filepath.Join("config", "locks"), filepath.Join("config", "tmp"), "repos", "overlays", "cache", "meta"}

// ensureStateRoot creates the state root and its fixed subdirectories 0700, and
// returns the root with its symlinks resolved. Use only the resolved root: a
// link that is retargeted after this check then redirects nothing.
//
// It refuses the root as given when another user owns it, even as a symlink to
// a directory of the current user, because that user chose the target. It
// refuses the resolved root or a subdirectory that another user owns or that
// group or others can write: whoever can write there can replace a file that
// Git runs, such as the fsmonitor hook on every status or the SSH wrapper in
// config/tmp, or the cached blobs that reads serve.
// Ancestors of the root are not checked.
//
// It narrows each existing subdirectory to 0700. The root keeps its mode,
// because the user chose it.
func ensureStateRoot(root string) (string, error) {
	// Check the owner before creating anything. With a trailing slash, macOS's
	// mkdir follows a dangling symlink and creates its target.
	root = filepath.Clean(root)
	var link unix.Stat_t
	err := lstat(root, &link)
	if errors.Is(err, unix.ENOENT) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		err = lstat(root, &link)
	}
	if err != nil {
		return "", &os.PathError{Op: "lstat", Path: root, Err: err}
	}
	if link.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("refusing state root %s: owned by another user", root)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if err := checkPrivateDir(resolved); err != nil {
		return "", err
	}
	for _, dir := range stateRootDirs {
		path := filepath.Join(resolved, dir)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
		if err := checkPrivateDir(path); err != nil {
			return "", err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return "", err
		}
	}
	return resolved, nil
}

// emptyDaemonTempDir empties config/tmp/daemon under root and returns it. Call
// it only while holding the daemon lock. Only the daemon that holds the lock
// uses the directory, so what is there a daemon left that never closed its
// git store. The CLI's git stores use config/tmp itself, at the same time.
func emptyDaemonTempDir(root string) (string, error) {
	dir := filepath.Join(root, "config", "tmp", "daemon")
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	return dir, os.Mkdir(dir, 0o700)
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
