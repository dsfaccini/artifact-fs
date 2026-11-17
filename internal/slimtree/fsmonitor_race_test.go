//go:build race

package slimtree

import (
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
)

func TestFSMonitorHookSkipsRaceExitSleep(t *testing.T) {
	link, _, _ := registerHookedRepo(t)
	gittest.RequireHookSkipsRaceExitSleep(t, hookedWorktree(t, link))
}
