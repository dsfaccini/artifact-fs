package fusefs

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
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

// FSKit takes no invalidations, so a mount through it sends none.
func TestInvalidateChangedSendsNothingOnFSKit(t *testing.T) {
	f := newCacheFixtureOn(t, BackendFSKit, baseFile("file.txt", "old", 0o100644))
	f.lookup(fuseops.RootInodeID, "file.txt")
	f.switchTo()
	if err := f.mount.InvalidateChanged(); err != nil {
		t.Fatal(err)
	}
	if inodes, entries := f.notifier.recorded(); len(inodes) != 0 || len(entries) != 0 {
		t.Fatalf("FSKit mount sent invalidations: inodes %v, entries %v", inodes, entries)
	}
}

// A failed FSKit attempt's Server can keep serving after fuse.Mount returns.
// So each attempt gets its own Notifier, and the mount invalidates through
// the Notifier of the attempt that mounted.
func TestMountRepoInvalidatesThroughTheMountedAttemptsNotifier(t *testing.T) {
	setMacFUSEKernelExtensionLoaded(t, true)
	t.Setenv(backendEnv, BackendAuto)
	var created []*fuse.Notifier
	savedNotifier := newNotifier
	newNotifier = func() *fuse.Notifier {
		n := fuse.NewNotifier()
		created = append(created, n)
		return n
	}
	t.Cleanup(func() { newNotifier = savedNotifier })
	savedMount := fuseMount
	fuseMount = func(_ context.Context, _ string, _ fuse.Server, cfg *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		if cfg.Backend == fuse.BackendFSKit {
			return nil, fuse.ErrFSKitUnavailable
		}
		return &fuse.MountedFileSystem{}, nil
	}
	t.Cleanup(func() { fuseMount = savedMount })

	m, err := MountRepoWithGate(context.Background(), context.Background(), model.RepoConfig{Name: "repo", MountPath: t.TempDir()}, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if m.Backend() != BackendKernel {
		t.Fatalf("mounted through %q, want the kernel extension", m.Backend())
	}
	if len(created) != 2 || created[0] == created[1] {
		t.Fatalf("attempts got %d Notifiers, want one each for FSKit and the kernel extension", len(created))
	}
	if fs := m.(*mountedFSWrapper).fs; fs.notifier != created[1] {
		t.Fatal("the mount invalidates through a Notifier other than the kernel extension attempt's")
	}
}
