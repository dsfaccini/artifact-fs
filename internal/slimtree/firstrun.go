package slimtree

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
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
	writeChecks(stderr, runDoctor(probe, goos, backend, ""))
	if err == nil {
		err = writeFirstRunMarker(home)
	}
	if err != nil {
		fmt.Fprintf(stderr, "slimtree: cannot record the first run, so the next run prints this summary again: %v\n", err)
	}
}

// markerCreated, when set, runs on the temporary marker right after its
// create. Tests use it to fail the write that follows.
var markerCreated func(*os.File)

// writeFirstRunMarker writes the marker to a temporary file and renames it
// into place, so a failed write, as on a full disk, leaves no empty marker
// that would hide the summary from the next run.
func writeFirstRunMarker(dir string) error {
	h, err := state.Open(dir)
	if err != nil {
		return err
	}
	home := h.Dir()
	f, err := os.CreateTemp(home, "."+firstRunMarker+"-*")
	if err != nil {
		return err
	}
	if markerCreated != nil {
		markerCreated(f)
	}
	_, err = fmt.Fprintf(f, "slimtree %s\n%s\n", version.String(), time.Now().UTC().Format(time.RFC3339))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(home, firstRunMarker))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
