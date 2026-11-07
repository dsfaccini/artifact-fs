package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/dsfaccini/fuse"
)

// wedgedMountRecovery is how macOS clears a stalled mount: nothing unmounts a
// stalled FSKit mount, including diskutil.
const wedgedMountRecovery = "only a reboot clears a stalled FSKit mount"

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
	const noFSKit = "FSKit is unavailable; it needs macOS 26 or later and macFUSE 5.5 or later"
	fskit := fskitAvailable()
	kext := fuse.MacFUSEKernelExtensionLoaded()
	switch {
	case backend == fusefs.BackendKernel && !kext:
		t.Skip("skipping: ARTIFACT_FS_FUSE_BACKEND=kernel needs the macFUSE kernel extension loaded; mounting would prompt macOS to load it")
	case backend == fusefs.BackendFSKit && !fskit:
		t.Skip("skipping: " + noFSKit)
	case backend == fusefs.BackendAuto && !fskit && !kext:
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

// mountsThroughFSKit reports whether e2e mounts here go through FSKit:
// ARTIFACT_FS_FUSE_BACKEND selects it, or the auto backend finds it available.
func mountsThroughFSKit() bool {
	backend, err := fusefs.MountBackend()
	return err == nil && (backend == fusefs.BackendFSKit || backend == fusefs.BackendAuto && fskitAvailable())
}

// fskitAvailable reports whether fuse.FSKitAvailable finds what an FSKit mount
// needs. Extension approval is only known at mount time.
func fskitAvailable() bool {
	return fuse.FSKitAvailable() == nil
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
