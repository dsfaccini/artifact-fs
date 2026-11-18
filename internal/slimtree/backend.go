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
		return "how to make worktrees: apfsclone (artifactfs worktrees are not supported yet; Linux support is coming)"
	}
	return "how to make worktrees: artifactfs, whose worktrees are not supported yet; Linux support is coming (apfsclone needs macOS)"
}

// notInThisRelease refuses cmd on a worktree of backend b, which this release
// cannot make or remove.
func notInThisRelease(cmd, b string) error {
	return fmt.Errorf("%s: %s worktrees are not supported yet, and Linux support is coming; slimtree makes and removes only apfsclone worktrees, on macOS", cmd, b)
}

// runDoctor runs doctor's checks for backend. An artifactfs report also
// fails a check that says this release makes no artifactfs worktrees, so
// doctor never reports ready for worktrees new refuses.
func runDoctor(probe doctor.Probe, goos, backend, dir string) doctor.Report {
	r := doctor.Run(probe, goos, backend, dir)
	if backend == string(artifactFS) {
		fix := "upgrade slimtree once a release makes artifactfs worktrees"
		if goos == "darwin" {
			fix = "use --backend apfsclone, the default on macOS"
		}
		release := doctor.Result{ID: "backend", Status: doctor.StatusFail, Detail: "artifactfs worktrees are not supported yet; Linux support is coming", Fix: fix}
		r.Checks = append([]doctor.Result{release}, r.Checks...)
		r.OK = false
	}
	return r
}
