//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
)

func TestReadDirRefreshesReusedFSKitHandle(t *testing.T) {
	for _, test := range []struct {
		name  string
		fskit bool
	}{
		{name: "ordinary handle"},
		{name: "FSKit reused handle", fskit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLocalOverlayFuseFixture(t)
			f.fs.fskit = test.fskit
			ctx := context.Background()
			open := &fuseops.OpenDirOp{Inode: f.nestInode}
			if err := f.fs.OpenDir(ctx, open); err != nil {
				t.Fatal(err)
			}
			read := func(offset fuseops.DirOffset) []byte {
				t.Helper()
				op := &fuseops.ReadDirOp{Handle: open.Handle, Offset: offset, Dst: make([]byte, 4096)}
				if err := f.fs.ReadDir(ctx, op); err != nil {
					t.Fatal(err)
				}
				return op.Dst[:op.BytesRead]
			}
			if !bytes.Contains(read(0), []byte("value.txt\x00")) {
				t.Fatal("initial enumeration omitted the existing file")
			}
			if _, err := f.store.CreateFile(ctx, "src/nested/added.txt", 0o644); err != nil {
				t.Fatal(err)
			}
			if got := read(1); len(got) != 0 {
				t.Fatal("a continued enumeration changed its snapshot")
			}
			if got := bytes.Contains(read(0), []byte("added.txt\x00")); got != test.fskit {
				t.Fatalf("rewound enumeration includes new file=%v, want %v", got, test.fskit)
			}
			if err := f.store.Remove(ctx, "src/nested/value.txt"); err != nil {
				t.Fatal(err)
			}
			if got := bytes.Contains(read(0), []byte("value.txt\x00")); got == test.fskit {
				t.Fatalf("rewound enumeration includes deleted file=%v, want %v", got, !test.fskit)
			}
			if err := f.fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: open.Handle}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMountBackendRejectsUnknownValues(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "", want: BackendAuto},
		{value: "auto", want: BackendAuto},
		{value: "fskit", want: BackendFSKit},
		{value: "kernel", want: BackendKernel},
		{value: "FSKit"},
		{value: " fskit"},
		{value: "kext"},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(backendEnv, test.value)
			got, err := MountBackend()
			if test.want == "" {
				if err == nil || !strings.Contains(err.Error(), "unsupported") {
					t.Fatalf("MountBackend() = %q, %v; want an unsupported value error", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("MountBackend() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestMountWithBackendFallsBackOnlyWhenFSKitIsUnavailable(t *testing.T) {
	unavailable := fmt.Errorf("mount: %w: dlopen failed", fuse.ErrFSKitUnavailable)
	result := func(code int32) error {
		return fmt.Errorf("mount (background): %w", fuse.FSKitMountError{Result: code})
	}
	kernelFailure := errors.New("kernel mount failed")
	otherFailure := errors.New("FSKit transport failed")
	unavailableCases := map[string]error{
		"framework unavailable":       unavailable,
		"unsupported OS version":      result(fuse.FSKitMountResultUnsupportedOSVersion),
		"helper tools install failed": result(fuse.FSKitMountResultHelperToolsInstallationFailed),
		"extension not found":         result(fuse.FSKitMountResultFileSystemExtensionNotFound),
		"extension requires approval": result(fuse.FSKitMountResultFileSystemExtensionRequiresApproval),
	}
	type testCase struct {
		name        string
		backend     string
		fskitErr    error
		kernelErr   error
		kextLoaded  bool
		wantMounts  []bool
		wantBackend string
		wantErrs    []error
	}
	tests := []testCase{
		{name: "kernel without kext", backend: BackendKernel, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{name: "kernel failure", backend: BackendKernel, kernelErr: kernelFailure, wantMounts: []bool{false}, wantBackend: BackendKernel, wantErrs: []error{kernelFailure}},
		{name: "fskit", backend: BackendFSKit, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "fskit unavailable never falls back", backend: BackendFSKit, fskitErr: unavailable, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{fuse.ErrFSKitUnavailable}},
		{name: "auto mounts FSKit", backend: BackendAuto, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "auto mount point failure", backend: BackendAuto, fskitErr: result(fuse.FSKitMountResultCreatingMountPointFailed), kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{fuse.FSKitMountError{Result: fuse.FSKitMountResultCreatingMountPointFailed}}},
		{name: "auto unexpected failure", backend: BackendAuto, fskitErr: result(fuse.FSKitMountResultUnexpectedFailure), kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{fuse.FSKitMountError{Result: fuse.FSKitMountResultUnexpectedFailure}}},
		{name: "auto other failure", backend: BackendAuto, fskitErr: otherFailure, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{otherFailure}},
		{name: "auto fallback failure", backend: BackendAuto, fskitErr: unavailable, kernelErr: kernelFailure, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel, wantErrs: []error{fuse.ErrFSKitUnavailable, kernelFailure}},
	}
	for name, fskitErr := range unavailableCases {
		tests = append(tests,
			testCase{name: "auto " + name + " with kext", backend: BackendAuto, fskitErr: fskitErr, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel},
			testCase{name: "auto " + name + " without kext", backend: BackendAuto, fskitErr: fskitErr, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{fskitErr}},
		)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mounts []bool
			mount := func(fskit bool) (*fuse.MountedFileSystem, error) {
				mounts = append(mounts, fskit)
				if fskit {
					return nil, test.fskitErr
				}
				return nil, test.kernelErr
			}
			_, backend, err := mountWithBackend(test.backend, mount, func() bool { return test.kextLoaded })
			if !slices.Equal(mounts, test.wantMounts) {
				t.Fatalf("mount attempts (true is FSKit) = %v, want %v", mounts, test.wantMounts)
			}
			if backend != test.wantBackend {
				t.Fatalf("backend = %q, want %q", backend, test.wantBackend)
			}
			if len(test.wantErrs) == 0 && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if len(test.wantErrs) > 0 && err == nil {
				t.Fatal("error = nil, want a mount error")
			}
			for _, want := range test.wantErrs {
				if !errors.Is(err, want) {
					t.Fatalf("error = %v, want it to wrap %v", err, want)
				}
			}
		})
	}
}

func TestFSKitRejectsXattrFallback(t *testing.T) {
	for _, test := range []struct {
		name  string
		fskit bool
		want  error
	}{
		{name: "ordinary backend", want: syscall.ENOSYS},
		{name: "FSKit", fskit: true, want: syscall.EPERM},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := &ArtifactFuse{fskit: test.fskit}
			if err := fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Name: "com.apple.FinderInfo"}); err != test.want {
				t.Fatalf("SetXattr error=%v, want %v", err, test.want)
			}
		})
	}
}
