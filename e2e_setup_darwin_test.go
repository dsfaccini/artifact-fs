package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"golang.org/x/sys/unix"
)

// skipIfNoFUSE skips the test unless ARTIFACT_FS_FUSE_BACKEND selects a transport
// that can mount here without prompting macOS to load the macFUSE kernel
// extension, and fails when the variable is invalid. It also skips when the
// mount helper cannot be executed: some sandboxed environments expose the
// macFUSE bundle but deny exec of mount_macfuse. Without these preflights every
// e2e case waits for its mount timeout.
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
	const noFSKit = "FSKit is unavailable; it needs macOS 26 or later and macFUSE 5.3 or later"
	_, kextErr := unix.SysctlRaw("vfs.generic.macfuse.version.number")
	switch {
	case backend == fusefs.BackendKernel && kextErr != nil:
		t.Skip("skipping: ARTIFACT_FS_FUSE_BACKEND=kernel needs the macFUSE kernel extension loaded; mounting would prompt macOS to load it")
	case backend == fusefs.BackendFSKit && !fskitAvailable():
		t.Skip("skipping: " + noFSKit)
	case backend == fusefs.BackendAuto && !fskitAvailable() && kextErr != nil:
		t.Skip("skipping: " + noFSKit + ", and the macFUSE kernel extension is not loaded")
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

// fskitAvailable mirrors the fuse fork's FSKit preflight: the MFMount framework
// is installed and macOS is 26 or later. Extension approval is only known at mount time.
func fskitAvailable() bool {
	if _, err := os.Stat("/Library/Filesystems/macfuse.fs/Contents/Frameworks/MFMount.framework/Versions/A/MFMount"); err != nil {
		return false
	}
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return false
	}
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 26
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
