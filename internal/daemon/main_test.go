package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// TestMain makes a real mount panic: unit tests must never mount, because e2e tests own all mounts.
func TestMain(m *testing.M) {
	// The FSMonitor hook that the daemon installs runs this binary, which
	// would otherwise run the tests inside Git. FSMonitorHook answers "/" for
	// a worktree that is not mounted, and package tests never mount.
	if len(os.Args) > 1 && os.Args[1] == "fsmonitor-hook" {
		fmt.Print("artifact-fs:package-test\x00/\x00")
		os.Exit(0)
	}
	defaultMount = func(cfg model.RepoConfig, _ *fusefs.Engine, _ *fusefs.ReadyGate, _ *slog.Logger) (fusefs.MountedFS, error) {
		panic(fmt.Sprintf("daemon unit test tried to mount %q; set Service.mount to fakeMount", cfg.MountPath))
	}
	gittest.IsolateConfig()
	m.Run()
}

func TestGitIgnoresMachineConfig(t *testing.T) { gittest.RequireConfigIsolated(t) }

// ctl is the controller a CLI command on s's root would run on. It shares s's
// registry connection.
func (s *Service) ctl() *Controller { return &Controller{core: s.core} }

// fakeMount stands in for a successful mount.
func fakeMount(model.RepoConfig, *fusefs.Engine, *fusefs.ReadyGate, *slog.Logger) (fusefs.MountedFS, error) {
	return &fakeMountedFS{unmounted: make(chan struct{})}, nil
}

// fakeMountedFS behaves like a live mount: Join returns once it is unmounted.
type fakeMountedFS struct {
	unmountOnce sync.Once
	unmounted   chan struct{}
}

func (m *fakeMountedFS) Backend() string { return "fake" }

func (m *fakeMountedFS) Join(ctx context.Context) error {
	select {
	case <-m.unmounted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *fakeMountedFS) Unmount() error {
	m.unmountOnce.Do(func() { close(m.unmounted) })
	return nil
}
