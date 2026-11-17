package cli

import (
	"fmt"
	"runtime"
	"testing"
)

// A test binary carries no release stamp and no VCS stamp, so it reports the
// dev build's fallback.
func TestVersionFlagPrintsTheBuildVersion(t *testing.T) {
	stdout, stderr, code := runCLI(t, "--version")
	if code != 0 {
		t.Fatalf("--version exit = %d, stderr=%q", code, stderr)
	}
	want := fmt.Sprintf("artifact-fs dev (untagged, unknown, %s, %s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if stdout != want {
		t.Fatalf("--version = %q, want %q", stdout, want)
	}
}
