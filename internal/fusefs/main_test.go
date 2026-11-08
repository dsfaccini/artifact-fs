package fusefs

import (
	"context"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/dsfaccini/fuse"
)

func TestMain(m *testing.M) {
	gittest.IsolateConfig()
	// A test that mounts without replacing fuseMount panics here instead. On
	// macOS a real mount can prompt to load the macFUSE kernel extension.
	fuseMount = func(context.Context, string, fuse.Server, *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		panic("fusefs package tests never mount: replace fuseMount")
	}
	os.Exit(m.Run())
}

func TestGitIgnoresMachineConfig(t *testing.T) { gittest.RequireConfigIsolated(t) }
