//go:build race

package daemon

import (
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
)

// The bootstrap installs the FSMonitor hook link, which runs this test
// binary.
func TestFSMonitorHookSkipsRaceExitSleep(t *testing.T) {
	f := newBootstrapFixture(t)
	if err := f.bootstrap(); err != nil {
		t.Fatal(err)
	}
	gittest.RequireHookSkipsRaceExitSleep(t, f.cfg.MountPath)
}
