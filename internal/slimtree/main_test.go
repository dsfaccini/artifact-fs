package slimtree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
)

// lockProbeEnv names a lock file. A post-checkout hook that runs this test
// binary with it set prints whether another process holds that lock.
const lockProbeEnv = "SLIMTREE_TEST_LOCK_PROBE"

func TestMain(m *testing.M) {
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
	os.Exit(m.Run())
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
	defer f.Close()
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
