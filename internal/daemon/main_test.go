package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// TestMain makes a real mount panic: unit tests must never mount, because e2e tests own all mounts.
func TestMain(m *testing.M) {
	defaultMount = func(cfg model.RepoConfig, _ *fusefs.Resolver, _ *fusefs.Engine, _ *fusefs.ReadyGate) (fusefs.MountedFS, error) {
		panic(fmt.Sprintf("daemon unit test tried to mount %q; set Service.mount to fakeMount", cfg.MountPath))
	}
	m.Run()
}

// fakeMount stands in for a successful mount.
func fakeMount(model.RepoConfig, *fusefs.Resolver, *fusefs.Engine, *fusefs.ReadyGate) (fusefs.MountedFS, error) {
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
