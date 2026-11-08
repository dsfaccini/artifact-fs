package gitstore

import (
	"fmt"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
)

func TestMain(m *testing.M) {
	gittest.IsolateConfig()
	// Git runs this binary as SSH, and a race build sleeps a second after it
	// exits 0.
	gittest.DisableRaceExitSleep()
	// A Store that a test never closes leaves its SSH wrapper's directory in
	// the temp dir, so the tests share one that goes when they end.
	tmp, err := os.MkdirTemp("", "gitstore-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", tmp)
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

func TestGitIgnoresMachineConfig(t *testing.T) { gittest.RequireConfigIsolated(t) }
