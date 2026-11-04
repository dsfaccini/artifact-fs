//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
)

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
	unavailable := errors.New("FSKit unavailable")
	fskitUnavailable := func(err error) bool { return errors.Is(err, unavailable) }
	kernelFailure := errors.New("kernel mount failed")
	otherFailure := errors.New("FSKit transport failed")
	tests := []struct {
		name        string
		backend     string
		fskitErr    error
		kernelErr   error
		kextLoaded  bool
		wantMounts  []bool
		wantBackend string
		wantErrs    []error
	}{
		{name: "kernel without kext", backend: BackendKernel, wantMounts: []bool{false}, wantBackend: BackendKernel},
		{name: "kernel failure", backend: BackendKernel, kernelErr: kernelFailure, wantMounts: []bool{false}, wantBackend: BackendKernel, wantErrs: []error{kernelFailure}},
		{name: "fskit", backend: BackendFSKit, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "fskit unavailable never falls back", backend: BackendFSKit, fskitErr: unavailable, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{unavailable}},
		{name: "auto mounts FSKit", backend: BackendAuto, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit},
		{name: "auto other failure", backend: BackendAuto, fskitErr: otherFailure, kextLoaded: true, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{otherFailure}},
		{name: "auto unavailable with kext", backend: BackendAuto, fskitErr: unavailable, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel},
		{name: "auto unavailable without kext", backend: BackendAuto, fskitErr: unavailable, wantMounts: []bool{true}, wantBackend: BackendFSKit, wantErrs: []error{unavailable}},
		{name: "auto fallback failure", backend: BackendAuto, fskitErr: unavailable, kernelErr: kernelFailure, kextLoaded: true, wantMounts: []bool{true, false}, wantBackend: BackendKernel, wantErrs: []error{unavailable, kernelFailure}},
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
			_, backend, err := mountWithBackend(test.backend, mount, fskitUnavailable, func() bool { return test.kextLoaded })
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

// assertMountResult checks the mount attempts (true is FSKit), the backend, and
// the error that a mountWithBackend or platformMount call produced.
func assertMountResult(t *testing.T, mounts []bool, backend string, err error, wantMounts []bool, wantBackend string, wantErr error) {
	t.Helper()
	if !slices.Equal(mounts, wantMounts) {
		t.Fatalf("mount attempts (true is FSKit) = %v, want %v", mounts, wantMounts)
	}
	if backend != wantBackend {
		t.Fatalf("backend = %q, want %q", backend, wantBackend)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
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
