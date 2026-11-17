package slimtree

import "fmt"

// backend names how slimtree makes a worktree.
type backend string

const (
	// apfsClone clones the base worktree's files with APFS clonefile.
	apfsClone backend = "apfsclone"
	// artifactFS serves the worktree from an artifact-fs FUSE mount.
	artifactFS backend = "artifactfs"
)

// defaultBackend is apfsclone on macOS and artifactfs elsewhere.
func defaultBackend(goos string) backend {
	if goos == "darwin" {
		return apfsClone
	}
	return artifactFS
}

// checkBackend refuses a backend that slimtree cannot run on goos.
func checkBackend(goos string, b backend) error {
	switch b {
	case apfsClone:
		if goos != "darwin" {
			return fmt.Errorf("apfsclone needs macOS, which clones files on APFS; use artifactfs on %s", goos)
		}
	case artifactFS:
	default:
		return fmt.Errorf("unknown backend %q: use apfsclone or artifactfs", b)
	}
	return nil
}

func backendUsage(goos string) string {
	if goos == "darwin" {
		return "how to make worktrees: apfsclone, or artifactfs (beta on macOS)"
	}
	return "how to make worktrees: artifactfs (apfsclone needs macOS)"
}

// notInThisRelease refuses cmd on a worktree of backend b, which this release
// cannot make or remove.
func notInThisRelease(cmd, b string) error {
	return fmt.Errorf("%s: %s worktrees are not in this release yet; this release makes and removes only apfsclone worktrees, on macOS", cmd, b)
}
