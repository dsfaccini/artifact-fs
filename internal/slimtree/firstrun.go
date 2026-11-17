package slimtree

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/version"
)

// firstRunMarker is the file in slimtree's home whose presence means the
// first-run summary has been shown.
const firstRunMarker = "first-run"

// firstRun prints the doctor summary to stderr when slimtree's home holds no
// first-run marker, then writes the marker. The summary probes no directory,
// so it drops no probe file into the current directory, which is often a work
// tree. A home that firstRun cannot read or write never fails the command:
// firstRun says so, and the next run prints the summary again.
func firstRun(stderr io.Writer, probe doctor.Probe, goos, backend string) {
	home, err := Home()
	if err == nil {
		if _, statErr := os.Stat(filepath.Join(home, firstRunMarker)); statErr == nil {
			return
		}
	}
	fmt.Fprintf(stderr, "slimtree's first run: what --backend %s needs here (slimtree doctor checks again)\n", backend)
	writeChecks(stderr, doctor.Run(probe, goos, backend, ""))
	if err == nil {
		err = writeFirstRunMarker(home)
	}
	if err != nil {
		fmt.Fprintf(stderr, "slimtree: cannot record the first run, so the next run prints this summary again: %v\n", err)
	}
}

func writeFirstRunMarker(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	marker := fmt.Appendf(nil, "slimtree %s\n%s\n", version.String(), time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(filepath.Join(home, firstRunMarker), marker, 0o600)
}
