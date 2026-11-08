package fusefs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
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
	repo := model.RepoPaths{Name: "repo", MountPath: t.TempDir()}
	attempts := mountRepoAttempts(t, repo, slog.New(slog.DiscardHandler), unredacted)
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

// FSKit takes no invalidations, so a mount through it sends none. Its switch
// pass still retires the number of a path the switch removed, and retires a
// held inode whose path a later switch brings back changed.
func TestInvalidateChangedSendsNothingOnFSKit(t *testing.T) {
	f := newCacheFixtureOn(t, BackendFSKit, baseFile("file.txt", "old", 0o100644), baseFile("held.txt", "blob-A", 0o100644))
	forgotten := f.lookup(fuseops.RootInodeID, "file.txt")
	held := f.lookup(fuseops.RootInodeID, "held.txt")
	forget(t, f.fs, forgotten)
	pass := func(nodes ...model.BaseNode) {
		t.Helper()
		f.switchTo(nodes...)
		if err := f.mount.InvalidateChanged(); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	pass(baseFile("file.txt", "old", 0o100644), baseFile("held.txt", "blob-B", 0o100644))
	if inodes, entries := f.notifier.recorded(); len(inodes) != 0 || len(entries) != 0 {
		t.Fatalf("FSKit mount sent invalidations: inodes %v, entries %v", inodes, entries)
	}
	if got := openErrno(f, held); got != syscall.ESTALE {
		t.Fatalf("open of inode %d, whose path came back with another blob, = errno %v, want ESTALE", held, got)
	}
	if got := f.lookup(fuseops.RootInodeID, "file.txt"); got == forgotten {
		t.Fatalf("file.txt came back with inode %d, which it had before a switch removed it", got)
	}
}

// The FSKit switch pass compares no inode, so it checks the inodes that open
// file handles are on for their read generation alone.
func TestOpenFileGenerationAfterSwitchesOnFSKit(t *testing.T) {
	testOpenFileGenerationAfterSwitches(t, BackendFSKit)
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

	m, err := MountRepoWithGate(context.Background(), context.Background(), model.RepoPaths{Name: "repo", MountPath: t.TempDir()}, nil, nil, slog.New(slog.DiscardHandler), unredacted)
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

// An FSKit attempt's server checks permissions itself, and a kernel
// extension attempt's server does not, even when both attempts run.
func TestMountRepoChecksPermissionsOnlyThroughAnFSKitAttempt(t *testing.T) {
	setMacFUSEKernelExtensionLoaded(t, true)
	t.Setenv(backendEnv, BackendAuto)
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := model.RepoPaths{ID: "repo", Name: "repo", MountPath: t.TempDir(), OverlayDir: filepath.Join(tmp, "overlay"), OverlayDBPath: filepath.Join(tmp, "overlay.db")}
	ov, err := overlaystore.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	if _, err := ov.CreateFile(ctx, "ro.txt", 0o444); err != nil {
		t.Fatal(err)
	}
	if err := ov.FlushFile(ctx, "ro.txt"); err != nil {
		t.Fatal(err)
	}
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{}}, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov}

	// Each attempt builds its server, then mounts it.
	var served []fuseutil.FileSystem
	var throughFSKit []bool
	savedServer := newFileSystemServer
	newFileSystemServer = func(fs fuseutil.FileSystem) fuse.Server {
		served = append(served, fs)
		return savedServer(fs)
	}
	t.Cleanup(func() { newFileSystemServer = savedServer })
	savedMount := fuseMount
	fuseMount = func(_ context.Context, _ string, _ fuse.Server, cfg *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		throughFSKit = append(throughFSKit, cfg.Backend == fuse.BackendFSKit)
		if cfg.Backend == fuse.BackendFSKit {
			return nil, fuse.ErrFSKitUnavailable
		}
		return &fuse.MountedFileSystem{}, nil
	}
	t.Cleanup(func() { fuseMount = savedMount })
	if _, err := MountRepoWithGate(ctx, ctx, cfg, engine, nil, slog.New(slog.DiscardHandler), unredacted); err != nil {
		t.Fatal(err)
	}
	if len(served) != 2 || len(throughFSKit) != 2 || !throughFSKit[0] || throughFSKit[1] {
		t.Fatalf("attempts through FSKit %v served %d file systems, want an FSKit attempt, then a kernel extension attempt", throughFSKit, len(served))
	}
	for i, fs := range served {
		fskit := throughFSKit[i]
		lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "ro.txt"}
		if err := fs.LookUpInode(ctx, lookup); err != nil {
			t.Fatal(err)
		}
		got := kernelErrno(fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: lookup.Entry.Child, OpenFlags: syscall.O_WRONLY}))
		if want := map[bool]syscall.Errno{true: syscall.EACCES, false: 0}[fskit]; got != want {
			t.Errorf("FSKit attempt %t: write open of a 0444 file = %v, want %v", fskit, got, want)
		}
	}
}
