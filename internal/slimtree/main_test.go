package slimtree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// lockProbeEnv names a lock file. A post-checkout hook that runs this test
// binary with it set prints whether another process holds that lock.
const lockProbeEnv = "SLIMTREE_TEST_LOCK_PROBE"

// notADaemon is what this test binary says when it runs as the daemon.
// slimtree starts the daemon from a copy of its own binary, which in a
// package test is this binary.
const notADaemon = "the slimtree test binary is not a daemon"

func TestMain(m *testing.M) {
	// Running the tests again as the daemon would launch the next copy.
	if len(os.Args) > 1 && (os.Args[1] == "daemon" || os.Args[1] == "afs") {
		fmt.Fprintln(os.Stderr, notADaemon)
		os.Exit(2)
	}
	if lock := os.Getenv(lockProbeEnv); lock != "" {
		os.Exit(probeLock(lock))
	}
	// Git runs this binary as the FSMonitor hook, through the hook link or
	// the hook script, and Main answers as the slimtree binary does.
	if _, _, ok := gitstore.ParseFSMonitorHookLink(os.Args[0]); ok || len(os.Args) > 1 && os.Args[1] == "fsmonitor-hook" {
		os.Exit(Main(context.Background(), os.Args, os.Stdout, os.Stderr))
	}
	gittest.IsolateConfig()
	gittest.DisableRaceExitSleep()
	// No package test launches the real daemon program; fakeDaemon gives a
	// test one.
	daemonProgram = func(*state.Home, string) (string, error) {
		return "", errors.New("a package test reached the daemon's launch without fakeDaemon")
	}
	os.Exit(m.Run())
}

// daemonProbeEnv marks a run of this test binary as the daemon. A run that
// reaches TestTestBinaryIsNotADaemon is one whose TestMain ran the tests.
const daemonProbeEnv = "SLIMTREE_TEST_DAEMON_PROBE"

// This test binary refuses to run as the daemon, at once. A package test
// that reaches the daemon's launch then fails, instead of running the tests
// again in the launched copy, which would launch the next one.
func TestTestBinaryIsNotADaemon(t *testing.T) {
	if os.Getenv(daemonProbeEnv) != "" {
		t.Fatal("this test binary ran its tests as the daemon")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"daemon", "afs"} {
		cmd := exec.CommandContext(t.Context(), exe, arg)
		cmd.Env = append(os.Environ(), daemonProbeEnv+"=1")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || string(out) != notADaemon+"\n" {
			t.Errorf("the test binary as %s: %v, output %q; want exit 2 and %q", arg, err, out, notADaemon)
		}
	}
}

func TestGitIgnoresMachineConfig(t *testing.T) { gittest.RequireConfigIsolated(t) }

// probeLock prints "lock held" when another process holds the flock on lock,
// and "lock free" otherwise.
func probeLock(lock string) int {
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}()
	switch err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); {
	case errors.Is(err, unix.EWOULDBLOCK):
		fmt.Println("lock held")
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 1
	default:
		fmt.Println("lock free")
	}
	return 0
}
