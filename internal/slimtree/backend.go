package slimtree

import (
	"fmt"

	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
)

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

// notInThisRelease refuses cmd, which removes a worktree of backend b, which
// this release cannot remove.
func notInThisRelease(cmd, b string) error {
	return fmt.Errorf("%s: removing %s worktrees is not supported yet; slimtree stop unmounts one", cmd, b)
}

// runDoctor runs doctor's checks for backend. An artifactfs report also
// warns that this release cannot remove artifactfs worktrees.
func runDoctor(probe doctor.Probe, goos, backend, dir string) doctor.Report {
	r := doctor.Run(probe, goos, backend, dir)
	if backend == string(artifactFS) {
		release := doctor.Result{ID: "backend", Status: doctor.StatusWarn, Detail: "slimtree rm of artifactfs worktrees is not supported yet", Fix: "upgrade slimtree once a release removes artifactfs worktrees"}
		r.Checks = append([]doctor.Result{release}, r.Checks...)
	}
	return r
}
