//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// errMountWedged reports a stalled mount. A stalled FSKit mount cannot be
// unmounted until reboot, and every process that touches it hangs.
var errMountWedged = errors.New("mount is wedged and needs a reboot to clear")

// wedgedMount holds the first mount path that stalled. Once it is set, e2e
// tests skip, and cleanup stops unmounting and deleting mount directories.
var wedgedMount atomic.Pointer[string]

func markMountWedged(mountPath string) error {
	wedgedMount.CompareAndSwap(nil, &mountPath)
	return fmt.Errorf("%s: %w; skipping remaining e2e tests", mountPath, errMountWedged)
}

func skipIfMountWedged(t *testing.T) {
	t.Helper()
	if mountPath := wedgedMount.Load(); mountPath != nil {
		t.Skipf("skipping: %s: %v", *mountPath, errMountWedged)
	}
}

// closeE2EService closes svc. A Close that outlives timeout is stuck unmounting,
// so it marks mountPath wedged. After any wedge it leaves svc running.
func closeE2EService(svc io.Closer, mountPath string, timeout time.Duration) error {
	if wedged := wedgedMount.Load(); wedged != nil {
		return fmt.Errorf("leaving the daemon for %s running: %s: %w", mountPath, *wedged, errMountWedged)
	}
	closed := make(chan error, 1)
	go func() { closed <- svc.Close() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-closed:
		return err
	case <-timer.C:
		return fmt.Errorf("Service.Close did not return within %s: %w", timeout, markMountWedged(mountPath))
	}
}

// newE2EMountDir returns a mount root for repoName. Its cleanup checks mount
// state first, where t.TempDir would delete through a mount that failed to unmount.
func newE2EMountDir(t *testing.T) string {
	t.Helper()
	mountDir, err := os.MkdirTemp("", "artifact-fs-e2e-mount-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeE2EMountDir(t, mountDir, filepath.Join(mountDir, repoName)) })
	return mountDir
}

// removeE2EMountDir deletes mountDir unless mountPath is still mounted or a
// mount is wedged. RemoveAll would delete through a live mount and hang on a wedged one.
func removeE2EMountDir(t *testing.T, mountDir, mountPath string) {
	t.Helper()
	if wedged := wedgedMount.Load(); wedged != nil {
		t.Logf("leaving %s in place: %s: %v", mountDir, *wedged, errMountWedged)
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for isMounted(mountPath) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if isMounted(mountPath) {
		t.Errorf("leaving %s in place: %s is still mounted", mountDir, mountPath)
		return
	}
	removeAllWithRetry(t, mountDir)
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestStalledCloseSkipsRemainingE2ETests(t *testing.T) {
	previous := wedgedMount.Swap(nil)
	t.Cleanup(func() { wedgedMount.Store(previous) })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stalled := closerFunc(func() error { <-release; return nil })
	if err := closeE2EService(stalled, "/stalled/mount", time.Millisecond); !errors.Is(err, errMountWedged) {
		t.Fatalf("stalled Close error = %v, want %v", err, errMountWedged)
	}

	skipped := false
	t.Run("later e2e test", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		skipIfMountWedged(t)
	})
	if !skipped {
		t.Fatal("an e2e test ran after a mount wedged")
	}

	var closed atomic.Bool
	tracked := closerFunc(func() error { closed.Store(true); return nil })
	if err := closeE2EService(tracked, "/other/mount", time.Second); !errors.Is(err, errMountWedged) || closed.Load() {
		t.Fatalf("Close after a wedge: closed=%v, error=%v", closed.Load(), err)
	}

	mountDir := t.TempDir()
	removeE2EMountDir(t, mountDir, filepath.Join(mountDir, repoName))
	if _, err := os.Stat(mountDir); err != nil {
		t.Fatalf("cleanup removed a mount directory after a wedge: %v", err)
	}
}

func TestRemoveE2EMountDirDeletesUnmountedDir(t *testing.T) {
	previous := wedgedMount.Swap(nil)
	t.Cleanup(func() { wedgedMount.Store(previous) })

	mountDir := newE2EMountDir(t)
	mountPath := filepath.Join(mountDir, repoName)
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}
	removeE2EMountDir(t, mountDir, mountPath)
	if _, err := os.Stat(mountDir); !os.IsNotExist(err) {
		t.Fatalf("unmounted mount directory remains: %v", err)
	}
}
