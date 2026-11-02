package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"golang.org/x/sys/unix"
)

// skipIfNoFUSE skips the test unless it can mount without prompting macOS to
// load the macFUSE kernel extension or exec the mount helper, and fails when
// ARTIFACT_FS_FUSE_BACKEND cannot mount in this build. Some sandboxed
// environments expose the macFUSE bundle but deny exec of mount_macfuse;
// without these preflights every e2e case waits for its mount timeout.
func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	skipIfMountWedged(t)
	if _, err := os.Stat("/Library/Filesystems/macfuse.fs"); err != nil {
		t.Skip("skipping: macFUSE not installed")
	}
	backend, err := fusefs.MountBackend()
	if err != nil {
		t.Fatal(err)
	}
	if backend != "fskit" {
		if _, err := unix.SysctlRaw("vfs.generic.macfuse.version.number"); err != nil {
			t.Skip("skipping: the macFUSE kernel extension is not loaded, and mounting would prompt to load it; " +
				"set ARTIFACT_FS_FUSE_BACKEND=fskit and build with -tags fskit to test on FSKit")
		}
	}
	helper := "/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse"
	cmd := exec.Command(helper, "--help")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(string(out), "operation not permitted") {
		t.Skipf("skipping: macFUSE mount helper is not executable in this environment: %v", err)
	}
}

// configGitSafeDir adds safe.directory entries for the mount path.
// macOS resolves /tmp to /private/tmp, so we add both variants.
func configGitSafeDir(t *testing.T, path string) {
	t.Helper()
	for _, p := range []string{path, fmt.Sprintf("/private%s", path)} {
		exec.Command("git", "config", "--global", "--add", "safe.directory", p).Run()
	}
}

// isMounted checks whether the given path is a FUSE mount point.
// On macOS, mount(8) reports /private/tmp even for /tmp paths.
func isMounted(path string) bool {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, path) || strings.Contains(s, "/private"+path)
}

func primeE2EMountPolling(*testing.T, string) {}
