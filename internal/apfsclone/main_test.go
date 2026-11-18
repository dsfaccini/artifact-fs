package apfsclone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/gittest"
)

// lockProbeEnv names the lock file that this test binary probes when a fake
// git or a hook runs it. lockProbeLogEnv names the file the probe appends its
// finding to.
const (
	lockProbeEnv    = "APFSCLONE_TEST_LOCK_PROBE"
	lockProbeLogEnv = "APFSCLONE_TEST_LOCK_PROBE_LOG"
)

func TestMain(m *testing.M) {
	if lock := os.Getenv(lockProbeEnv); lock != "" {
		os.Exit(probeLock(lock, os.Getenv(lockProbeLogEnv), os.Args[1]))
	}
	gittest.IsolateConfig()
	// The lock probe is this test binary, which a race build makes sleep
	// after it exits.
	gittest.DisableRaceExitSleep()
	os.Exit(m.Run())
}

// probeLock appends "<label> held", "<label> free" or "<label> missing" to
// log, as another process holds the flock on lock, holds none, or lock does
// not exist.
func probeLock(lock, log, label string) int {
	finding := "free"
	f, err := os.Open(lock)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		finding = "missing"
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 1
	default:
		defer unlockFile(f)
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); errors.Is(err, unix.EWOULDBLOCK) {
			finding = "held"
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	out, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer out.Close()
	if _, err := fmt.Fprintln(out, label, finding); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestGitIgnoresMachineConfig(t *testing.T) { gittest.RequireConfigIsolated(t) }
