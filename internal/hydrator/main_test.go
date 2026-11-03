package hydrator

import (
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gittest"
)

func TestMain(m *testing.M) {
	gittest.IsolateConfig()
	os.Exit(m.Run())
}

func TestGitIgnoresMachineConfig(t *testing.T) { gittest.RequireConfigIsolated(t) }
