package fusefs

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse"
)

func setMacFUSEKernelExtensionLoaded(t *testing.T, loaded bool) {
	t.Helper()
	saved := macFUSEKernelExtensionLoaded
	macFUSEKernelExtensionLoaded = func() bool { return loaded }
	t.Cleanup(func() { macFUSEKernelExtensionLoaded = saved })
}

// The auto backend selects FSKit through MountConfig.Backend, then falls back
// to the kernel extension with a new Server: fuse.Mount forbids reusing one
// after a mount that failed past INIT.
func TestMountRepoFallsBackWithANewServer(t *testing.T) {
	setMacFUSEKernelExtensionLoaded(t, true)
	repo := model.RepoConfig{Name: "repo", MountPath: t.TempDir()}
	attempts := mountRepoAttempts(t, repo, slog.New(slog.DiscardHandler))
	if len(attempts) != 2 {
		t.Fatalf("got %d mount attempts, want FSKit, then the kernel extension", len(attempts))
	}
	fskit, kernel := attempts[0], attempts[1]
	if fskit.cfg.Backend != fuse.BackendFSKit || kernel.cfg.Backend == fuse.BackendFSKit {
		t.Fatalf("backends = %q, %q; want %q, then the kernel extension", fskit.cfg.Backend, kernel.cfg.Backend, fuse.BackendFSKit)
	}
	if _, ok := fskit.cfg.Options["backend"]; ok {
		t.Fatalf("FSKit attempt sets the deprecated backend option: %v", fskit.cfg.Options)
	}
	if fskit.server == kernel.server {
		t.Fatal("the kernel fallback reuses the FSKit attempt's Server")
	}
}

func TestPlatformMountReachesFSKit(t *testing.T) {
	unavailable := fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable)
	for _, test := range []struct {
		name        string
		backend     string
		fskitErr    error
		kextLoaded  bool
		wantMounts  []bool
		wantBackend string
		wantErr     error
	}{
		{name: "fskit", backend: BackendFSKit, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "auto", backend: BackendAuto, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "kernel", backend: BackendKernel, kextLoaded: true, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{name: "auto falls back to a loaded kext", backend: BackendAuto, fskitErr: unavailable, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel},
		{name: "auto keeps an unloaded kext unloaded", backend: BackendAuto, fskitErr: unavailable, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErr: fuse.ErrFSKitUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			setMacFUSEKernelExtensionLoaded(t, test.kextLoaded)
			var mounts []bool
			_, backend, err := platformMount(test.backend, func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				if fskit {
					return nil, test.fskitErr
				}
				return nil, nil
			})
			assertMountResult(t, mounts, backend, err, test.wantMounts, test.wantBackend, test.wantErr)
		})
	}
}
