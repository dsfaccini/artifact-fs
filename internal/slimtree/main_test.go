package slimtree

import (
	"context"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
)

func TestMain(m *testing.M) {
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
